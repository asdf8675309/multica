package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
	"github.com/multica-ai/multica/server/internal/daemon/processtree"
	"github.com/multica-ai/multica/server/internal/daemon/repocache"
)

// Salvage saves work that only exists on this machine before a GC pass deletes
// the Git checkout or the branch that holds it.
//
// The GC used to delete a done card's whole task directory and every unused
// agent branch without asking whether an agent left uncommitted files or
// commits that no remote has. Salvage asks. A checkout is SAFE when
// `git status` shows nothing to save and HEAD is contained in a remote-tracking
// ref of origin. A SAFE checkout is removed as before. An UNSAFE one is written
// to a Git bundle plus a JSON manifest under <workspaces root>/.salvage, and
// only then removed. Any failure keeps the directory: the GC fails closed.

const (
	// salvageDirName is a dot directory, so runGC and ScanDiskUsage already
	// refuse to treat it as a workspace. It is a sibling of the workspace and
	// .repos directories. See runGC for the scan rule that protects it.
	salvageDirName = ".salvage"

	// salvageTmpPrefix marks per-operation scratch directories. Salvage runs
	// only inside runGC, which is serial, so every scratch directory found at
	// the start of a cycle belongs to an operation that died.
	salvageTmpPrefix = ".tmp-"

	// Every bundle and manifest is named <stamp>_<parts>-<nonce>.bundle|.json.
	// sweepSalvage matches this shape, so changing it here changes what the
	// sweep may delete.
	salvageStampLayout = "20060102T150405Z"
	salvageNonceBytes  = 6

	// salvageGitTimeout bounds each Git step of a salvage. It is longer than
	// gitCmdTimeout because hashing a large untracked tree and packing a bundle
	// legitimately take minutes; the other GC Git calls keep the short limit.
	salvageGitTimeout = 5 * time.Minute

	salvageRefPrefix         = "refs/multica-salvage/"
	salvageStateFile         = ".gc_salvage_state.json"
	salvageWarnInterval      = 24 * time.Hour
	salvageMaxIgnoredEntries = 200
	salvageMaxDirtyEntries   = 5000
	salvageOriginRemote      = "origin"

	salvageManifestVersion = 1
	salvageKindWorktree    = "task_worktree"
	salvageKindBranch      = "agent_branch"

	// The identity signs snapshot commits. Without one Git falls back to the
	// user's global config, and fails outright on a machine that has none.
	salvageIdentityName  = "Multica GC Salvage"
	salvageIdentityEmail = "gc-salvage@multica.invalid"
)

var (
	errSalvageTooLarge = errors.New("salvage payload exceeds MULTICA_GC_SALVAGE_MAX_MB")
	errSalvageFull     = errors.New("salvage directory is at MULTICA_GC_SALVAGE_TOTAL_MAX_MB")
	errSalvageGitLink  = errors.New(".git is a symlink, not inspected")

	nonceHex           = fmt.Sprintf("[0-9a-f]{%d}", 2*salvageNonceBytes)
	salvageScratchName = regexp.MustCompile(`^` + regexp.QuoteMeta(salvageTmpPrefix) + nonceHex + `$`)
	// Groups: 1 the base a bundle and its manifest share, 2 the extension,
	// 3 the ".tmp" of a half-written file.
	salvageFileName = regexp.MustCompile(`^([0-9]{8}T[0-9]{6}Z_[A-Za-z0-9._-]+-` + nonceHex + `)\.(bundle|json)(\.tmp)?$`)
)

// ---- Git runner -----------------------------------------------------------

// salvageBaseEnv is the environment of every salvage Git call.
//
// The GIT_* variables removed here each redirect Git to a different repository
// or index than the -C flag names. A daemon started from a shell that exported
// one of them would make salvage bundle the wrong repository and then report
// success, so they are dropped rather than trusted.
//
// LC_ALL=C because requiredCommitsFromVerify parses the English text of `git
// bundle verify`. A translated Git would word the heading differently and
// every bundle would read as having no prerequisites.
func salvageBaseEnv() []string {
	scrubbed := map[string]struct{}{
		"GIT_DIR": {}, "GIT_WORK_TREE": {}, "GIT_INDEX_FILE": {}, "GIT_COMMON_DIR": {},
		"GIT_OBJECT_DIRECTORY": {}, "GIT_ALTERNATE_OBJECT_DIRECTORIES": {},
	}
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if _, drop := scrubbed[name]; drop {
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME="+salvageIdentityName,
		"GIT_AUTHOR_EMAIL="+salvageIdentityEmail,
		"GIT_COMMITTER_NAME="+salvageIdentityName,
		"GIT_COMMITTER_EMAIL="+salvageIdentityEmail,
	)
}

