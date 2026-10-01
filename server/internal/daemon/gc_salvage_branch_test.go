package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// commitOnBranch adds a commit to a branch of the bare cache without a checkout.
func (f *salvageFixture) commitOnBranch(branch, file, content string) string {
	f.t.Helper()
	work := filepath.Join(f.t.TempDir(), "work")
	runGitForGC(f.t, "", "clone", f.cache, work)
	runGitForGC(f.t, work, "checkout", "-B", "tmp", "refs/remotes/origin/"+"main")
	writeSalvageFile(f.t, filepath.Join(work, file), []byte(content))
	runGitForGC(f.t, work, "add", file)
	runGitForGC(f.t, work, "commit", "-m", "branch work")
	runGitForGC(f.t, work, "push", f.cache, "tmp:refs/heads/"+branch, "--force")
	return strings.TrimSpace(string(gitOutRaw(f.t, f.cache, "rev-parse", "refs/heads/"+branch)))
}

func TestSalvage_BranchDeletionBundlesUnpushedCommitsFirst(t *testing.T) {
	t.Parallel()
	f := newSalvageFixture(t)
	runGitForGC(t, f.cache, "branch", "agent/pushed/1", "refs/remotes/origin/main")
	tip := f.commitOnBranch("agent/unpushed/1", "unpushed.txt", "only here\n")

	f.d.pruneWorktree(f.cache)

	if gitRefExists(t, f.cache, "refs/heads/agent/pushed/1") {
		t.Fatal("a branch whose commits are all on origin is deleted")
	}
	if gitRefExists(t, f.cache, "refs/heads/agent/unpushed/1") {
		t.Fatal("the branch is deleted once its bundle exists")
	}
	entries := f.entries()
	if len(entries) != 1 {
		t.Fatalf("want one bundle for the unpushed branch and none for the pushed one, got %d", len(entries))
	}
	m := entries[0].m
	if m.Kind != salvageKindBranch || m.Branch != "agent/unpushed/1" || m.HeadSHA != tip || m.UnpushedCommits != 1 {
		t.Fatalf("branch manifest wrong: %+v", m)
	}
	clone := f.restore(entries[0])
	if got := blobAt(t, clone, "unpushed.txt"); string(got) != "only here\n" {
		t.Fatalf("restored branch content = %q", got)
	}
}

func TestSalvage_BranchIsKeptWhenItsBundleFails(t *testing.T) {
	t.Parallel()
	f := newSalvageFixture(t)
	f.d.cfg.GCSalvageMaxBytes = 1 // no bundle fits
	f.commitOnBranch("agent/kept/1", "unpushed.txt", "only here\n")

	f.d.pruneWorktree(f.cache)

	if !gitRefExists(t, f.cache, "refs/heads/agent/kept/1") {
		t.Fatal("a branch whose bundle failed must stay: deleting it loses the commits")
	}
	f.noSalvageOutput()
	if !strings.Contains(f.logs.String(), "keeping agent branch") {
		t.Fatalf("a kept branch must be logged, log = %s", f.logs.String())
	}

	// A second cycle stays quiet: one warning per branch per day.
	before := strings.Count(f.logs.String(), "keeping agent branch")
	f.d.pruneWorktree(f.cache)
	if after := strings.Count(f.logs.String(), "keeping agent branch"); after != before {
		t.Fatalf("branch warning repeated within a day: %d -> %d", before, after)
	}
}

