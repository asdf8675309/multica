package daemon

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

func newTerminalArtifactDaemon(t *testing.T, artifactTTL, terminalTTL time.Duration) *Daemon {
	t.Helper()
	d := newGCTestDaemon(t, http.NewServeMux())
	d.cfg.GCArtifactTTL = artifactTTL
	d.cfg.GCTerminalArtifactTTL = terminalTTL
	return d
}

func issueResult(status string, updatedAgo time.Duration) IssueGCCheckResult {
	return IssueGCCheckResult{ID: "iss", Found: true, Status: status, UpdatedAt: time.Now().Add(-updatedAgo)}
}

// A done or cancelled card clears its artifacts on the short terminal TTL. An
// open card keeps the long one, so a short terminal TTL never reaches in_review.
func TestGCDecision_TerminalCardUsesTerminalArtifactTTL(t *testing.T) {
	t.Parallel()
	meta := func() *execenv.GCMeta {
		return &execenv.GCMeta{IssueID: "iss", WorkspaceID: "ws", CompletedAt: time.Now().Add(-time.Hour)}
	}
	const (
		artifactTTL = 4 * time.Hour
		terminalTTL = 30 * time.Minute
	)
	cases := []struct {
		name   string
		status string
		want   gcAction
	}{
		{"done card completed 1h ago is past the 30m terminal TTL", "done", gcActionCleanArtifacts},
		{"cancelled card is terminal too", "cancelled", gcActionCleanArtifacts},
		{"in_review card keeps the 4h TTL", "in_review", gcActionSkip},
		{"in_progress card keeps the 4h TTL", "in_progress", gcActionSkip},
		{"blocked card keeps the 4h TTL", "blocked", gcActionSkip},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newTerminalArtifactDaemon(t, artifactTTL, terminalTTL)
			taskDir := createTaskDir(t, d.cfg.WorkspacesRoot, "ws", "issue-abc", meta())
			// Updated 10 minutes ago: inside GCTTL, so no whole-directory removal.
			got := d.gcDecisionIssueResult(taskDir, meta(), issueResult(tc.status, 10*time.Minute))
			if got != tc.want {
				t.Fatalf("status %q: action = %d, want %d", tc.status, got, tc.want)
			}
		})
	}
}

// Unset means "same as MULTICA_GC_ARTIFACT_TTL", so an operator who sets
// nothing sees today's behavior.
func TestGCDecision_UnsetTerminalArtifactTTLKeepsUpstreamBehavior(t *testing.T) {
	t.Parallel()
	d := newTerminalArtifactDaemon(t, 4*time.Hour, 0)
	meta := &execenv.GCMeta{IssueID: "iss", WorkspaceID: "ws", CompletedAt: time.Now().Add(-time.Hour)}
	taskDir := createTaskDir(t, d.cfg.WorkspacesRoot, "ws", "issue-abc", meta)

	if got := d.gcDecisionIssueResult(taskDir, meta, issueResult("done", 10*time.Minute)); got != gcActionSkip {
		t.Fatalf("done card at 1h with a 4h artifact TTL and no terminal TTL: action = %d, want skip", got)
	}
	meta.CompletedAt = time.Now().Add(-5 * time.Hour)
	if got := d.gcDecisionIssueResult(taskDir, meta, issueResult("done", 10*time.Minute)); got != gcActionCleanArtifacts {
		t.Fatalf("done card at 5h: action = %d, want artifact cleanup", got)
	}
}

// MULTICA_GC_ARTIFACT_TTL=0 disables artifact cleanup for every card. A terminal
// TTL must not switch it back on.
func TestGCDecision_ZeroArtifactTTLStillDisablesTerminalCleanup(t *testing.T) {
	t.Parallel()
	d := newTerminalArtifactDaemon(t, 0, 30*time.Minute)
	meta := &execenv.GCMeta{IssueID: "iss", WorkspaceID: "ws", CompletedAt: time.Now().Add(-5 * time.Hour)}
	taskDir := createTaskDir(t, d.cfg.WorkspacesRoot, "ws", "issue-abc", meta)

	if got := d.gcDecisionIssueResult(taskDir, meta, issueResult("done", 10*time.Minute)); got != gcActionSkip {
		t.Fatalf("action = %d, want skip while MULTICA_GC_ARTIFACT_TTL is 0", got)
	}
}

