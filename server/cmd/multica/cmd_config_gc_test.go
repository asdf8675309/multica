package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/internal/daemon"
)

func TestApplyConfigSetGCKeys(t *testing.T) {
	var cfg cli.CLIConfig
	for key, value := range map[string]string{
		"gc_artifact_ttl":          "0s",
		"gc_ttl":                   "48h",
		"gc_interval":              "30m",
		"gc_codex_session_ttl":     "336h",
		"gc_terminal_artifact_ttl": "1h",
		"gc_salvage":               "false",
		"gc_salvage_max_mb":        "0",
		"gc_salvage_total_max_mb":  "4096",
		"gc_salvage_ttl":           "72h",
	} {
		if err := applyConfigSet(&cfg, key, value); err != nil {
			t.Fatalf("applyConfigSet(%s, %s): %v", key, value, err)
		}
	}
	if cfg.GCArtifactTTL != "0s" || cfg.GCTTL != "48h" || cfg.GCInterval != "30m" ||
		cfg.GCCodexSessionTTL != "336h" || cfg.GCTerminalArtifactTTL != "1h" || cfg.GCSalvageTTL != "72h" {
		t.Fatalf("durations not stored: %+v", cfg)
	}
	if cfg.GCSalvage == nil || *cfg.GCSalvage {
		t.Fatalf("gc_salvage false must persist as a non-nil false, got %v", cfg.GCSalvage)
	}
	if cfg.GCSalvageMaxMB == nil || *cfg.GCSalvageMaxMB != 0 {
		t.Fatalf("gc_salvage_max_mb 0 must persist as a non-nil 0, got %v", cfg.GCSalvageMaxMB)
	}
	if cfg.GCSalvageTotalMaxMB == nil || *cfg.GCSalvageTotalMaxMB != 4096 {
		t.Fatalf("gc_salvage_total_max_mb = %v, want 4096", cfg.GCSalvageTotalMaxMB)
	}

	for _, key := range []string{
		"gc_artifact_ttl", "gc_ttl", "gc_interval", "gc_codex_session_ttl", "gc_terminal_artifact_ttl",
		"gc_salvage", "gc_salvage_max_mb", "gc_salvage_total_max_mb", "gc_salvage_ttl",
	} {
		if err := applyConfigSet(&cfg, key, ""); err != nil {
			t.Fatalf("clear %s: %v", key, err)
		}
	}
	if cfg.GCArtifactTTL != "" || cfg.GCTTL != "" || cfg.GCInterval != "" || cfg.GCCodexSessionTTL != "" ||
		cfg.GCTerminalArtifactTTL != "" || cfg.GCSalvageTTL != "" ||
		cfg.GCSalvage != nil || cfg.GCSalvageMaxMB != nil || cfg.GCSalvageTotalMaxMB != nil {
		t.Fatalf("empty string must clear every gc_* key, got %+v", cfg)
	}
}

func TestApplyConfigSetGCKeysRejectBadValues(t *testing.T) {
	cases := []struct{ key, value string }{
		{"gc_artifact_ttl", "60mm"},
		{"gc_salvage_ttl", "60mm"},
		{"gc_artifact_ttl", "-1h"},
		{"gc_interval", "0s"},
		{"gc_ttl", "0s"},
		{"gc_salvage", "maybe"},
		{"gc_salvage_max_mb", "-1"},
		{"gc_salvage_total_max_mb", "lots"},
	}
	for _, tc := range cases {
		var cfg cli.CLIConfig
		if err := applyConfigSet(&cfg, tc.key, tc.value); err == nil {
			t.Errorf("applyConfigSet(%s, %q) accepted a bad value: %+v", tc.key, tc.value, cfg)
		}
	}
}

func TestRunConfigShowListsGCKeys(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cmd := newConfigTestCmd()
	out, err := captureStdout(t, func() error { return runConfigShow(cmd, nil) })
	if err != nil {
		t.Fatalf("runConfigShow: %v", err)
	}
	for _, key := range []string{
		"gc_artifact_ttl:", "gc_ttl:", "gc_interval:", "gc_codex_session_ttl:", "gc_terminal_artifact_ttl:",
		"gc_salvage:", "gc_salvage_max_mb:", "gc_salvage_total_max_mb:", "gc_salvage_ttl:",
	} {
		if !strings.Contains(out, key) {
			t.Fatalf("runConfigShow output missing %q:\n%s", key, out)
		}
	}
}

// loadGCConfig loads the config the way `daemon start` does.
func loadGCConfig(t *testing.T, fileCfg cli.CLIConfig) daemon.Config {
	t.Helper()
	cfg, err := loadDaemonStartConfig(testDaemonOverrides(t), fileCfg)
	if err != nil {
		t.Fatalf("loadDaemonStartConfig: %v", err)
	}
	return cfg
}

func testDaemonOverrides(t *testing.T) daemon.Overrides {
	return daemon.Overrides{
		ServerURL:      "http://localhost:0",
		WorkspacesRoot: filepath.Join(t.TempDir(), "ws"),
		AllowNoAgents:  true,
	}
}

