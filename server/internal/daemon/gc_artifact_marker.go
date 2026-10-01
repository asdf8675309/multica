package daemon

import (
	"os"
	"path/filepath"
	"time"
)

// artifactsCleanedMarker records that an artifact walk finished after the
// task's last completion, so the next cycle does not walk the tree again.
const artifactsCleanedMarker = ".gc_artifacts_cleaned"

// artifactsAlreadyCleaned reports whether an artifact walk finished after the
// task's last completion. A task that runs again writes a new completed_at,
// which moves past the marker and allows the next walk.
func artifactsAlreadyCleaned(taskDir string, completedAt time.Time) bool {
	info, err := os.Stat(filepath.Join(taskDir, artifactsCleanedMarker))
	if err != nil {
		return false
	}
	return info.ModTime().After(completedAt)
}

// markArtifactsCleaned records a finished pattern walk. It runs after every
// gcActionCleanArtifacts, open card or done; artifactsAlreadyCleaned reads it
// only for done and cancelled cards. The managed-artifact action removes exact
// paths without a walk, so it has nothing to mark.
//
// Creating the marker would move taskDir's mtime, which orphanByMTime reads,
// so the mtime is put back.
func (d *Daemon) markArtifactsCleaned(taskDir string) {
	data := []byte(time.Now().UTC().Format(time.RFC3339Nano) + "\n")
	if err := writeFileKeepingDirMTime(taskDir, artifactsCleanedMarker, data); err != nil {
		d.logger.Debug("gc: write artifact marker failed", "dir", taskDir, "error", err)
	}
}

// writeFileKeepingDirMTime writes dir/name and restores dir's mtime, so GC
// bookkeeping inside a task directory does not restart its orphan clock.
func writeFileKeepingDirMTime(dir, name string, data []byte) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	writeErr := os.WriteFile(filepath.Join(dir, name), data, 0o600)
	if err := os.Chtimes(dir, time.Now(), info.ModTime()); err != nil && writeErr == nil {
		return err
	}
	return writeErr
}
