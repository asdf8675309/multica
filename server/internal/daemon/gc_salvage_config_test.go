package daemon

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Not parallel: LoadConfig reads process environment variables.
func TestLoadConfig_GCSalvageSettings(t *testing.T) {
	stageFakeAgent(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SHELL", filepath.Join(t.TempDir(), "missing-shell"))
	for _, key := range []string{
		"MULTICA_GC_TERMINAL_ARTIFACT_TTL", "MULTICA_GC_SALVAGE", "MULTICA_GC_SALVAGE_MAX_MB",
		"MULTICA_GC_SALVAGE_TOTAL_MAX_MB", "MULTICA_GC_SALVAGE_TTL",
	} {
		t.Setenv(key, "")
	}
	overrides := Overrides{ServerURL: "http://localhost:0", WorkspacesRoot: t.TempDir()}

	cfg, err := LoadConfig(overrides)
	if err != nil {
		t.Fatalf("LoadConfig with defaults: %v", err)
	}
	if cfg.GCTerminalArtifactTTL != 0 {
		t.Errorf("GCTerminalArtifactTTL = %s, want 0 (same as artifact TTL)", cfg.GCTerminalArtifactTTL)
	}
	if !cfg.GCSalvageEnabled {
		t.Error("GCSalvageEnabled must default to true")
	}
	if cfg.GCSalvageMaxBytes != 500*1024*1024 {
		t.Errorf("GCSalvageMaxBytes = %d, want 500 MB", cfg.GCSalvageMaxBytes)
	}
	if cfg.GCSalvageTotalMaxBytes != 2048*1024*1024 {
		t.Errorf("GCSalvageTotalMaxBytes = %d, want 2048 MB", cfg.GCSalvageTotalMaxBytes)
	}
	if cfg.GCSalvageTTL != 168*time.Hour {
		t.Errorf("GCSalvageTTL = %s, want 168h", cfg.GCSalvageTTL)
	}

	t.Setenv("MULTICA_GC_TERMINAL_ARTIFACT_TTL", "30m")
	t.Setenv("MULTICA_GC_SALVAGE", "false")
	t.Setenv("MULTICA_GC_SALVAGE_MAX_MB", "100")
	t.Setenv("MULTICA_GC_SALVAGE_TOTAL_MAX_MB", "300")
	t.Setenv("MULTICA_GC_SALVAGE_TTL", "2160h")
	cfg, err = LoadConfig(overrides)
	if err != nil {
		t.Fatalf("LoadConfig with overrides: %v", err)
	}
	if cfg.GCTerminalArtifactTTL != 30*time.Minute || cfg.GCSalvageEnabled ||
		cfg.GCSalvageMaxBytes != 100<<20 || cfg.GCSalvageTotalMaxBytes != 300<<20 || cfg.GCSalvageTTL != 2160*time.Hour {
		t.Fatalf("overrides not applied: %+v", cfg)
	}

	for _, key := range []string{"MULTICA_GC_SALVAGE_MAX_MB", "MULTICA_GC_SALVAGE_TOTAL_MAX_MB"} {
		t.Run(key, func(t *testing.T) {
			for _, bad := range []string{"-5", "many"} {
				t.Setenv(key, bad)
				if _, err := LoadConfig(overrides); err == nil || !strings.Contains(err.Error(), key) {
					t.Fatalf("%s=%q: error = %v, want a named validation error", key, bad, err)
				}
			}
		})
	}
}

// Not parallel: LoadConfig reads process environment variables.
func TestLoadConfig_GCOverridesRejectOutOfRange(t *testing.T) {
	stageFakeAgent(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SHELL", filepath.Join(t.TempDir(), "missing-shell"))
	t.Setenv("MULTICA_GC_ENABLED", "")
	negative := -1
	zero := time.Duration(0)
	for name, o := range map[string]Overrides{
		"negative salvage cap": {GCSalvageMaxMB: &negative},
		"negative total cap":   {GCSalvageTotalMaxMB: &negative},
		"zero interval":        {GCInterval: &zero},
	} {
		o.ServerURL = "http://localhost:0"
		o.WorkspacesRoot = t.TempDir()
		if _, err := LoadConfig(o); err == nil {
			t.Errorf("%s: LoadConfig accepted an out-of-range override", name)
		}
	}
}
