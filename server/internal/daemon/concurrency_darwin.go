package daemon

import "golang.org/x/sys/unix"

func readTotalMemoryBytes() (uint64, error) {
	return unix.SysctlUint64("hw.memsize")
}
