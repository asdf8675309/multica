package daemon

import (
	"bufio"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"
)

const (
	DefaultTaskMemoryMB        = 2048
	DefaultTaskMemoryReserveMB = 2048
)

type hostResources struct {
	CPUs          int
	MemTotalBytes uint64
	MemErr        error
}

// deriveMaxConcurrentTasks picks the task limit when none was set. Each task
// is an agent CLI plus whatever it runs (an `npm ci` peaks near 1.2 GB RSS),
// so a flat 20 can exhaust memory and swap on an 8 vCPU / 16 GB host, where
// about 6 tasks fit. An explicit value always wins.
func deriveMaxConcurrentTasks(explicit *int, host hostResources, perTaskMB, reserveMB int) int {
	if explicit != nil {
		return *explicit
	}
	if host.MemErr != nil || perTaskMB <= 0 {
		return DefaultMaxConcurrentTasks
	}
	n := DefaultMaxConcurrentTasks
	if host.CPUs > 0 && host.CPUs < n {
		n = host.CPUs
	}
	usable := int64(host.MemTotalBytes/uint64(bytesPerMegabyte)) - int64(reserveMB)
	if byMem := int(usable / int64(perTaskMB)); byMem < n {
		n = byMem
	}
	if n < 1 {
		n = 1
	}
	return n
}

// ConcurrencyDerivation records how a derived max_concurrent_tasks was
// reached, so the daemon start path can log it once. LoadConfig also runs for
// read-only probes such as `daemon probe-runtimes`, which must stay quiet.
type ConcurrencyDerivation struct {
	Limit        int
	Host         hostResources
	TaskMemoryMB int
	ReserveMB    int
}

func readHostResources() hostResources {
	mem, err := readTotalMemoryBytes()
	return hostResources{CPUs: runtime.NumCPU(), MemTotalBytes: mem, MemErr: err}
}

func readMemTotalBytes(path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kb, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return 0, fmt.Errorf("parse MemTotal %q: %w", fields[1], err)
			}
			return kb * 1024, nil
		}
	}
	if err := s.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("MemTotal not found in %s", path)
}

// LogMaxConcurrentTasksDerivation logs the derived limit and its inputs. It is
// a no-op when the limit was set explicitly.
func (c Config) LogMaxConcurrentTasksDerivation() {
	d := c.MaxConcurrentTasksDerivation
	if d == nil {
		return
	}
	if d.Host.MemErr != nil {
		slog.Info("max_concurrent_tasks not set; host memory unreadable, using upstream default",
			"max_concurrent_tasks", d.Limit, "cpus", d.Host.CPUs, "err", d.Host.MemErr)
		return
	}
	slog.Info("max_concurrent_tasks not set; derived from host resources",
		"max_concurrent_tasks", d.Limit, "cpus", d.Host.CPUs, "mem_total_mb", d.Host.MemTotalBytes/uint64(bytesPerMegabyte),
		"task_memory_mb", d.TaskMemoryMB, "task_memory_reserve_mb", d.ReserveMB)
}
