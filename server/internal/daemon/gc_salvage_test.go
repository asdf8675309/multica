package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

// These tests use real Git repositories, real linked worktrees and real
// bundles. Git is never mocked: the failure modes under test (a snapshot that
// leaks into the shared object store, a sparse checkout read as deleted files,
// a remote glob that matches the wrong remote) only exist in the real program.

type salvageFixture struct {
	t      *testing.T
	d      *Daemon
	root   string
	remote string // bare "origin"
	cache  string // bare repo cache with the remote-tracking layout the daemon uses
	logs   *safeBuffer
}

// safeBuffer is a log sink that is safe to write from the GC and read from the test.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func newSafeBuffer() *safeBuffer { return &safeBuffer{} }

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func gitOutRaw(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git -C %s %s: %v: %s", dir, strings.Join(args, " "), err, stderr.String())
	}
	return out
}

func writeSalvageFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func newSalvageFixture(t *testing.T) *salvageFixture {
	t.Helper()
	d := newGCTestDaemon(t, http.NewServeMux())
	logs := newSafeBuffer()
	d.logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	d.cfg.GCSalvageEnabled = true
	d.cfg.GCSalvageMaxBytes = 1 << 20
	d.cfg.GCSalvageTotalMaxBytes = 64 << 20
	d.cfg.GCSalvageTTL = 168 * time.Hour

	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	runGitForGC(t, "", "init", "--bare", "-b", "main", remote)

	seed := filepath.Join(base, "seed")
	runGitForGC(t, "", "init", "-b", "main", seed)
	writeSalvageFile(t, filepath.Join(seed, "a.txt"), []byte("one\n"))
	writeSalvageFile(t, filepath.Join(seed, "dir", "b.txt"), []byte("bee\n"))
	writeSalvageFile(t, filepath.Join(seed, "other", "c.txt"), []byte("sea\n"))
	writeSalvageFile(t, filepath.Join(seed, ".gitignore"), []byte("ignored/\n*.log\n"))
	runGitForGC(t, seed, "add", "-A")
	runGitForGC(t, seed, "commit", "-m", "seed")
	runGitForGC(t, seed, "remote", "add", "origin", remote)
	runGitForGC(t, seed, "push", "origin", "main")

	cache := filepath.Join(base, "cache.git")
	runGitForGC(t, "", "clone", "--bare", remote, cache)
	runGitForGC(t, cache, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
	runGitForGC(t, cache, "fetch", "origin")

	return &salvageFixture{t: t, d: d, root: d.cfg.WorkspacesRoot, remote: remote, cache: cache, logs: logs}
}

// newTask makes a completed issue task whose workdir/repo is a linked worktree
// of the shared cache, cut from origin/main: clean and fully pushed.
func (f *salvageFixture) newTask(name string) (taskDir, checkout string) {
	f.t.Helper()
	taskDir = createTaskDir(f.t, f.root, "ws1", "issue-"+name, &execenv.GCMeta{
		IssueID:     "iss-" + name,
		WorkspaceID: "ws1",
		CompletedAt: time.Now().Add(-48 * time.Hour),
	})
	checkout = filepath.Join(taskDir, "workdir", "repo")
	runGitForGC(f.t, "", "-C", f.cache, "worktree", "add", "-b", "agent/"+name+"/1", checkout, "refs/remotes/origin/main")
	return taskDir, checkout
}

func (f *salvageFixture) salvageDirPath() string { return filepath.Join(f.root, salvageDirName) }

type salvageEntry struct {
	m      salvageManifest
	bundle string
}

func (f *salvageFixture) entries() []salvageEntry {
	f.t.Helper()
	files, _ := filepath.Glob(filepath.Join(f.salvageDirPath(), "*.json"))
	sort.Strings(files)
	var out []salvageEntry
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			f.t.Fatal(err)
		}
		var m salvageManifest
		if err := json.Unmarshal(data, &m); err != nil {
			f.t.Fatalf("manifest %s: %v", file, err)
		}
		bundle := filepath.Join(f.salvageDirPath(), m.BundleFile)
		if _, err := os.Stat(bundle); err != nil {
			f.t.Fatalf("manifest %s names a bundle that is not on disk: %v", file, err)
		}
		out = append(out, salvageEntry{m: m, bundle: bundle})
	}
	return out
}

func (f *salvageFixture) noSalvageOutput() {
	f.t.Helper()
	matches, _ := filepath.Glob(filepath.Join(f.salvageDirPath(), "*"))
	if len(matches) != 0 {
		f.t.Fatalf("salvage directory should hold nothing, found %v", matches)
	}
}

// restore fetches the bundle's ref into a fresh clone of origin. That clone
// holds the base commits, which is the precondition the manifest states.
func (f *salvageFixture) restore(e salvageEntry) string {
	f.t.Helper()
	clone := filepath.Join(f.t.TempDir(), "clone")
	runGitForGC(f.t, "", "clone", f.remote, clone)
	runGitForGC(f.t, clone, "fetch", e.bundle, e.m.BundleRef)
	return clone
}

func blobAt(t *testing.T, clone, path string) []byte {
	t.Helper()
	return gitOutRaw(t, clone, "show", "FETCH_HEAD:"+path)
}

func mustExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("%s should exist: %v", path, err)
	}
}

func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("%s should not exist, lstat err = %v", path, err)
	}
}

func TestSalvage_CleanPushedCheckoutIsRemovedWithoutBundle(t *testing.T) {
	t.Parallel()
	f := newSalvageFixture(t)
	taskDir, checkout := f.newTask("clean")
	// An ignored file must never block removal.
	writeSalvageFile(t, filepath.Join(checkout, "ignored", "cache.bin"), randomBytes(t, 4096))

	if _, removed := f.d.cleanTaskDir(taskDir); !removed {
		t.Fatal("a clean, pushed checkout must be removed")
	}
	mustNotExist(t, taskDir)
	f.noSalvageOutput()
}