// salvageGitArgs prefixes every salvage call with the settings that stop a
// repository from running code or a daemon during inspection. The repository
// content belongs to an agent, so its config and hooks are not trusted:
// core.fsmonitor=false stops `git status` from starting a filesystem monitor
// that would outlive the call and hold files open inside a directory about to
// be deleted, core.hooksPath points at nothing so no hook runs, and
// commit.gpgsign=false keeps a signing prompt from blocking the snapshot commit.
func salvageGitArgs(dir string, args ...string) []string {
	return append([]string{
		"-c", "core.fsmonitor=false",
		"-c", "core.hooksPath=" + os.DevNull,
		"-c", "safe.directory=*",
		"-c", "commit.gpgsign=false",
		"-C", dir,
	}, args...)
}

func newSalvageGitCommand(dir string, extraEnv []string, stdin string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", salvageGitArgs(dir, args...)...)
	cmd.Env = append(salvageBaseEnv(), extraEnv...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	return cmd
}

// runSalvageGit returns stdout only. Callers that parse `-z` output must not
// see stderr mixed in.
func runSalvageGit(ctx context.Context, dir string, extraEnv []string, stdin string, args ...string) (string, error) {
	stepCtx, cancel := context.WithTimeout(ctx, salvageGitTimeout)
	defer cancel()
	out, err := processtree.Output(stepCtx, newSalvageGitCommand(dir, extraEnv, stdin, args...), 5*time.Second)
	if err != nil {
		return string(out), salvageGitError(stepCtx, args, err)
	}
	return string(out), nil
}

// runSalvageGitCombined returns stdout and stderr together, for the commands
// whose useful answer Git prints on stderr (`bundle verify`).
func runSalvageGitCombined(ctx context.Context, dir string, extraEnv []string, args ...string) (string, error) {
	stepCtx, cancel := context.WithTimeout(ctx, salvageGitTimeout)
	defer cancel()
	out, err := processtree.CombinedOutput(stepCtx, newSalvageGitCommand(dir, extraEnv, "", args...), 5*time.Second)
	if err != nil {
		return string(out), salvageGitError(stepCtx, args, err)
	}
	return string(out), nil
}

func salvageGitError(stepCtx context.Context, args []string, err error) error {
	name := "git"
	if len(args) > 0 {
		name = "git " + args[0]
	}
	if errors.Is(stepCtx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%s timed out after %s: %w", name, salvageGitTimeout, err)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(exitErr.Stderr)))
	}
	return fmt.Errorf("%s: %w", name, err)
}

func isGitExitCode(err error, code int) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == code
}

// ---- finding and inspecting checkouts --------------------------------------

