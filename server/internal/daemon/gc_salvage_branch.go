package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/repocache"
)

// salvageWarnLimiter lets a repeating failure log once per interval per key.
// The zero value is ready to use.
type salvageWarnLimiter struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func (l *salvageWarnLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last == nil {
		l.last = make(map[string]time.Time)
	}
	if prev, ok := l.last[key]; ok && now.Sub(prev) < salvageWarnInterval {
		return false
	}
	l.last[key] = now
	return true
}

// salvageAgentBranch reports whether pruneWorktreeLocked may delete branch. A
// branch whose commits all sit on origin is deleted as before. Otherwise its
// unpushed commits are bundled first, and the branch stays if that fails.
func (d *Daemon) salvageAgentBranch(ctx context.Context, barePath, branch string) bool {
	if !d.cfg.GCSalvageEnabled {
		return true
	}
	ref := "refs/heads/" + branch
	unpushed, err := repocache.CountCommitsNotOnRemote(ctx, barePath, ref, salvageOriginRemote)
	if err != nil {
		if ctx.Err() == nil {
			d.warnBranchKept(barePath, branch, "count unpushed commits failed", err)
		}
		return false
	}
	if unpushed == 0 {
		return true
	}
	if err := d.salvageHasRoom(ctx); err != nil {
		d.warnBranchKept(barePath, branch, "salvage directory is full", err)
		return false
	}
	if err := d.bundleBranch(ctx, barePath, branch, ref, unpushed); err != nil {
		if ctx.Err() == nil {
			d.warnBranchKept(barePath, branch, "salvage failed", err)
		}
		return false
	}
	return true
}

func (d *Daemon) bundleBranch(ctx context.Context, barePath, branch, ref string, unpushed int) error {
	head, err := runSalvageGit(ctx, barePath, nil, "", "rev-parse", "--verify", ref)
	if err != nil {
		return err
	}
	head = strings.TrimSpace(head)
	nonce, err := randomNonce()
	if err != nil {
		return err
	}
	salvageRoot := d.salvageDir()
	if err := os.MkdirAll(salvageRoot, 0o700); err != nil {
		return fmt.Errorf("create salvage directory: %w", err)
	}
	stamp := time.Now().UTC().Format(salvageStampLayout)
	base := stamp + "_branch_" + salvageSafeName(filepath.Base(barePath)) + "_" + salvageSafeName(branch) + "-" + nonce
	bundlePath := filepath.Join(salvageRoot, base+".bundle")
	size, prereqs, err := d.createVerifiedBundle(ctx, barePath, ref, bundlePath)
	if err != nil {
		return err
	}
	manifest := salvageManifest{
		Version:         salvageManifestVersion,
		Kind:            salvageKindBranch,
		CreatedAt:       time.Now().UTC(),
		RepoPath:        barePath,
		Branch:          branch,
		HeadSHA:         head,
		BundleFile:      filepath.Base(bundlePath),
		BundleBytes:     size,
		BundleRef:       ref,
		Prerequisites:   prereqs,
		RestoreHint:     "git fetch " + filepath.Base(bundlePath) + " " + ref + " (run inside a clone that has the prerequisite commits)",
		UnpushedCommits: unpushed,
		Notes:           []string{"The bundle holds the commits of this branch that origin does not have. It was written before the branch was deleted from the repository cache."},
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		os.Remove(bundlePath)
		return fmt.Errorf("encode manifest: %w", err)
	}
	if err := publishSalvageFile(filepath.Join(salvageRoot, base+".json"), data); err != nil {
		os.Remove(bundlePath)
		return fmt.Errorf("write manifest: %w", err)
	}
	d.logger.Info("gc: salvaged unpushed branch before deleting it",
		"repo", barePath, "branch", branch, "bundle", bundlePath, "bundle_bytes", size, "unpushed_commits", unpushed)
	return nil
}

func (d *Daemon) warnBranchKept(barePath, branch, reason string, cause error) {
	if !d.salvageWarn.allow(barePath+"\x00"+branch, time.Now()) {
		return
	}
	d.logger.Warn("gc: keeping agent branch, its unpushed commits could not be salvaged",
		"repo", barePath, "branch", branch, "reason", reason, "error", cause)
}