// Terminal cards come back every cycle until GCTTL removes the directory. The
// marker stops a 15-minute interval from walking each of them every time.
func TestGCDecision_ArtifactMarkerSkipsSecondWalk(t *testing.T) {
	t.Parallel()
	d := newTerminalArtifactDaemon(t, 4*time.Hour, 30*time.Minute)
	completed := time.Now().Add(-time.Hour)
	meta := &execenv.GCMeta{IssueID: "iss", WorkspaceID: "ws", CompletedAt: completed}
	taskDir := createTaskDir(t, d.cfg.WorkspacesRoot, "ws", "issue-abc", meta)
	writeFile(t, filepath.Join(taskDir, "workdir", "repo", "node_modules", "pkg", "index.js"), 256)
	writeFile(t, filepath.Join(taskDir, "workdir", "repo", "src", "main.ts"), 64)
	result := issueResult("done", 10*time.Minute)

	first := d.gcDecisionIssueResult(taskDir, meta, result)
	if first != gcActionCleanArtifacts {
		t.Fatalf("first cycle: action = %d, want artifact cleanup", first)
	}
	stats := &gcStats{byPattern: map[string]int{}}
	d.applyGCAction(taskDir, first, stats)
	if stats.artifactRemoved != 1 {
		t.Fatalf("artifact_removed = %d, want 1", stats.artifactRemoved)
	}
	assertGone(t, taskDir, "workdir/repo/node_modules")
	assertKept(t, taskDir, "workdir/repo/src/main.ts", artifactsCleanedMarker)

	if got := d.gcDecisionIssueResult(taskDir, meta, result); got != gcActionSkip {
		t.Fatalf("second cycle: action = %d, want skip, the marker says the walk already ran", got)
	}

	// The task ran again and completed after the marker was written: its
	// artifacts are new, so the walk is allowed again.
	marker := filepath.Join(taskDir, artifactsCleanedMarker)
	past := completed.Add(-time.Hour)
	if err := os.Chtimes(marker, past, past); err != nil {
		t.Fatal(err)
	}
	if got := d.gcDecisionIssueResult(taskDir, meta, result); got != gcActionCleanArtifacts {
		t.Fatalf("a completion newer than the marker: action = %d, want artifact cleanup", got)
	}
}

// Open cards are never skipped on the marker: they get the long TTL and the
// existing walk-every-cycle behavior.
func TestGCDecision_ArtifactMarkerDoesNotApplyToOpenCards(t *testing.T) {
	t.Parallel()
	d := newTerminalArtifactDaemon(t, 4*time.Hour, 30*time.Minute)
	meta := &execenv.GCMeta{IssueID: "iss", WorkspaceID: "ws", CompletedAt: time.Now().Add(-5 * time.Hour)}
	taskDir := createTaskDir(t, d.cfg.WorkspacesRoot, "ws", "issue-abc", meta)
	d.markArtifactsCleaned(taskDir)

	if got := d.gcDecisionIssueResult(taskDir, meta, issueResult("in_review", 10*time.Minute)); got != gcActionCleanArtifacts {
		t.Fatalf("open card with a marker: action = %d, want artifact cleanup", got)
	}
}

// orphanByMTime reads the task directory's mtime, so writing the marker must
// not reset the orphan clock.
func TestGCDecision_ArtifactMarkerKeepsTaskDirMTime(t *testing.T) {
	t.Parallel()
	d := newTerminalArtifactDaemon(t, 4*time.Hour, 30*time.Minute)
	meta := &execenv.GCMeta{IssueID: "iss", WorkspaceID: "ws", CompletedAt: time.Now().Add(-time.Hour)}
	taskDir := createTaskDir(t, d.cfg.WorkspacesRoot, "ws", "issue-abc", meta)
	writeFile(t, filepath.Join(taskDir, "workdir", "repo", "node_modules", "pkg", "index.js"), 256)
	past := time.Now().Add(-500 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(taskDir, past, past); err != nil {
		t.Fatal(err)
	}

	d.applyGCAction(taskDir, gcActionCleanArtifacts, &gcStats{byPattern: map[string]int{}})

	assertKept(t, taskDir, artifactsCleanedMarker)
	info, err := os.Stat(taskDir)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(past) {
		t.Fatalf("writing the marker moved the task dir mtime to %s, want %s", info.ModTime(), past)
	}
}