func TestSalvage_IgnoredFileOnlyIsRemovedWithoutBundle(t *testing.T) {
	t.Parallel()
	f := newSalvageFixture(t)
	taskDir, checkout := f.newTask("ignored")
	writeSalvageFile(t, filepath.Join(checkout, "build.log"), []byte("noise\n"))
	writeSalvageFile(t, filepath.Join(checkout, "ignored", "x.bin"), randomBytes(t, 2048))

	if _, removed := f.d.cleanTaskDir(taskDir); !removed {
		t.Fatal("a checkout with only ignored files must be removed")
	}
	f.noSalvageOutput()
}

func TestSalvage_DirtyTrackedEditIsBundledThenRemoved(t *testing.T) {
	t.Parallel()
	f := newSalvageFixture(t)
	taskDir, checkout := f.newTask("dirty")
	edited := []byte("one\nedited by the agent\n")
	writeSalvageFile(t, filepath.Join(checkout, "a.txt"), edited)
	writeSalvageFile(t, filepath.Join(checkout, "ignored", "cache.bin"), randomBytes(t, 3000))
	head := strings.TrimSpace(string(gitOutRaw(t, checkout, "rev-parse", "HEAD")))
	originMain := strings.TrimSpace(string(gitOutRaw(t, f.cache, "rev-parse", "refs/remotes/origin/main")))

	if _, removed := f.d.cleanTaskDir(taskDir); !removed {
		t.Fatal("the directory must be removed once its work is salvaged")
	}
	mustNotExist(t, taskDir)

	entries := f.entries()
	if len(entries) != 1 {
		t.Fatalf("want exactly one bundle, got %d", len(entries))
	}
	e := entries[0]
	m := e.m
	if m.Kind != salvageKindWorktree || m.WorkspaceID != "ws1" || m.TaskID != "dirty" || m.IssueID != "iss-dirty" {
		t.Fatalf("manifest identity wrong: %+v", m)
	}
	if m.TaskDir != taskDir || m.RepoPath != "repo" || m.Branch != "agent/dirty/1" || m.HeadSHA != head {
		t.Fatalf("manifest location wrong: task_dir=%q repo=%q branch=%q head=%q", m.TaskDir, m.RepoPath, m.Branch, m.HeadSHA)
	}
	if m.DirtyFilesTotal != 1 || len(m.DirtyFiles) != 1 || m.DirtyFiles[0].Path != "a.txt" {
		t.Fatalf("manifest dirty list wrong: %+v", m.DirtyFiles)
	}
	if !strings.HasPrefix(m.BundleRef, salvageRefPrefix) || strings.Contains(m.BundleRef, " ") {
		t.Fatalf("bundle ref %q is not a salvage ref", m.BundleRef)
	}
	if len(m.Prerequisites) != 1 || m.Prerequisites[0] != originMain {
		t.Fatalf("prerequisites = %v, want [%s]", m.Prerequisites, originMain)
	}
	if len(m.IgnoredFiles) == 0 || m.IgnoredFiles[0].Path != "ignored/" || m.IgnoredFiles[0].Bytes < 3000 {
		t.Fatalf("ignored files not listed with sizes: %+v", m.IgnoredFiles)
	}
	notes := strings.Join(m.Notes, "\n")
	if !strings.Contains(notes, "Git-ignored files are not in the bundle") || !strings.Contains(notes, "LFS") {
		t.Fatalf("manifest must state what the bundle does not hold, notes = %q", notes)
	}
	if !strings.Contains(m.RestoreHint, "git fetch") || !strings.Contains(m.RestoreHint, m.BundleRef) {
		t.Fatalf("restore hint = %q", m.RestoreHint)
	}

	clone := f.restore(e)
	if got := blobAt(t, clone, "a.txt"); !bytes.Equal(got, edited) {
		t.Fatalf("restored a.txt = %q, want %q", got, edited)
	}
	snapshotParent := strings.TrimSpace(string(gitOutRaw(t, clone, "rev-parse", "FETCH_HEAD^")))
	if snapshotParent != head {
		t.Fatalf("snapshot parent = %s, want HEAD %s", snapshotParent, head)
	}
	if author := strings.TrimSpace(string(gitOutRaw(t, clone, "log", "-1", "--format=%an <%ae>", "FETCH_HEAD"))); author != salvageIdentityName+" <"+salvageIdentityEmail+">" {
		t.Fatalf("snapshot author = %q, want the fixed salvage identity", author)
	}
}

func TestSalvage_UntrackedNonIgnoredFileIsBundled(t *testing.T) {
	t.Parallel()
	f := newSalvageFixture(t)
	taskDir, checkout := f.newTask("untracked")
	notes := []byte("half-finished notes\n\x00binary tail\xff")
	writeSalvageFile(t, filepath.Join(checkout, "notes", "todo.md"), notes)

	if _, removed := f.d.cleanTaskDir(taskDir); !removed {
		t.Fatal("directory must be removed after salvage")
	}
	entries := f.entries()
	if len(entries) != 1 {
		t.Fatalf("want one bundle, got %d", len(entries))
	}
	clone := f.restore(entries[0])
	if got := blobAt(t, clone, "notes/todo.md"); !bytes.Equal(got, notes) {
		t.Fatalf("restored untracked file differs: %q", got)
	}
}