func TestSalvage_FullSalvageDirectoryKeepsUnpushedBranch(t *testing.T) {
	t.Parallel()
	f := newSalvageFixture(t)
	if err := os.MkdirAll(f.salvageDirPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	writeSalvageFile(t, filepath.Join(f.salvageDirPath(), "older.bundle"), randomBytes(t, 4096))
	f.d.cfg.GCSalvageTotalMaxBytes = 4096

	f.commitOnBranch("agent/fullcap/1", "unpushed.txt", "only here\n")
	f.d.pruneWorktree(f.cache)
	if !gitRefExists(t, f.cache, "refs/heads/agent/fullcap/1") {
		t.Fatal("a full .salvage must keep the unpushed branch too")
	}
}

func TestSalvage_DisabledDeletesBranchAsBefore(t *testing.T) {
	t.Parallel()
	f := newSalvageFixture(t)
	f.d.cfg.GCSalvageEnabled = false
	f.commitOnBranch("agent/off/branch", "unpushed.txt", "x\n")
	f.d.pruneWorktree(f.cache)
	if gitRefExists(t, f.cache, "refs/heads/agent/off/branch") {
		t.Fatal("with salvage off the stale branch is deleted as before")
	}
	mustNotExist(t, f.salvageDirPath())
}

// Eviction runs right after branch pruning under the same lock. A branch the
// prune kept because its bundle failed is the only copy of its commits, so
// removing the whole repo cache would lose exactly what the prune protected.
func TestSalvage_EvictionKeepsRepoWhenPruneKeptAnUnsavedBranch(t *testing.T) {
	t.Parallel()
	f := newSalvageFixture(t)
	f.d.cfg.GCSalvageMaxBytes = 1 // no bundle fits
	f.d.cfg.GCRepoTTL = time.Hour
	writeLastUsed(t, f.cache, time.Now().Add(-48*time.Hour))
	f.commitOnBranch("agent/kept/1", "unpushed.txt", "only here\n")

	stats := &gcStats{byPattern: map[string]int{}}
	f.d.maintainRepoCache(context.Background(), f.cache, stats)

	mustExist(t, f.cache)
	if !gitRefExists(t, f.cache, "refs/heads/agent/kept/1") {
		t.Fatal("the unsaved branch must survive the cycle")
	}
	if stats.repoCachesReclaimed != 0 || strings.Contains(f.logs.String(), "repo cache evicted") {
		t.Fatalf("repo cache evicted although it holds an unsaved branch: reclaimed=%d", stats.repoCachesReclaimed)
	}

	// Once the branch is saved the repo is evictable again.
	f.d.cfg.GCSalvageMaxBytes = 1 << 20
	f.d.maintainRepoCache(context.Background(), f.cache, stats)
	mustNotExist(t, f.cache)
	if len(f.entries()) != 1 {
		t.Fatalf("the branch must be bundled before the repo goes, got %d bundles", len(f.entries()))
	}
}

func TestSalvage_BranchIsKeptWhenUnpushedCountFails(t *testing.T) {
	t.Parallel()
	f := newSalvageFixture(t)
	// rev-list fails on a ref that does not resolve.
	if f.d.salvageAgentBranch(context.Background(), f.cache, "agent/missing/1") {
		t.Fatal("a failed unpushed-commit count must keep the branch")
	}
	if !strings.Contains(f.logs.String(), "count unpushed commits failed") {
		t.Fatalf("the count failure must be logged, log = %s", f.logs.String())
	}
}

// armEviction arms a salvage fixture so the next maintainRepoCache cycle
// would evict the repo unless pruning vetoes it.
func (f *salvageFixture) armEviction() *gcStats {
	f.t.Helper()
	f.d.cfg.GCRepoTTL = time.Hour
	writeLastUsed(f.t, f.cache, time.Now().Add(-48*time.Hour))
	return &gcStats{byPattern: map[string]int{}}
}

// A cycle that deletes one branch and keeps another is the case the single-
// branch test cannot reach: the early `!pending` return is skipped, so the
// verdict comes from the heavy-maintenance returns further down.
func TestSalvage_EvictionKeepsRepoWhenOneBranchIsDeletedAndAnotherKept(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f *salvageFixture)
	}{
		{"heavy maintenance completes", func(f *salvageFixture) {}},
		{"heavy maintenance disabled", func(f *salvageFixture) { f.d.cfg.GCRepoMaintenanceEnabled = false }},
		{"tasks are active", func(f *salvageFixture) { f.d.activeTasks.Add(1) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newSalvageFixture(t)
			tc.setup(f)
			f.d.cfg.GCSalvageMaxBytes = 16 << 10 // only the large branch exceeds it
			f.commitOnBranch("agent/saved/1", "small.txt", "small\n")
			f.commitOnBranch("agent/big/1", "big.bin", string(randomBytes(t, 64<<10)))
			stats := f.armEviction()

			f.d.maintainRepoCache(context.Background(), f.cache, stats)

			if gitRefExists(t, f.cache, "refs/heads/agent/saved/1") {
				t.Fatal("the small branch must be bundled and deleted, otherwise this test does not reach the deleted>0 paths")
			}
			if !gitRefExists(t, f.cache, "refs/heads/agent/big/1") {
				t.Fatal("the branch whose bundle exceeds the cap must be kept")
			}
			mustExist(t, f.cache)
			if stats.repoCachesReclaimed != 0 || strings.Contains(f.logs.String(), "repo cache evicted") {
				t.Fatalf("repo cache evicted although it holds an unsaved branch: reclaimed=%d", stats.repoCachesReclaimed)
			}

			// Control: with the cap lifted the same repo is evictable, so the
			// survival above came from the kept branch and nothing else.
			f.d.cfg.GCSalvageMaxBytes = 1 << 20
			f.d.maintainRepoCache(context.Background(), f.cache, stats)
			mustNotExist(t, f.cache)
		})
	}
}

