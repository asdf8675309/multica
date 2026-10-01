package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/internal/daemon"
)

func TestApplyConfigSetTaskMemoryKeys(t *testing.T) {
	var cfg cli.CLIConfig
	if err := applyConfigSet(&cfg, "task_memory_mb", "1536"); err != nil {
		t.Fatal(err)
	}
	if err := applyConfigSet(&cfg, "task_memory_reserve_mb", "0"); err != nil {
		t.Fatal(err)
	}
	if cfg.TaskMemoryMB == nil || *cfg.TaskMemoryMB != 1536 {
		t.Fatalf("task_memory_mb = %v, want 1536", cfg.TaskMemoryMB)
	}
	if cfg.TaskMemoryReserveMB == nil || *cfg.TaskMemoryReserveMB != 0 {
		t.Fatalf("a reserve of 0 must persist as a non-nil 0, got %v", cfg.TaskMemoryReserveMB)
	}
	for _, key := range []string{"task_memory_mb", "task_memory_reserve_mb"} {
		if err := applyConfigSet(&cfg, key, ""); err != nil {
			t.Fatalf("clear %s: %v", key, err)
		}
	}
	if cfg.TaskMemoryMB != nil || cfg.TaskMemoryReserveMB != nil {
		t.Fatalf("empty string must clear both keys, got %+v", cfg)
	}
}

func TestApplyConfigSetTaskMemoryKeysRejectBadValues(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"task_memory_mb", "0"},
		{"task_memory_mb", "-5"},
		{"task_memory_mb", "2g"},
		{"task_memory_reserve_mb", "-1"},
		{"task_memory_reserve_mb", "lots"},
	} {
		var cfg cli.CLIConfig
		if err := applyConfigSet(&cfg, tc.key, tc.value); err == nil {
			t.Errorf("applyConfigSet(%s, %q) accepted a bad value: %+v", tc.key, tc.value, cfg)
		}
	}
}

func loadTaskMemoryConfig(t *testing.T, fileCfg cli.CLIConfig) (daemon.Config, error) {
	t.Helper()
	return loadDaemonStartConfig(testDaemonOverrides(t), fileCfg)
}

// Not parallel: reads process environment variables.
func TestTaskMemoryPrecedenceEnvOverConfigOverDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, key := range []string{"MULTICA_DAEMON_MAX_CONCURRENT_TASKS", "MULTICA_DAEMON_TASK_MEMORY_MB", "MULTICA_DAEMON_TASK_MEMORY_RESERVE_MB"} {
		t.Setenv(key, "")
	}
	mb, reserve := 1024, 0
	fileCfg := cli.CLIConfig{TaskMemoryMB: &mb, TaskMemoryReserveMB: &reserve}

	t.Run("default when nothing is set", func(t *testing.T) {
		cfg, err := loadTaskMemoryConfig(t, cli.CLIConfig{})
		if err != nil {
			t.Fatal(err)
		}
		d := cfg.MaxConcurrentTasksDerivation
		if d == nil || d.TaskMemoryMB != daemon.DefaultTaskMemoryMB || d.ReserveMB != daemon.DefaultTaskMemoryReserveMB {
			t.Fatalf("defaults not applied: %+v", d)
		}
	})
	t.Run("config.json over default, reserve 0 kept", func(t *testing.T) {
		cfg, err := loadTaskMemoryConfig(t, fileCfg)
		if err != nil {
			t.Fatal(err)
		}
		d := cfg.MaxConcurrentTasksDerivation
		if d == nil || d.TaskMemoryMB != 1024 || d.ReserveMB != 0 {
			t.Fatalf("config values not applied: %+v", d)
		}
	})
	t.Run("env over config.json", func(t *testing.T) {
		t.Setenv("MULTICA_DAEMON_TASK_MEMORY_MB", "3072")
		t.Setenv("MULTICA_DAEMON_TASK_MEMORY_RESERVE_MB", "512")
		cfg, err := loadTaskMemoryConfig(t, fileCfg)
		if err != nil {
			t.Fatal(err)
		}
		d := cfg.MaxConcurrentTasksDerivation
		if d == nil || d.TaskMemoryMB != 3072 || d.ReserveMB != 512 {
			t.Fatalf("env did not win: %+v", d)
		}
	})
}

// Not parallel: reads process environment variables.
func TestTaskMemoryRejectsInvalidPersistedAndEnvValues(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MULTICA_DAEMON_MAX_CONCURRENT_TASKS", "")
	t.Setenv("MULTICA_DAEMON_TASK_MEMORY_MB", "")
	t.Setenv("MULTICA_DAEMON_TASK_MEMORY_RESERVE_MB", "")
	zero, negative := 0, -1
	for name, fileCfg := range map[string]cli.CLIConfig{
		"zero task memory": {TaskMemoryMB: &zero},
		"negative reserve": {TaskMemoryReserveMB: &negative},
	} {
		if _, err := loadTaskMemoryConfig(t, fileCfg); err == nil {
			t.Errorf("%s: hand-edited config must fail daemon start", name)
		}
	}
	t.Setenv("MULTICA_DAEMON_TASK_MEMORY_MB", "0")
	if _, err := loadTaskMemoryConfig(t, cli.CLIConfig{}); err == nil {
		t.Error("env task memory 0 must be rejected")
	}
}

// Not parallel: reads process environment variables.
func TestTaskMemoryFeedsDerivedLimit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MULTICA_DAEMON_MAX_CONCURRENT_TASKS", "")
	t.Setenv("MULTICA_DAEMON_TASK_MEMORY_MB", "")
	t.Setenv("MULTICA_DAEMON_TASK_MEMORY_RESERVE_MB", "")
	huge := 1 << 30
	cfg, err := loadTaskMemoryConfig(t, cli.CLIConfig{TaskMemoryMB: &huge})
	if err != nil {
		t.Fatal(err)
	}
	if d := cfg.MaxConcurrentTasksDerivation; d == nil || d.Host.MemErr != nil {
		t.Skipf("host memory unreadable: %+v", d)
	}
	if cfg.MaxConcurrentTasks != 1 {
		t.Fatalf("a per-task size larger than the host must floor at 1, got %d", cfg.MaxConcurrentTasks)
	}
}

// Not parallel: swaps the process-wide slog default and reads the environment.
func TestDerivationIsLoggedByTheStartPathNotByLoadConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MULTICA_DAEMON_MAX_CONCURRENT_TASKS", "")
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	if _, err := daemon.LoadConfig(testDaemonOverrides(t)); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Fatalf("LoadConfig must not log the derivation (probe-runtimes calls it too): %s", buf.String())
	}
	if _, err := loadTaskMemoryConfig(t, cli.CLIConfig{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "max_concurrent_tasks") {
		t.Fatalf("start path must log the derivation, got %q", buf.String())
	}

	buf.Reset()
	t.Setenv("MULTICA_DAEMON_MAX_CONCURRENT_TASKS", "3")
	cfg, err := loadTaskMemoryConfig(t, cli.CLIConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxConcurrentTasks != 3 || buf.Len() != 0 {
		t.Fatalf("an explicit limit must not log a derivation: limit=%d log=%q", cfg.MaxConcurrentTasks, buf.String())
	}
}