func TestSalvage_UnpushedCommitIsBundled(t *testing.T) {
	t.Parallel()
	f := newSalvageFixture(t)
	taskDir, checkout := f.newTask("unpushed")
	writeSalvageFile(t, filepath.Join(checkout, "feature.txt"), []byte("committed but never pushed\n"))
	runGitForGC(t, checkout, "add", "feature.txt")
	runGitForGC(t, checkout, "commit", "-m", "agent work")
	head := strings.TrimSpace(string(gitOutRaw(t, checkout, "rev-parse", "HEAD")))

	if _, removed := f.d.cleanTaskDir(taskDir); !removed {
		t.Fatal("directory must be removed after salvage")
	}
	entries := f.entries()
	if len(entries) != 1 {
		t.Fatalf("want one bundle, got %d", len(entries))
	}
	if entries[0].m.UnpushedCommits != 1 || entries[0].m.SnapshotSHA != "" {
		t.Fatalf("clean tree with one unpushed commit: unpushed=%d snapshot=%q", entries[0].m.UnpushedCommits, entries[0].m.SnapshotSHA)
	}
	clone := f.restore(entries[0])
	if got := strings.TrimSpace(string(gitOutRaw(t, clone, "rev-parse", "FETCH_HEAD"))); got != head {
		t.Fatalf("restored tip = %s, want %s", got, head)
	}
	if got := blobAt(t, clone, "feature.txt"); string(got) != "committed but never pushed\n" {
		t.Fatalf("restored feature.txt = %q", got)
	}
}

// The push check must name origin. A commit that only some other remote holds
// is not backed up where the daemon fetches from.
func TestSalvage_CommitHeldOnlyByForeignRemoteCountsAsUnpushed(t *testing.T) {
	t.Parallel()
	f := newSalvageFixture(t)
	taskDir, checkout := f.newTask("foreign")
	writeSalvageFile(t, filepath.Join(checkout, "feature.txt"), []byte("only on the fork\n"))
	runGitForGC(t, checkout, "add", "feature.txt")
	runGitForGC(t, checkout, "commit", "-m", "agent work")
	head := strings.TrimSpace(string(gitOutRaw(t, checkout, "rev-parse", "HEAD")))
	// A remote-tracking ref of a different remote points at the commit.
	runGitForGC(t, f.cache, "update-ref", "refs/remotes/fork/agent", head)

	if _, removed := f.d.cleanTaskDir(taskDir); !removed {
		t.Fatal("directory must be removed after salvage")
	}
	if got := len(f.entries()); got != 1 {
		t.Fatalf("a commit held only by another remote must be salvaged, got %d bundles", got)
	}
}

func TestSalvage_OverCapKeepsDirectory(t *testing.T) {
	t.Parallel()

	t.Run("uncommitted payload is measured before anything is hashed", func(t *testing.T) {
		t.Parallel()
		f := newSalvageFixture(t)
		f.d.cfg.GCSalvageMaxBytes = 1024
		taskDir, checkout := f.newTask("bigfile")
		writeSalvageFile(t, filepath.Join(checkout, "big.bin"), randomBytes(t, 8192))

		if _, removed := f.d.cleanTaskDir(taskDir); removed {
			t.Fatal("a payload over the cap must keep the directory")
		}
		mustExist(t, filepath.Join(checkout, "big.bin"))
		// No salvage work started: the scratch and bundle directory were never made.
		mustNotExist(t, f.salvageDirPath())
		if !strings.Contains(f.logs.String(), "keeping task directory") {
			t.Fatalf("a kept directory must be logged, log = %s", f.logs.String())
		}
	})

	t.Run("a bundle over the cap is discarded", func(t *testing.T) {
		t.Parallel()
		f := newSalvageFixture(t)
		f.d.cfg.GCSalvageMaxBytes = 100 << 10
		taskDir, checkout := f.newTask("bigcommit")
		writeSalvageFile(t, filepath.Join(checkout, "big.bin"), randomBytes(t, 300<<10))
		runGitForGC(t, checkout, "add", "big.bin")
		runGitForGC(t, checkout, "commit", "-m", "big")

		if _, removed := f.d.cleanTaskDir(taskDir); removed {
			t.Fatal("a bundle over the cap must keep the directory")
		}
		mustExist(t, filepath.Join(checkout, "big.bin"))
		f.noSalvageOutput()
	})
}

func TestSalvage_RestoreRoundTripIsByteForByte(t *testing.T) {
	t.Parallel()
	f := newSalvageFixture(t)
	taskDir, checkout := f.newTask("roundtrip")
	// Binary content with every byte value, so line-ending or encoding damage shows.
	all := make([]byte, 256*4)
	for i := range all {
		all[i] = byte(i)
	}
	tracked := append([]byte("one\n"), randomBytes(t, 512)...)
	writeSalvageFile(t, filepath.Join(checkout, "a.txt"), tracked)
	writeSalvageFile(t, filepath.Join(checkout, "new", "all.bin"), all)
	writeSalvageFile(t, filepath.Join(checkout, "dir", "b.txt"), nil) // truncated tracked file
	if err := os.Remove(filepath.Join(checkout, "other", "c.txt")); err != nil {
		t.Fatal(err)
	}

	if _, removed := f.d.cleanTaskDir(taskDir); !removed {
		t.Fatal("directory must be removed after salvage")
	}
	entries := f.entries()
	if len(entries) != 1 {
		t.Fatalf("want one bundle, got %d", len(entries))
	}
	clone := f.restore(entries[0])
	if got := blobAt(t, clone, "a.txt"); !bytes.Equal(got, tracked) {
		t.Fatal("tracked edit differs after restore")
	}
	if got := blobAt(t, clone, "new/all.bin"); !bytes.Equal(got, all) {
		t.Fatal("untracked binary differs after restore")
	}
	if got := blobAt(t, clone, "dir/b.txt"); len(got) != 0 {
		t.Fatalf("truncated file restored with %d bytes", len(got))
	}
	names := string(gitOutRaw(t, clone, "ls-tree", "-r", "--name-only", "FETCH_HEAD"))
	if strings.Contains(names, "other/c.txt") {
		t.Fatal("a file the agent deleted must be absent from the snapshot")
	}
}