// The agent-branch scan reads packed-refs, and `worktree list` does not when
// refs/heads/main is also a loose ref, so a corrupt packed-refs fails only the
// agent-branch scan. Eviction's own worktree count still works, which is what
// makes the prune verdict, not a later git failure, the thing keeping the repo.
func TestSalvage_EvictionKeepsRepoWhenTheAgentBranchScanFails(t *testing.T) {
	t.Parallel()
	f := newSalvageFixture(t)
	stats := f.armEviction()
	packed := filepath.Join(f.cache, "packed-refs")
	original, err := os.ReadFile(packed)
	if err != nil {
		t.Fatal(err)
	}
	// update-ref with an unchanged value writes nothing, so write the loose ref directly.
	sha := strings.TrimSpace(string(gitOutRaw(t, f.cache, "rev-parse", "refs/heads/main")))
	writeSalvageFile(t, filepath.Join(f.cache, "refs", "heads", "main"), []byte(sha+"\n"))
	if err := os.WriteFile(packed, append(append([]byte{}, original...), "garbage line\n"...), 0o644); err != nil {
		t.Fatal(err)
	}

	f.d.maintainRepoCache(context.Background(), f.cache, stats)

	if !strings.Contains(f.logs.String(), "agent branch scan failed") {
		t.Fatalf("the agent-branch scan must be the one that failed; log = %s", f.logs.String())
	}
	mustExist(t, f.cache)
	if stats.repoCachesReclaimed != 0 || strings.Contains(f.logs.String(), "repo cache evicted") {
		t.Fatalf("repo cache evicted although its branches could not be listed: reclaimed=%d", stats.repoCachesReclaimed)
	}

	// Control: once the scan works the same repo is evictable.
	if err := os.WriteFile(packed, original, 0o644); err != nil {
		t.Fatal(err)
	}
	f.d.maintainRepoCache(context.Background(), f.cache, stats)
	mustNotExist(t, f.cache)
}

// Eviction counts worktrees with the same `worktree list` the prune scan uses,
// so a persistent failure would stop eviction by itself and hide the prune
// verdict. A git that fails the first `worktree list` and then behaves lets the
// prune verdict stand alone. Uses t.Setenv, so it cannot run in parallel.
func TestSalvage_EvictionKeepsRepoWhenTheWorktreeBranchScanFailsOnce(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("requires a POSIX shell: %v", err)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not found: %v", err)
	}
	f := newSalvageFixture(t)
	stats := f.armEviction()

	flag := filepath.Join(t.TempDir(), "already-failed")
	fakeGit := filepath.Join(t.TempDir(), "git")
	script := `#!/bin/sh
if [ "$3" = "worktree" ] && [ "$4" = "list" ] && [ ! -e "$MULTICA_TEST_FAIL_ONCE" ]; then
  : > "$MULTICA_TEST_FAIL_ONCE"
  echo "fatal: injected worktree list failure" >&2
  exit 128
fi
exec "$MULTICA_TEST_REAL_GIT" "$@"
`
	if err := os.WriteFile(fakeGit, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MULTICA_TEST_REAL_GIT", realGit)
	t.Setenv("MULTICA_TEST_FAIL_ONCE", flag)
	t.Setenv("PATH", filepath.Dir(fakeGit)+string(os.PathListSeparator)+os.Getenv("PATH"))

	f.d.maintainRepoCache(context.Background(), f.cache, stats)

	if !strings.Contains(f.logs.String(), "worktree branch scan failed") {
		t.Fatalf("the worktree-branch scan must be the one that failed; log = %s", f.logs.String())
	}
	mustExist(t, f.cache)
	if stats.repoCachesReclaimed != 0 || strings.Contains(f.logs.String(), "repo cache evicted") {
		t.Fatalf("repo cache evicted although its branches could not be listed: reclaimed=%d", stats.repoCachesReclaimed)
	}

	// Control: the injected failure is spent, so the next cycle may evict.
	f.d.maintainRepoCache(context.Background(), f.cache, stats)
	mustNotExist(t, f.cache)
}
