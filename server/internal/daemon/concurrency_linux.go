package daemon

func readTotalMemoryBytes() (uint64, error) {
	return readMemTotalBytes("/proc/meminfo")
}