// Reading a checkout must not change it. A task that resumes in the same
// directory after a failed salvage sees its own index, and the shared cache
// stays free of snapshot objects and refs.
func TestSalvage_SnapshotLeavesCheckoutAndSharedRepoUntouched(t *testing.T) {
	t.Parallel()
	f := newSalvageFixture(t)
	taskDir, checkout := f.newTask("readonly")
	writeSalvageFile(t, filepath.Join(checkout, "a.txt"), []byte("edited\n"))
	writeSalvageFile(t, filepath.Join(checkout, "fresh.txt"), []byte("new\n"))

	indexPath := strings.TrimSpace(string(gitOutRaw(t, checkout, "rev-parse", "--path-format=absolute", "--git-path", "index")))
	indexBefore, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	statusBefore := gitOutRaw(t, checkout, "status", "--porcelain=v1", "-z")
	objectsBefore := string(gitOutRaw(t, f.cache, "count-objects", "-v"))

	owner := execenv.EnvRootOwner{WorkspaceID: "ws1", TaskID: "readonly"}
	if !f.d.salvageTaskDir(context.Background(), taskDir, owner) {
		t.Fatal("salvage must succeed")
	}

	indexAfter, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(indexBefore, indexAfter) {
		t.Fatal("salvage changed the checkout's own index")
	}
	if got := gitOutRaw(t, checkout, "status", "--porcelain=v1", "-z"); !bytes.Equal(got, statusBefore) {
		t.Fatalf("checkout status changed: %q -> %q", statusBefore, got)
	}
	if got := string(gitOutRaw(t, f.cache, "count-objects", "-v")); got != objectsBefore {
		t.Fatalf("shared object store changed:\n%s\n->\n%s", objectsBefore, got)
	}
	entries := f.entries()
	if len(entries) != 1 {
		t.Fatalf("want one bundle, got %d", len(entries))
	}
	snapshot := entries[0].m.SnapshotSHA
	if snapshot == "" {
		t.Fatal("a dirty tree must record its snapshot commit")
	}
	if out, err := exec.Command("git", "-C", f.cache, "cat-file", "-e", snapshot).CombinedOutput(); err == nil {
		t.Fatalf("snapshot commit %s leaked into the shared repo: %s", snapshot, out)
	}
	if refs := strings.TrimSpace(string(gitOutRaw(t, f.cache, "for-each-ref", salvageRefPrefix))); refs != "" {
		t.Fatalf("salvage left refs in the shared repo: %s", refs)
	}
	if leftover, _ := filepath.Glob(filepath.Join(f.salvageDirPath(), salvageTmpPrefix+"*")); len(leftover) != 0 {
		t.Fatalf("scratch directory not removed: %v", leftover)
	}
}

func TestSalvage_SparseCheckoutSnapshotKeepsFilesOutsideTheCone(t *testing.T) {
	t.Parallel()
	f := newSalvageFixture(t)
	taskDir, checkout := f.newTask("sparse")
	runGitForGC(t, checkout, "sparse-checkout", "set", "dir")
	mustNotExist(t, filepath.Join(checkout, "other", "c.txt"))
	writeSalvageFile(t, filepath.Join(checkout, "dir", "b.txt"), []byte("bee, edited\n"))

	if _, removed := f.d.cleanTaskDir(taskDir); !removed {
		t.Fatal("directory must be removed after salvage")
	}
	entries := f.entries()
	if len(entries) != 1 {
		t.Fatalf("want one bundle, got %d", len(entries))
	}
	clone := f.restore(entries[0])
	if got := blobAt(t, clone, "dir/b.txt"); string(got) != "bee, edited\n" {
		t.Fatalf("edit inside the cone lost: %q", got)
	}
	// A snapshot built from an empty index would list only the files on disk
	// and record every file outside the cone as deleted.
	if got := blobAt(t, clone, "other/c.txt"); string(got) != "sea\n" {
		t.Fatalf("file outside the sparse cone is missing from the snapshot: %q", got)
	}
	if got := blobAt(t, clone, "a.txt"); string(got) != "one\n" {
		t.Fatalf("root file changed by the snapshot: %q", got)
	}
}

