//go:build !linux && !darwin

package daemon

import "errors"

func readTotalMemoryBytes() (uint64, error) {
	return 0, errors.New("total memory is not readable on this platform")
}