// findGitRoots returns every directory under workdir that holds a `.git` entry,
// the workdir itself included. A `.git` FILE marks a linked worktree or a
// submodule, so both kinds are found. Nested repositories are found too: Git
// stores a nested repository as an untracked directory in its parent, so the
// parent's bundle does not contain its commits and each one needs its own.
//
// node_modules is not searched. Package managers put Git checkouts there, all
// of them ignored by the parent and reproducible from the lockfile.
//
// A walk error is returned, not skipped: a directory that could not be read
// may hold a checkout, and removing it without looking is the failure salvage
// exists to prevent.
//
// A `.git` that is a symlink is an error too. Its working tree is inside the
// task directory and would be deleted, but inspecting it would run Git against
// a repository outside the task directory that nothing here created. Nothing in
// the daemon makes such a checkout, so keeping the directory costs little.
func findGitRoots(workdir string) ([]string, error) {
	var roots []string
	err := filepath.WalkDir(workdir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if path == workdir && errors.Is(walkErr, os.ErrNotExist) {
				return filepath.SkipAll
			}
			return walkErr
		}
		if entry.Name() == ".git" {
			if entry.Type()&linkedDirModes != 0 {
				return fmt.Errorf("%s: %w", path, errSalvageGitLink)
			}
			roots = append(roots, filepath.Dir(path))
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&linkedDirModes != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() && entry.Name() == "node_modules" {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("search %s for git checkouts: %w", workdir, err)
	}
	sort.Strings(roots)
	return roots, nil
}

type statusEntry struct {
	Status string `json:"status"`
	Path   string `json:"path"`
}

// parseStatusZ parses `git status --porcelain=v1 -z`. A rename or copy record
// is followed by a second NUL-terminated field naming the original path; that
// field has no status prefix and is skipped, because the new path is the one
// that exists on disk.
func parseStatusZ(out string) []statusEntry {
	fields := strings.Split(out, "\x00")
	var entries []statusEntry
	for i := 0; i < len(fields); i++ {
		record := fields[i]
		if len(record) < 4 {
			continue
		}
		status := record[:2]
		entries = append(entries, statusEntry{Status: status, Path: record[3:]})
		if strings.ContainsAny(status, "RC") {
			i++
		}
	}
	return entries
}

type salvageRepo struct {
	root     string
	rel      string // path relative to the workdir, "." for the workdir itself
	head     string // "" when HEAD is unborn
	branch   string // "" when HEAD is detached
	dirty    []statusEntry
	unpushed int
	nested   []string // absolute roots of checkouts nested below root
}

func (r *salvageRepo) safe() bool { return len(r.dirty) == 0 && r.unpushed == 0 }

func inspectSalvageRepo(ctx context.Context, workdir, root string, allRoots []string) (*salvageRepo, error) {
	rel, err := filepath.Rel(workdir, root)
	if err != nil {
		return nil, fmt.Errorf("relative path of %s: %w", root, err)
	}
	repo := &salvageRepo{root: root, rel: filepath.ToSlash(rel)}
	for _, other := range allRoots {
		if other != root && strings.HasPrefix(other, root+string(os.PathSeparator)) {
			repo.nested = append(repo.nested, other)
		}
	}

	head, err := runSalvageGit(ctx, root, nil, "", "rev-parse", "--verify", "--quiet", "HEAD")
	switch {
	case err == nil:
		repo.head = strings.TrimSpace(head)
	case isGitExitCode(err, 1) && strings.TrimSpace(head) == "":
		// Exit 1 with no output is how --quiet reports an unborn HEAD. A
		// checkout that never committed can still hold files worth saving.
	default:
		return nil, err
	}
	if branch, err := runSalvageGit(ctx, root, nil, "", "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
		repo.branch = strings.TrimSpace(branch)
	}

	// --ignore-submodules=none because a repository's own config can hide a
	// dirty submodule, and hidden is exactly what this check must not accept.
	status, err := runSalvageGit(ctx, root, nil, "", "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return nil, err
	}
	repo.dirty = parseStatusZ(status)

	if repo.head != "" {
		repo.unpushed, err = repocache.CountCommitsNotOnRemote(ctx, root, "HEAD", salvageOriginRemote)
		if err != nil {
			return nil, err
		}
	}
	return repo, nil
}

// payloadBytes sums the size of the files a snapshot would hash, before
// anything is hashed. A checkout with a multi-gigabyte untracked file must be
// refused by looking at the file, not after Git has copied it into an object.
func (r *salvageRepo) payloadBytes() (int64, error) {
	nested := make(map[string]struct{}, len(r.nested))
	for _, n := range r.nested {
		nested[n] = struct{}{}
	}
	var total int64
	for _, entry := range r.dirty {
		full := filepath.Join(r.root, filepath.FromSlash(entry.Path))
		info, err := os.Lstat(full)
		if errors.Is(err, os.ErrNotExist) {
			continue // a deleted file adds nothing to the snapshot
		}
		if err != nil {
			return 0, fmt.Errorf("measure %s: %w", full, err)
		}
		switch {
		case info.Mode()&linkedDirModes != 0:
			total += info.Size()
		case info.IsDir():
			if _, isNested := nested[filepath.Clean(full)]; isNested {
				continue // salvaged as its own checkout
			}
			total += dirSize(full)
		default:
			total += info.Size()
		}
	}
	return total, nil
}

// ---- bundle writing --------------------------------------------------------

type salvageFile struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

// salvageManifest describes one bundle. It is written after the bundle, so a
// manifest on disk always names a bundle that exists and passed verification.
type salvageManifest struct {
	Version     int       `json:"version"`
	Kind        string    `json:"kind"`
	CreatedAt   time.Time `json:"created_at"`
	TaskDir     string    `json:"task_dir,omitempty"`
	WorkspaceID string    `json:"workspace_id,omitempty"`
	TaskID      string    `json:"task_id,omitempty"`
	IssueID     string    `json:"issue_id,omitempty"`
	RepoPath    string    `json:"repo_path,omitempty"` // checkout path relative to the task workdir, or the bare repo path for a branch
	Branch      string    `json:"branch,omitempty"`
	HeadSHA     string    `json:"head_sha,omitempty"`
	SnapshotSHA string    `json:"snapshot_sha,omitempty"`

	BundleFile    string   `json:"bundle_file"`
	BundleBytes   int64    `json:"bundle_bytes"`
	BundleRef     string   `json:"bundle_ref"`
	Prerequisites []string `json:"prerequisite_commits"`
	RestoreHint   string   `json:"restore_hint"`

	UnpushedCommits  int            `json:"unpushed_commits"`
	DirtyFiles       []statusEntry  `json:"dirty_files,omitempty"`
	DirtyFilesTotal  int            `json:"dirty_files_total"`
	IgnoredFiles     []salvageFile  `json:"ignored_files,omitempty"`
	IgnoredTotal     int            `json:"ignored_files_total"`
	IgnoredListError string         `json:"ignored_files_error,omitempty"`
	Notes            []string       `json:"notes"`
	Extra            map[string]any `json:"extra,omitempty"`
}

var bundleRequiresRE = regexp.MustCompile(`(?m)^([0-9a-f]{40}|[0-9a-f]{64})\b`)

// requiredCommitsFromVerify reads the prerequisite commits out of `git bundle
// verify` output. Git prints "The bundle requires this ref:" (or "these N
// refs:") followed by one object id per line, and prints the ref the bundle
// contains under a different heading, so only the lines after the "requires"
// heading count.
func requiredCommitsFromVerify(out string) []string {
	_, after, found := strings.Cut(out, "The bundle requires")
	if !found {
		return nil
	}
	if idx := strings.Index(after, "The bundle uses"); idx >= 0 {
		after = after[:idx]
	}
	// The heading line itself carries no object id; only the lines below do.
	_, body, _ := strings.Cut(after, "\n")
	var shas []string
	for _, m := range bundleRequiresRE.FindAllStringSubmatch(body, -1) {
		shas = append(shas, m[1])
	}
	return shas
}

func randomNonce() (string, error) {
	var b [salvageNonceBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("random nonce: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

var unsafeNameChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func salvageSafeName(s string) string {
	s = strings.Trim(unsafeNameChars.ReplaceAllString(s, "_"), "._-")
	if len(s) > 48 {
		s = s[:48]
	}
	if s == "" {
		return "x"
	}
	return s
}

func (d *Daemon) salvageDir() string {
	return filepath.Join(d.cfg.WorkspacesRoot, salvageDirName)
}

// salvageHasRoom reports whether .salvage may take another bundle. The check
// runs before any salvage work, and it never evicts: an older bundle is the
// only copy of somebody's work until its TTL says otherwise.
func (d *Daemon) salvageHasRoom(ctx context.Context) error {
	size, err := dirSizeContext(ctx, d.salvageDir())
	if err != nil {
		return fmt.Errorf("measure salvage directory: %w", err)
	}
	if size >= d.cfg.GCSalvageTotalMaxBytes {
		return fmt.Errorf("%w (%d of %d bytes used)", errSalvageFull, size, d.cfg.GCSalvageTotalMaxBytes)
	}
	return nil
}

// publishSalvageFile writes data to <path>.tmp, syncs it, then renames it. A
// crash leaves a .tmp file, which sweepSalvage removes, and never a truncated
// manifest that names a bundle.
func publishSalvageFile(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// createVerifiedBundle writes a bundle of ref minus everything origin already
// has, verifies it, and moves it into place. gitDir is the repository the
// bundle is read from. On success the bundle is final; on any error nothing is
// left behind.
func (d *Daemon) createVerifiedBundle(ctx context.Context, gitDir, ref, finalPath string) (size int64, prerequisites []string, err error) {
	tmp := finalPath + ".tmp"
	defer func() {
		if err != nil {
			os.Remove(tmp)
		}
	}()
	// `--not --remotes=origin` and not bare `--remotes`: a commit held only by
	// some other configured remote is not backed up where the daemon fetches.
	if _, err = runSalvageGit(ctx, gitDir, nil, "", "bundle", "create", tmp, ref, "--not", "--remotes="+salvageOriginRemote); err != nil {
		return 0, nil, err
	}
	info, statErr := os.Stat(tmp)
	if statErr != nil {
		return 0, nil, statErr
	}
	if info.Size() > d.cfg.GCSalvageMaxBytes {
		return 0, nil, fmt.Errorf("%w: bundle is %d bytes, cap is %d", errSalvageTooLarge, info.Size(), d.cfg.GCSalvageMaxBytes)
	}
	verifyOut, err := runSalvageGitCombined(ctx, gitDir, nil, "bundle", "verify", tmp)
	if err != nil {
		return 0, nil, fmt.Errorf("verify bundle: %w", err)
	}
	if err = os.Rename(tmp, finalPath); err != nil {
		return 0, nil, err
	}
	return info.Size(), requiredCommitsFromVerify(verifyOut), nil
}

// ---- checkout salvage ------------------------------------------------------

type salvageOwner struct {
	taskDir     string
	workspaceID string
	taskID      string
	issueID     string
}

// salvageTaskDir saves every unsafe checkout below taskDir/workdir and reports
// whether taskDir may now be removed. It runs inside the env-root reservation
// that applyGCAction holds, after the ownership proof, so a task cannot start
// on the directory while it is being read.
//
// A directory without a Git checkout is removable at once, as before.
func (d *Daemon) salvageTaskDir(ctx context.Context, taskDir string, owner execenv.EnvRootOwner) bool {
	if !d.cfg.GCSalvageEnabled {
		return true
	}
	workdir := filepath.Join(taskDir, "workdir")
	roots, err := findGitRoots(workdir)
	if err != nil {
		d.salvageKeepsDir(taskDir, "search for git checkouts failed", err)
		return false
	}
	if len(roots) == 0 {
		return true
	}

	// Inspect and measure every checkout before writing any bundle. If the
	// second checkout is over the cap, writing the first one's bundle now would
	// leave a duplicate of it in .salvage on every cycle that keeps the directory.
	var unsafe []*salvageRepo
	for _, root := range roots {
		repo, err := inspectSalvageRepo(ctx, workdir, root, roots)
		if err != nil {
			d.salvageKeepsDir(taskDir, "inspect checkout failed", fmt.Errorf("%s: %w", root, err))
			return false
		}
		if repo.safe() {
			continue
		}
		payload, err := repo.payloadBytes()
		if err != nil {
			d.salvageKeepsDir(taskDir, "measure checkout failed", err)
			return false
		}
		if payload > d.cfg.GCSalvageMaxBytes {
			d.salvageKeepsDir(taskDir, "unsaved files exceed salvage cap", fmt.Errorf("%w: %s holds %d bytes, cap is %d", errSalvageTooLarge, root, payload, d.cfg.GCSalvageMaxBytes))
			return false
		}
		unsafe = append(unsafe, repo)
	}
	if len(unsafe) == 0 {
		return true
	}

	if err := d.salvageHasRoom(ctx); err != nil {
		d.salvageKeepsDir(taskDir, "salvage directory is full", err)
		return false
	}

	so := salvageOwner{taskDir: taskDir, workspaceID: owner.WorkspaceID, taskID: owner.TaskID}
	if meta, err := execenv.ReadGCMeta(taskDir); err == nil {
		so.issueID = meta.IssueID
	}
	for _, repo := range unsafe {
		if err := d.salvageCheckout(ctx, so, repo); err != nil {
			d.salvageKeepsDir(taskDir, "salvage failed", fmt.Errorf("%s: %w", repo.root, err))
			return false
		}
	}
	return true
}

func (d *Daemon) salvageCheckout(ctx context.Context, so salvageOwner, repo *salvageRepo) (err error) {
	salvageRoot := d.salvageDir()
	if err := os.MkdirAll(salvageRoot, 0o700); err != nil {
		return fmt.Errorf("create salvage directory: %w", err)
	}
	nonce, err := randomNonce()
	if err != nil {
		return err
	}
	scratch := filepath.Join(salvageRoot, salvageTmpPrefix+nonce)
	if err := os.Mkdir(scratch, 0o700); err != nil {
		return fmt.Errorf("create scratch directory: %w", err)
	}
	defer os.RemoveAll(scratch)

	// The bundle is read from a throwaway bare repository, never from the
	// shared repo cache. A ref that names a commit the shared repository does
	// not have breaks every `git fetch` and `git gc` that walks refs, and a
	// crash between creating the ref and deleting it would leave that behind.
	// Here the ref and the snapshot objects die with the scratch directory.
	scratchRepo := filepath.Join(scratch, "repo")
	if _, err := runSalvageGit(ctx, scratch, nil, "", "init", "--quiet", "--bare", scratchRepo); err != nil {
		return err
	}
	realObjects, err := gitPath(ctx, repo.root, "objects")
	if err != nil {
		return err
	}
	scratchObjects := filepath.Join(scratchRepo, "objects")
	if err := os.WriteFile(filepath.Join(scratchObjects, "info", "alternates"), []byte(realObjects+"\n"), 0o600); err != nil {
		return fmt.Errorf("link scratch repository to %s: %w", realObjects, err)
	}

	refName := salvageRefPrefix + salvageSafeName(so.workspaceID) + "-" + salvageSafeName(so.taskID) + "-" + nonce
	target := repo.head
	if len(repo.dirty) > 0 {
		target, err = d.buildSnapshot(ctx, repo, scratch, scratchObjects, realObjects)
		if err != nil {
			return err
		}
	}
	snapshotSHA := ""
	if len(repo.dirty) > 0 {
		snapshotSHA = target
	}
	if _, err := runSalvageGit(ctx, scratchRepo, nil, "", "update-ref", refName, target); err != nil {
		return err
	}
	if err := mirrorOriginRefs(ctx, repo.root, scratchRepo); err != nil {
		return err
	}

	stamp := time.Now().UTC().Format(salvageStampLayout)
	base := stamp + "_" + salvageSafeName(so.workspaceID) + "_" + salvageSafeName(filepath.Base(so.taskDir)) + "_" + salvageSafeName(repo.rel) + "-" + nonce
	bundlePath := filepath.Join(salvageRoot, base+".bundle")
	size, prereqs, err := d.createVerifiedBundle(ctx, scratchRepo, refName, bundlePath)
	if err != nil {
		return err
	}

	manifest := salvageManifest{
		Version:         salvageManifestVersion,
		Kind:            salvageKindWorktree,
		CreatedAt:       time.Now().UTC(),
		TaskDir:         so.taskDir,
		WorkspaceID:     so.workspaceID,
		TaskID:          so.taskID,
		IssueID:         so.issueID,
		RepoPath:        repo.rel,
		Branch:          repo.branch,
		HeadSHA:         repo.head,
		SnapshotSHA:     snapshotSHA,
		BundleFile:      filepath.Base(bundlePath),
		BundleBytes:     size,
		BundleRef:       refName,
		Prerequisites:   prereqs,
		RestoreHint:     "git fetch " + filepath.Base(bundlePath) + " " + refName + " (run inside a clone that has the prerequisite commits)",
		UnpushedCommits: repo.unpushed,
		DirtyFilesTotal: len(repo.dirty),
		Notes: []string{
			"Git-ignored files are not in the bundle. Their names and sizes are listed under ignored_files.",
			"Git LFS objects are not in the bundle. Pointer files are.",
			"The snapshot commit has HEAD as its parent and holds tracked edits plus untracked files that Git does not ignore.",
		},
	}
	manifest.DirtyFiles = repo.dirty
	if len(manifest.DirtyFiles) > salvageMaxDirtyEntries {
		manifest.DirtyFiles = manifest.DirtyFiles[:salvageMaxDirtyEntries]
	}
	manifest.IgnoredFiles, manifest.IgnoredTotal, manifest.IgnoredListError = listIgnored(ctx, repo.root)

	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		os.Remove(bundlePath)
		return fmt.Errorf("encode manifest: %w", err)
	}
	if err := publishSalvageFile(filepath.Join(salvageRoot, base+".json"), data); err != nil {
		os.Remove(bundlePath)
		return fmt.Errorf("write manifest: %w", err)
	}
	d.logger.Info("gc: salvaged unsaved work",
		"dir", so.taskDir,
		"checkout", repo.rel,
		"bundle", bundlePath,
		"bundle_bytes", size,
		"dirty_files", len(repo.dirty),
		"unpushed_commits", repo.unpushed,
	)
	return nil
}

func gitPath(ctx context.Context, root, name string) (string, error) {
	out, err := runSalvageGit(ctx, root, nil, "", "rev-parse", "--path-format=absolute", "--git-path", name)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// buildSnapshot commits the working tree of repo without touching the checkout
// or the shared object store, and returns the snapshot commit id.
//
// Three pieces of environment make that true, and dropping any one of them
// makes the GC modify what it is only meant to read:
//   - GIT_INDEX_FILE names a COPY of the real index. `git add` then updates the
//     copy, and the checkout's own index, which a resumed task still uses, is
//     untouched. The index is copied rather than rebuilt with read-tree so a
//     sparse checkout keeps its skip-worktree entries; read-tree would mark
//     every path outside the cone as deleted.
//   - GIT_OBJECT_DIRECTORY sends every new object to the scratch repository.
//   - GIT_ALTERNATE_OBJECT_DIRECTORIES lets Git still read the real objects,
//     which the parent commit and unchanged blobs live in.
func (d *Daemon) buildSnapshot(ctx context.Context, repo *salvageRepo, scratch, scratchObjects, realObjects string) (string, error) {
	realIndex, err := gitPath(ctx, repo.root, "index")
	if err != nil {
		return "", err
	}
	scratchIndex := filepath.Join(scratch, "index")
	if err := copyFileIfExists(realIndex, scratchIndex); err != nil {
		return "", fmt.Errorf("copy index: %w", err)
	}
	env := []string{
		"GIT_INDEX_FILE=" + scratchIndex,
		"GIT_OBJECT_DIRECTORY=" + scratchObjects,
		"GIT_ALTERNATE_OBJECT_DIRECTORIES=" + realObjects,
	}

	// Nested checkouts are excluded: each is bundled on its own, and `git add`
	// on an embedded repository with no commit fails the whole command.
	addArgs := []string{"add", "-A", "--", "."}
	for _, nested := range repo.nested {
		rel, err := filepath.Rel(repo.root, nested)
		if err != nil {
			return "", err
		}
		addArgs = append(addArgs, ":(exclude,literal)"+filepath.ToSlash(rel))
	}
	if _, err := runSalvageGit(ctx, repo.root, env, "", addArgs...); err != nil {
		return "", err
	}
	tree, err := runSalvageGit(ctx, repo.root, env, "", "write-tree")
	if err != nil {
		return "", err
	}
	commitArgs := []string{"commit-tree", strings.TrimSpace(tree), "-m", "multica gc salvage snapshot"}
	if repo.head != "" {
		commitArgs = append(commitArgs, "-p", repo.head)
	}
	commit, err := runSalvageGit(ctx, repo.root, env, "", commitArgs...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(commit), nil
}

func copyFileIfExists(src, dst string) error {
	in, err := os.Open(src)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// mirrorOriginRefs copies the remote-tracking refs of origin into the scratch
// repository, so the bundle command's `--not --remotes=origin` excludes exactly
// what it would exclude in the real repository.
func mirrorOriginRefs(ctx context.Context, realRoot, scratchRepo string) error {
	out, err := runSalvageGit(ctx, realRoot, nil, "", "for-each-ref", "--format=%(objectname) %(refname)", "refs/remotes/"+salvageOriginRemote+"/")
	if err != nil {
		return err
	}
	var script strings.Builder
	for _, line := range strings.Split(out, "\n") {
		sha, ref, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		fmt.Fprintf(&script, "update %s %s\n", ref, sha)
	}
	if script.Len() == 0 {
		return nil
	}
	_, err = runSalvageGit(ctx, scratchRepo, nil, script.String(), "update-ref", "--stdin")
	return err
}

// listIgnored names the ignored paths a bundle does not carry, so a reader of
// the manifest can tell what was lost. Directories are listed whole, which
// keeps a node_modules tree to one entry. A listing failure is recorded in the
// manifest and does not block salvage: the list is information, not the work.
func listIgnored(ctx context.Context, root string) (files []salvageFile, total int, listErr string) {
	out, err := runSalvageGit(ctx, root, nil, "", "ls-files", "-z", "--others", "--ignored", "--exclude-standard", "--directory")
	if err != nil {
		return nil, 0, err.Error()
	}
	for _, name := range strings.Split(out, "\x00") {
		if name == "" {
			continue
		}
		total++
		if len(files) >= salvageMaxIgnoredEntries {
			continue
		}
		full := filepath.Join(root, filepath.FromSlash(name))
		var size int64
		if info, err := os.Lstat(full); err == nil {
			if info.IsDir() {
				size = dirSize(full)
			} else {
				size = info.Size()
			}
		}
		files = append(files, salvageFile{Path: name, Bytes: size})
	}
	return files, total, ""
}

// ---- keep-and-warn state ---------------------------------------------------

type salvageState struct {
	Failures   int       `json:"failures"`
	LastWarned time.Time `json:"last_warned"`
}

// salvageKeepsDir records that the directory stays this cycle. The first
// failure warns at once; later cycles warn once per salvageWarnInterval, so a
// 15-minute GC interval does not write one warning per directory per cycle for
// the life of a directory that will never fit.
//
// The counter file is written in place and the directory mtime is restored:
// orphanByMTime reads that mtime, and a new file in the directory would
// otherwise restart the orphan clock of the directory being examined.
func (d *Daemon) salvageKeepsDir(taskDir, reason string, cause error) {
	statePath := filepath.Join(taskDir, salvageStateFile)
	var state salvageState
	if data, err := os.ReadFile(statePath); err == nil {
		_ = json.Unmarshal(data, &state)
	}
	now := time.Now()
	warn := state.Failures == 0 || now.Sub(state.LastWarned) >= salvageWarnInterval
	state.Failures++
	if warn {
		state.LastWarned = now
	}

	if data, err := json.Marshal(state); err == nil {
		if writeErr := writeFileKeepingDirMTime(taskDir, salvageStateFile, data); writeErr != nil {
			d.logger.Debug("gc: write salvage state failed", "dir", taskDir, "error", writeErr)
		}
	}

	if warn {
		d.logger.Warn("gc: keeping task directory, its unsaved work could not be salvaged",
			"dir", taskDir,
			"reason", reason,
			"error", cause,
			"failures", state.Failures,
		)
		return
	}
	d.logger.Debug("gc: keeping task directory, salvage still blocked",
		"dir", taskDir, "reason", reason, "failures", state.Failures)
}

// ---- cycle housekeeping ----------------------------------------------------

// sweepSalvage runs at the start of every GC cycle. It removes the scratch
// directories and half-written .tmp files of an interrupted salvage, then
// removes each bundle and manifest pair whose newer file is older than
// GCSalvageTTL. The manifest goes first, so a manifest on disk still always
// names a bundle that exists, and the pair never outlives one of its halves.
//
// Only names that match the shape salvage writes (salvageScratchName,
// salvageFileName) are touched, so a file an operator drops into .salvage is
// never deleted.
func (d *Daemon) sweepSalvage() {
	dir := d.salvageDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			d.logger.Warn("gc: read salvage directory failed", "dir", dir, "error", err)
		}
		return
	}
	type salvagePair struct {
		manifest, bundle string
		newest           time.Time
	}
	pairs := map[string]*salvagePair{}
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(dir, name)
		if entry.IsDir() {
			if salvageScratchName.MatchString(name) {
				if err := os.RemoveAll(path); err != nil {
					d.logger.Warn("gc: remove stale salvage scratch failed", "path", path, "error", err)
				}
			}
			continue
		}
		m := salvageFileName.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		if m[3] != "" {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				d.logger.Warn("gc: remove stale salvage temp file failed", "path", path, "error", err)
			}
			continue
		}
		if d.cfg.GCSalvageTTL <= 0 {
			continue
		}
		p := pairs[m[1]]
		if p == nil {
			p = &salvagePair{}
			pairs[m[1]] = p
		}
		if m[2] == "json" {
			p.manifest = path
		} else {
			p.bundle = path
		}
		modTime := time.Now() // an unreadable age keeps the pair
		if info, err := entry.Info(); err == nil {
			modTime = info.ModTime()
		}
		if modTime.After(p.newest) {
			p.newest = modTime
		}
	}
	expired := 0
	for _, p := range pairs {
		if time.Since(p.newest) <= d.cfg.GCSalvageTTL {
			continue
		}
		removed := true
		for _, path := range []string{p.manifest, p.bundle} {
			if path == "" {
				continue
			}
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				d.logger.Warn("gc: remove expired salvage file failed", "path", path, "error", err)
				removed = false
				break
			}
		}
		if removed {
			expired++
		}
	}
	if expired > 0 {
		d.logger.Info("gc: removed expired salvage bundles", "count", expired, "ttl", d.cfg.GCSalvageTTL)
	}
}