func TestSalvage_NestedRepositoryIsSalvagedSeparately(t *testing.T) {
	t.Parallel()
	f := newSalvageFixture(t)
	taskDir, checkout := f.newTask("nested")
	writeSalvageFile(t, filepath.Join(checkout, "a.txt"), []byte("outer edit\n"))
	nested := filepath.Join(checkout, "vendor", "lib")
	runGitForGC(t, "", "init", "-b", "main", nested)
	writeSalvageFile(t, filepath.Join(nested, "lib.txt"), []byte("nested commit\n"))
	runGitForGC(t, nested, "add", "lib.txt")
	runGitForGC(t, nested, "commit", "-m", "nested work")
	// A second nested repository with no commit at all: `git add` on it fails,
	// so the outer snapshot must leave it out.
	runGitForGC(t, "", "init", "-b", "main", filepath.Join(checkout, "vendor", "empty"))
	writeSalvageFile(t, filepath.Join(checkout, "vendor", "empty", "wip.txt"), []byte("wip\n"))

	if _, removed := f.d.cleanTaskDir(taskDir); !removed {
		t.Fatal("directory must be removed after salvage")
	}
	byPath := map[string]salvageEntry{}
	for _, e := range f.entries() {
		byPath[e.m.RepoPath] = e
	}
	for _, want := range []string{"repo", "repo/vendor/lib", "repo/vendor/empty"} {
		if _, ok := byPath[want]; !ok {
			t.Fatalf("no bundle for checkout %q, have %v", want, keysOf(byPath))
		}
	}
	outer := f.restore(byPath["repo"])
	if got := blobAt(t, outer, "a.txt"); string(got) != "outer edit\n" {
		t.Fatalf("outer edit lost: %q", got)
	}
	if names := string(gitOutRaw(t, outer, "ls-tree", "-r", "--name-only", "FETCH_HEAD")); strings.Contains(names, "vendor/") {
		t.Fatalf("the outer snapshot must not contain the nested checkout, tree: %s", names)
	}

	inner := filepath.Join(t.TempDir(), "inner")
	runGitForGC(t, "", "init", "-b", "main", inner)
	runGitForGC(t, inner, "fetch", byPath["repo/vendor/lib"].bundle, byPath["repo/vendor/lib"].m.BundleRef)
	if got := blobAt(t, inner, "lib.txt"); string(got) != "nested commit\n" {
		t.Fatalf("nested commit lost: %q", got)
	}
	emptyClone := filepath.Join(t.TempDir(), "empty")
	runGitForGC(t, "", "init", "-b", "main", emptyClone)
	runGitForGC(t, emptyClone, "fetch", byPath["repo/vendor/empty"].bundle, byPath["repo/vendor/empty"].m.BundleRef)
	if got := blobAt(t, emptyClone, "wip.txt"); string(got) != "wip\n" {
		t.Fatalf("unborn nested checkout's file lost: %q", got)
	}
}

func keysOf(m map[string]salvageEntry) []string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestSalvage_OrphanRemovalIsSalvagedToo(t *testing.T) {
	t.Parallel()
	f := newSalvageFixture(t)
	// No .gc_meta.json: the orphan path decides this directory.
	taskDir := createTaskDir(t, f.root, "ws1", "issue-orph", nil)
	checkout := filepath.Join(taskDir, "workdir", "repo")
	runGitForGC(t, "", "-C", f.cache, "worktree", "add", "-b", "agent/orph/1", checkout, "refs/remotes/origin/main")
	writeSalvageFile(t, filepath.Join(checkout, "a.txt"), []byte("orphaned edit\n"))

	stats := &gcStats{byPattern: map[string]int{}}
	if removed := f.d.applyGCAction(taskDir, gcActionOrphan, stats); removed != 1 {
		t.Fatalf("orphan action removed %d directories, want 1", removed)
	}
	mustNotExist(t, taskDir)
	entries := f.entries()
	if len(entries) != 1 {
		t.Fatalf("orphan removal must salvage, got %d bundles", len(entries))
	}
	if got := blobAt(t, f.restore(entries[0]), "a.txt"); string(got) != "orphaned edit\n" {
		t.Fatalf("restored orphan edit = %q", got)
	}
}

func TestSalvage_DirectoryWithoutGitCheckoutBehavesAsBefore(t *testing.T) {
	t.Parallel()
	f := newSalvageFixture(t)
	taskDir := createTaskDir(t, f.root, "ws1", "issue-plain", nil)
	writeSalvageFile(t, filepath.Join(taskDir, "workdir", "notes.txt"), []byte("not a repo\n"))

	if _, removed := f.d.cleanTaskDir(taskDir); !removed {
		t.Fatal("a directory with no checkout must be removed as before")
	}
	mustNotExist(t, f.salvageDirPath())
}

func TestSalvage_DisabledBehavesLikeUpstream(t *testing.T) {
	t.Parallel()
	f := newSalvageFixture(t)
	f.d.cfg.GCSalvageEnabled = false
	taskDir, checkout := f.newTask("off")
	writeSalvageFile(t, filepath.Join(checkout, "a.txt"), []byte("lost on purpose\n"))

	if _, removed := f.d.cleanTaskDir(taskDir); !removed {
		t.Fatal("with salvage off the directory is removed without inspection")
	}
	mustNotExist(t, f.salvageDirPath())
	mustNotExist(t, f.salvageDirPath())
}

