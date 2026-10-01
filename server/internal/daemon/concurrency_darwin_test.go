package daemon

import "testing"

func TestReadTotalMemoryBytesDarwin(t *testing.T) {
	got, err := readTotalMemoryBytes()
	if err != nil || got == 0 {
		t.Fatalf("hw.memsize: got %d, %v", got, err)
	}
}
