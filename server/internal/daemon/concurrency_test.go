package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDeriveMaxConcurrentTasks(t *testing.T) {
	const gb = uint64(1 << 30)
	three, zero := 3, 0
	cases := []struct {
		name     string
		explicit *int
		host     hostResources
		want     int
	}{
		{"8 CPU / 16 GB host", nil, hostResources{CPUs: 8, MemTotalBytes: 16 * gb}, 7},
		{"CPU bound", nil, hostResources{CPUs: 4, MemTotalBytes: 64 * gb}, 4},
		{"capped at upstream default", nil, hostResources{CPUs: 64, MemTotalBytes: 512 * gb}, DefaultMaxConcurrentTasks},
		{"low memory floors at 1", nil, hostResources{CPUs: 8, MemTotalBytes: 1 * gb}, 1},
		{"unreadable memory uses upstream default", nil, hostResources{CPUs: 8, MemErr: errors.New("no /proc")}, DefaultMaxConcurrentTasks},
		{"explicit wins", &three, hostResources{CPUs: 64, MemTotalBytes: 512 * gb}, 3},
		{"explicit 0 wins", &zero, hostResources{CPUs: 8, MemTotalBytes: 16 * gb}, 0},
	}
	for _, tc := range cases {
		if got := deriveMaxConcurrentTasks(tc.explicit, tc.host, DefaultTaskMemoryMB, DefaultTaskMemoryReserveMB); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestReadMemTotalBytes(t *testing.T) {
	p := filepath.Join(t.TempDir(), "meminfo")
	if err := os.WriteFile(p, []byte("MemFree: 10 kB\nMemTotal:       16384000 kB\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readMemTotalBytes(p)
	if err != nil || got != 16384000*1024 {
		t.Fatalf("got %d, %v", got, err)
	}
	if _, err := readMemTotalBytes(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing file must error")
	}
}

func TestDeriveMaxConcurrentTasksHonorsTaskMemorySettings(t *testing.T) {
	const gb = uint64(1 << 30)
	host := hostResources{CPUs: 16, MemTotalBytes: 16 * gb}
	for _, tc := range []struct {
		perTaskMB, reserveMB, want int
	}{
		{1024, 0, 16},
		{4096, 0, 4},
		{2048, 8192, 4},
		{2048, 2048, 7},
	} {
		if got := deriveMaxConcurrentTasks(nil, host, tc.perTaskMB, tc.reserveMB); got != tc.want {
			t.Errorf("perTask=%d reserve=%d: got %d, want %d", tc.perTaskMB, tc.reserveMB, got, tc.want)
		}
	}
}