func TestSalvage_TotalCapStopsSalvageAndEvictsNothing(t *testing.T) {
	t.Parallel()
	f := newSalvageFixture(t)
	if err := os.MkdirAll(f.salvageDirPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	older := filepath.Join(f.salvageDirPath(), "older.bundle")
	writeSalvageFile(t, older, randomBytes(t, 4096))
	f.d.cfg.GCSalvageTotalMaxBytes = 4096 // exactly full

	taskDir, checkout := f.newTask("full")
	writeSalvageFile(t, filepath.Join(checkout, "a.txt"), []byte("needs salvage\n"))
	if _, removed := f.d.cleanTaskDir(taskDir); removed {
		t.Fatal("with .salvage full the directory must be kept")
	}
	mustExist(t, older) // nothing is evicted early
	mustExist(t, filepath.Join(checkout, "a.txt"))
	if got := len(f.entries()); got != 0 {
		t.Fatalf("no new bundle may be written, got %d", got)
	}
}

func TestSalvage_KeptDirectoryWarnsOncePerDay(t *testing.T) {
	t.Parallel()
	f := newSalvageFixture(t)
	f.d.cfg.GCSalvageMaxBytes = 16
	taskDir, checkout := f.newTask("noisy")
	writeSalvageFile(t, filepath.Join(checkout, "big.bin"), randomBytes(t, 4096))
	past := time.Now().Add(-200 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(taskDir, past, past); err != nil {
		t.Fatal(err)
	}
	warnings := func() int { return strings.Count(f.logs.String(), "level=WARN msg=\"gc: keeping task directory") }

	for i := 0; i < 3; i++ {
		if _, removed := f.d.cleanTaskDir(taskDir); removed {
			t.Fatal("over-cap directory must be kept")
		}
	}
	if got := warnings(); got != 1 {
		t.Fatalf("3 kept cycles logged %d warnings, want 1 (first failure only)", got)
	}
	var state salvageState
	data, err := os.ReadFile(filepath.Join(taskDir, salvageStateFile))
	if err != nil {
		t.Fatalf("counter file missing: %v", err)
	}
	if err := json.Unmarshal(data, &state); err != nil || state.Failures != 3 {
		t.Fatalf("counter = %+v (err %v), want 3 failures", state, err)
	}
	info, err := os.Stat(taskDir)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(past) {
		t.Fatalf("the counter file moved the directory mtime to %s: it would restart the orphan clock", info.ModTime())
	}

	// A day later the warning comes back.
	state.LastWarned = time.Now().Add(-25 * time.Hour)
	data, _ = json.Marshal(state)
	if err := os.WriteFile(filepath.Join(taskDir, salvageStateFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
	f.d.cleanTaskDir(taskDir)
	if got := warnings(); got != 2 {
		t.Fatalf("after 25h the warning must repeat, total warnings = %d, want 2", got)
	}
}

// salvageName builds a file name the way salvageCheckout and bundleBranch do.
func salvageName(parts, nonce, ext string) string {
	return time.Now().UTC().Format(salvageStampLayout) + "_" + parts + "-" + nonce + ext
}

func TestSalvage_SweepRemovesScratchAndExpiredFilesOnly(t *testing.T) {
	t.Parallel()
	f := newSalvageFixture(t)
	dir := f.salvageDirPath()
	old := time.Now().Add(-200 * time.Hour)
	fresh := time.Now().Add(-1 * time.Hour)
	mk := func(name string, mtime time.Time) string {
		p := filepath.Join(dir, name)
		writeSalvageFile(t, p, []byte("x"))
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		return p
	}
	oldBundle := mk(salvageName("ws1_task_repo", "aaaaaaaaaaaa", ".bundle"), old)
	oldManifest := mk(salvageName("ws1_task_repo", "aaaaaaaaaaaa", ".json"), old)
	newBundle := mk(salvageName("ws1_task_repo", "bbbbbbbbbbbb", ".bundle"), fresh)
	newManifest := mk(salvageName("ws1_task_repo", "bbbbbbbbbbbb", ".json"), fresh)
	// The manifest was rewritten recently: the pair is as young as its newer half.
	mixedBundle := mk(salvageName("branch_cache.git_agent_x", "cccccccccccc", ".bundle"), old)
	mixedManifest := mk(salvageName("branch_cache.git_agent_x", "cccccccccccc", ".json"), fresh)
	halfWritten := mk(salvageName("ws1_task_repo", "dddddddddddd", ".bundle.tmp"), fresh)
	scratch := filepath.Join(dir, salvageTmpPrefix+"deadbeef0123")
	writeSalvageFile(t, filepath.Join(scratch, "repo", "HEAD"), []byte("x"))
	// Files an operator dropped in: none matches the salvage naming scheme.
	operatorFiles := []string{
		mk("operator-notes.txt", old),
		mk("notes.tmp", old),
		mk("notes.json", old),
		mk("notes.bundle", old),
		mk("notes.json.tmp", old),
	}
	operatorDir := filepath.Join(dir, salvageTmpPrefix+"keep")
	writeSalvageFile(t, filepath.Join(operatorDir, "x"), []byte("x"))

	f.d.sweepSalvage()

	for _, gone := range []string{oldBundle, oldManifest, halfWritten, scratch} {
		mustNotExist(t, gone)
	}
	for _, kept := range append([]string{newBundle, newManifest, mixedBundle, mixedManifest, operatorDir}, operatorFiles...) {
		mustExist(t, kept)
	}

	// TTL 0 keeps every bundle.
	f.d.cfg.GCSalvageTTL = 0
	oldAgain := mk(salvageName("ws1_task_repo", "eeeeeeeeeeee", ".bundle"), old)
	f.d.sweepSalvage()
	mustExist(t, oldAgain)
}

// The names the sweep matches must be the names salvage writes, or expired
// bundles would never be removed.
func TestSalvage_SweepMatchesTheNamesSalvageWrites(t *testing.T) {
	t.Parallel()
	f := newSalvageFixture(t)
	taskDir, checkout := f.newTask("sweepname")
	writeSalvageFile(t, filepath.Join(checkout, "a.txt"), []byte("edited\n"))
	if _, removed := f.d.cleanTaskDir(taskDir); !removed {
		t.Fatal("salvage must succeed")
	}
	f.commitOnBranch("agent/sweepname/2", "u.txt", "u\n")
	f.d.pruneWorktree(f.cache)
	files, _ := filepath.Glob(filepath.Join(f.salvageDirPath(), "*"))
	if len(files) != 4 {
		t.Fatalf("want a bundle and manifest for the checkout and for the branch, got %v", files)
	}
	old := time.Now().Add(-200 * time.Hour)
	for _, file := range files {
		if !salvageFileName.MatchString(filepath.Base(file)) {
			t.Fatalf("salvage wrote %s, which the sweep does not recognise", filepath.Base(file))
		}
		if err := os.Chtimes(file, old, old); err != nil {
			t.Fatal(err)
		}
	}
	f.d.sweepSalvage()
	f.noSalvageOutput()
}

// The salvage directory sits beside the workspaces. No scan may treat it as a
// workspace, or GC would delete the bundles it just wrote.
func TestSalvage_DirectoryIsNeverScannedAsTaskRoot(t *testing.T) {
	t.Parallel()
	f := newSalvageFixture(t)
	f.d.cfg.GCOrphanTTL = 0 // any directory the scan reaches is old enough to remove
	stray := createTaskDir(t, f.root, salvageDirName, "issue-old", nil)
	writeSalvageFile(t, filepath.Join(stray, "workdir", "keep.txt"), randomBytes(t, 512))
	past := time.Now().Add(-500 * time.Hour)
	if err := os.Chtimes(stray, past, past); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(f.salvageDirPath(), "kept.bundle")
	writeSalvageFile(t, bundle, randomBytes(t, 2048))

	f.d.runGC(context.Background())

	mustExist(t, stray)
	mustExist(t, bundle)

	report, err := ScanDiskUsage(f.root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if report.TotalWorkspaceCount != 0 || report.TotalTaskCount != 0 {
		t.Fatalf("disk usage counted .salvage as a workspace: %d workspaces, %d tasks", report.TotalWorkspaceCount, report.TotalTaskCount)
	}
	if report.SalvageSizeBytes < 2048 || report.SalvageCount != 1 {
		t.Fatalf("disk usage must report the salvage footprint: bytes=%d count=%d", report.SalvageSizeBytes, report.SalvageCount)
	}
}

// git status can start a filesystem monitor from repository config. The
// repository content belongs to an agent, so salvage must not run it.
func TestSalvage_DoesNotRunConfiguredFsmonitor(t *testing.T) {
	t.Parallel()
	f := newSalvageFixture(t)
	taskDir, checkout := f.newTask("fsmon")
	writeSalvageFile(t, filepath.Join(checkout, "a.txt"), []byte("edited\n"))
	marker := filepath.Join(t.TempDir(), "fsmonitor-ran")
	hook := filepath.Join(t.TempDir(), "fsmonitor.sh")
	writeSalvageFile(t, hook, []byte("#!/bin/sh\ntouch '"+marker+"'\nexit 1\n"))
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitForGC(t, checkout, "config", "core.fsmonitor", hook)

	// Positive control: a plain git status does run the hook, so an absent
	// marker below means salvage suppressed it, not that the hook never fires.
	_, _ = exec.Command("git", "-C", checkout, "status", "--porcelain").CombinedOutput()
	mustExist(t, marker)
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}

	owner := execenv.EnvRootOwner{WorkspaceID: "ws1", TaskID: "fsmon"}
	if !f.d.salvageTaskDir(context.Background(), taskDir, owner) {
		t.Fatal("salvage must succeed")
	}
	mustNotExist(t, marker)
}

// Not parallel: it sets GIT_CONFIG_GLOBAL for the whole process.
func TestSalvage_IgnoresConfiguredHooksPath(t *testing.T) {
	f := newSalvageFixture(t)
	taskDir, checkout := f.newTask("hooks")
	writeSalvageFile(t, filepath.Join(checkout, "a.txt"), []byte("edited\n"))

	hooksDir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "hook-ran")
	hook := filepath.Join(hooksDir, "reference-transaction")
	writeSalvageFile(t, hook, []byte("#!/bin/sh\ntouch '"+marker+"'\n"))
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
	globalConfig := filepath.Join(t.TempDir(), "gitconfig")
	writeSalvageFile(t, globalConfig, []byte("[core]\n\thooksPath = "+hooksDir+"\n"))
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)

	// Positive control: a ref update outside salvage fires the configured hook.
	control := filepath.Join(t.TempDir(), "control")
	runGitForGC(t, "", "init", "-q", control)
	runGitForGC(t, control, "commit", "--allow-empty", "-m", "control")
	if _, err := os.Stat(marker); err != nil {
		t.Skipf("this git does not run reference-transaction hooks: %v", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}

	owner := execenv.EnvRootOwner{WorkspaceID: "ws1", TaskID: "hooks"}
	if !f.d.salvageTaskDir(context.Background(), taskDir, owner) {
		t.Fatal("salvage must succeed")
	}
	mustNotExist(t, marker)
}

func TestSalvage_ScrubsGitEnvironmentThatRedirectsRepositories(t *testing.T) {
	f := newSalvageFixture(t)
	taskDir, checkout := f.newTask("gitdir")
	writeSalvageFile(t, filepath.Join(checkout, "a.txt"), []byte("edited\n"))
	// A daemon started from a shell with GIT_DIR exported would inspect the
	// wrong repository, see it clean, and let the removal go ahead.
	t.Setenv("GIT_DIR", f.remote)

	if _, removed := f.d.cleanTaskDir(taskDir); !removed {
		t.Fatal("directory must be removed after salvage")
	}
	entries := f.entries()
	if len(entries) != 1 || entries[0].m.DirtyFilesTotal != 1 {
		t.Fatalf("GIT_DIR in the daemon environment must not redirect salvage, got %+v", entries)
	}
}

func TestParseStatusZ(t *testing.T) {
	t.Parallel()
	out := " M a.txt\x00?? dir with space/new.txt\x00R  new-name.txt\x00old-name.txt\x00D  gone.txt\x00"
	got := parseStatusZ(out)
	want := []statusEntry{
		{Status: " M", Path: "a.txt"},
		{Status: "??", Path: "dir with space/new.txt"},
		{Status: "R ", Path: "new-name.txt"},
		{Status: "D ", Path: "gone.txt"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("parseStatusZ = %v, want %v", got, want)
	}
}

func TestRequiredCommitsFromVerify(t *testing.T) {
	t.Parallel()
	sha1 := strings.Repeat("a", 40)
	sha2 := strings.Repeat("b", 40)
	out := "The bundle contains this ref:\n" + strings.Repeat("c", 40) + " refs/multica-salvage/x\nThe bundle requires these 2 refs:\n" + sha1 + " subject one\n" + sha2 + "\nThe bundle uses this hash algorithm: sha1\nbundle.bundle is okay\n"
	got := requiredCommitsFromVerify(out)
	if len(got) != 2 || got[0] != sha1 || got[1] != sha2 {
		t.Fatalf("requiredCommitsFromVerify = %v", got)
	}
	if got := requiredCommitsFromVerify("The bundle records a complete history.\n"); len(got) != 0 {
		t.Fatalf("a complete-history bundle has no prerequisites, got %v", got)
	}
}

func TestSalvage_UnreadableDirectoryKeepsTaskDir(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 directory")
	}
	f := newSalvageFixture(t)
	taskDir, checkout := f.newTask("unreadable")
	writeSalvageFile(t, filepath.Join(checkout, "a.txt"), []byte("edited\n"))
	locked := filepath.Join(taskDir, "workdir", "locked")
	writeSalvageFile(t, filepath.Join(locked, "maybe-a-repo.txt"), []byte("?\n"))
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	if _, removed := f.d.cleanTaskDir(taskDir); removed {
		t.Fatal("a directory the search could not read may hold a checkout: keep the task dir")
	}
	mustExist(t, filepath.Join(checkout, "a.txt"))
	f.noSalvageOutput()
	if !strings.Contains(f.logs.String(), "search for git checkouts failed") {
		t.Fatalf("the walk error must be the reason logged, log = %s", f.logs.String())
	}
}

func TestSalvage_InspectFailureKeepsTaskDir(t *testing.T) {
	t.Parallel()
	f := newSalvageFixture(t)
	taskDir := createTaskDir(t, f.root, "ws1", "issue-broken", nil)
	broken := filepath.Join(taskDir, "workdir", "repo")
	writeSalvageFile(t, filepath.Join(broken, "work.txt"), []byte("unsaved\n"))
	// A .git file naming a git dir that is gone: every git call in it fails.
	writeSalvageFile(t, filepath.Join(broken, ".git"), []byte("gitdir: "+filepath.Join(t.TempDir(), "gone")+"\n"))

	if _, removed := f.d.cleanTaskDir(taskDir); removed {
		t.Fatal("a checkout that could not be inspected must keep the task dir")
	}
	mustExist(t, filepath.Join(broken, "work.txt"))
	if !strings.Contains(f.logs.String(), "inspect checkout failed") {
		t.Fatalf("the inspect error must be the reason logged, log = %s", f.logs.String())
	}
}

// A checkout whose .git is a symlink keeps its working tree inside the task
// directory, so deleting the directory would lose those edits.
func TestSalvage_SymlinkedGitKeepsTaskDir(t *testing.T) {
	t.Parallel()
	f := newSalvageFixture(t)
	external := filepath.Join(t.TempDir(), "external")
	runGitForGC(t, "", "init", "-b", "main", external)
	taskDir := createTaskDir(t, f.root, "ws1", "issue-symlink", nil)
	checkout := filepath.Join(taskDir, "workdir", "repo")
	writeSalvageFile(t, filepath.Join(checkout, "edit.txt"), []byte("only here\n"))
	if err := os.Symlink(filepath.Join(external, ".git"), filepath.Join(checkout, ".git")); err != nil {
		t.Fatal(err)
	}

	if _, removed := f.d.cleanTaskDir(taskDir); removed {
		t.Fatal("a symlinked .git must keep the task dir, not read as no checkout")
	}
	mustExist(t, filepath.Join(checkout, "edit.txt"))
	if !strings.Contains(f.logs.String(), "symlink") {
		t.Fatalf("the kept reason must name the symlink, log = %s", f.logs.String())
	}
}

// Not parallel: sets a process environment variable.
func TestSalvage_GitRunsInTheCLocale(t *testing.T) {
	t.Setenv("LC_ALL", "de_DE.UTF-8")
	effective := ""
	for _, kv := range salvageBaseEnv() {
		if v, ok := strings.CutPrefix(kv, "LC_ALL="); ok {
			effective = v // exec uses the last value of a duplicated key
		}
	}
	if effective != "C" {
		t.Fatalf("salvage git must run with LC_ALL=C so bundle verify output parses, got %q", effective)
	}
}

func TestSalvage_HasRoomFailsClosedWhenSizingIsCancelled(t *testing.T) {
	t.Parallel()
	f := newSalvageFixture(t)
	writeSalvageFile(t, filepath.Join(f.salvageDirPath(), "x.bundle"), []byte("x"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.d.salvageHasRoom(ctx); err == nil {
		t.Fatal("a .salvage size that could not be measured must not read as room")
	}
	if err := f.d.salvageHasRoom(context.Background()); err != nil {
		t.Fatalf("an almost empty .salvage has room: %v", err)
	}
}

func TestSalvage_RunGCSweepsSalvageDirectory(t *testing.T) {
	t.Parallel()
	f := newSalvageFixture(t)
	scratch := filepath.Join(f.salvageDirPath(), salvageTmpPrefix+"0123456789ab")
	writeSalvageFile(t, filepath.Join(scratch, "repo", "HEAD"), []byte("x"))

	f.d.runGC(context.Background())

	mustNotExist(t, scratch)
}