// Not parallel: reads process environment variables.
func TestGCConfigPrecedenceEnvOverConfigOverDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, key := range []string{"MULTICA_GC_ARTIFACT_TTL", "MULTICA_GC_SALVAGE_MAX_MB", "MULTICA_GC_SALVAGE"} {
		t.Setenv(key, "")
	}
	zero, off := 0, false
	fileCfg := cli.CLIConfig{GCArtifactTTL: "0s", GCSalvageMaxMB: &zero, GCSalvage: &off}

	t.Run("default when nothing is set", func(t *testing.T) {
		cfg := loadGCConfig(t, cli.CLIConfig{})
		if cfg.GCArtifactTTL != daemon.DefaultGCArtifactTTL || cfg.GCSalvageMaxBytes != int64(daemon.DefaultGCSalvageMaxMB)<<20 ||
			cfg.GCSalvageEnabled != daemon.DefaultGCSalvageEnabled {
			t.Fatalf("defaults not applied: ttl=%s max=%d salvage=%t", cfg.GCArtifactTTL, cfg.GCSalvageMaxBytes, cfg.GCSalvageEnabled)
		}
	})
	t.Run("config.json over default, zero and false kept", func(t *testing.T) {
		cfg := loadGCConfig(t, fileCfg)
		if cfg.GCArtifactTTL != 0 || cfg.GCSalvageMaxBytes != 0 || cfg.GCSalvageEnabled {
			t.Fatalf("config values not applied: ttl=%s max=%d salvage=%t", cfg.GCArtifactTTL, cfg.GCSalvageMaxBytes, cfg.GCSalvageEnabled)
		}
	})
	t.Run("env over config.json", func(t *testing.T) {
		t.Setenv("MULTICA_GC_ARTIFACT_TTL", "3h")
		t.Setenv("MULTICA_GC_SALVAGE_MAX_MB", "7")
		t.Setenv("MULTICA_GC_SALVAGE", "true")
		cfg := loadGCConfig(t, fileCfg)
		if cfg.GCArtifactTTL != 3*time.Hour || cfg.GCSalvageMaxBytes != 7<<20 || !cfg.GCSalvageEnabled {
			t.Fatalf("env did not win: ttl=%s max=%d salvage=%t", cfg.GCArtifactTTL, cfg.GCSalvageMaxBytes, cfg.GCSalvageEnabled)
		}
	})
}

func TestApplyGCConfigOverridesRejectsInvalidPersistedValues(t *testing.T) {
	t.Setenv("MULTICA_GC_SALVAGE_TTL", "")
	t.Setenv("MULTICA_GC_INTERVAL", "")
	for _, fileCfg := range []cli.CLIConfig{
		{GCSalvageTTL: "60mm"},
		{GCInterval: "0s"},
	} {
		var o daemon.Overrides
		if err := applyGCConfigOverrides(&o, fileCfg); err == nil {
			t.Errorf("hand-edited config %+v must fail loudly, got overrides %+v", fileCfg, o)
		}
	}
}

// Each persisted gc_* key gives way to its own env var and to no other. Every
// key and every env value is distinct, so a swapped destination or a
// misspelled env name in applyGCConfigOverrides lands on the wrong value.
//
// Not parallel: reads process environment variables.
func TestGCConfigEveryKeyYieldsToItsOwnEnv(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	off, maxMB, totalMB := false, 17, 18
	fileCfg := cli.CLIConfig{
		GCArtifactTTL:         "11h",
		GCTTL:                 "12h",
		GCInterval:            "13m",
		GCCodexSessionTTL:     "14h",
		GCTerminalArtifactTTL: "15m",
		GCSalvageTTL:          "16h",
		GCSalvage:             &off,
		GCSalvageMaxMB:        &maxMB,
		GCSalvageTotalMaxMB:   &totalMB,
	}
	keys := []struct {
		env, envValue    string
		fromCfg, fromEnv any
		get              func(daemon.Config) any
	}{
		{"MULTICA_GC_ARTIFACT_TTL", "21h", 11 * time.Hour, 21 * time.Hour, func(c daemon.Config) any { return c.GCArtifactTTL }},
		{"MULTICA_GC_TTL", "22h", 12 * time.Hour, 22 * time.Hour, func(c daemon.Config) any { return c.GCTTL }},
		{"MULTICA_GC_INTERVAL", "23m", 13 * time.Minute, 23 * time.Minute, func(c daemon.Config) any { return c.GCInterval }},
		{"MULTICA_GC_CODEX_SESSION_TTL", "24h", 14 * time.Hour, 24 * time.Hour, func(c daemon.Config) any { return c.GCCodexSessionTTL }},
		{"MULTICA_GC_TERMINAL_ARTIFACT_TTL", "25m", 15 * time.Minute, 25 * time.Minute, func(c daemon.Config) any { return c.GCTerminalArtifactTTL }},
		{"MULTICA_GC_SALVAGE_TTL", "26h", 16 * time.Hour, 26 * time.Hour, func(c daemon.Config) any { return c.GCSalvageTTL }},
		{"MULTICA_GC_SALVAGE", "true", false, true, func(c daemon.Config) any { return c.GCSalvageEnabled }},
		{"MULTICA_GC_SALVAGE_MAX_MB", "27", int64(17) << 20, int64(27) << 20, func(c daemon.Config) any { return c.GCSalvageMaxBytes }},
		{"MULTICA_GC_SALVAGE_TOTAL_MAX_MB", "28", int64(18) << 20, int64(28) << 20, func(c daemon.Config) any { return c.GCSalvageTotalMaxBytes }},
	}
	for _, k := range keys {
		t.Setenv(k.env, "")
	}
	cases := []string{""}
	for _, k := range keys {
		cases = append(cases, k.env)
	}
	for _, envKey := range cases {
		name := envKey
		if name == "" {
			name = "no env"
		}
		t.Run(name, func(t *testing.T) {
			for _, k := range keys {
				if k.env == envKey {
					t.Setenv(k.env, k.envValue)
				}
			}
			cfg := loadGCConfig(t, fileCfg)
			for _, k := range keys {
				want := k.fromCfg
				if k.env == envKey {
					want = k.fromEnv
				}
				if got := k.get(cfg); got != want {
					t.Errorf("%s set: config for %s = %v, want %v", name, k.env, got, want)
				}
			}
		})
	}
}
