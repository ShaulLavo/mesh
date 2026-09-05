//go:build linux

package worker

import "testing"

func TestLinuxPTYSlavePath(t *testing.T) {
	for _, path := range []string{"/dev/pts/0", "/dev/pts/17", "/dev/pts/99999"} {
		if !linuxPTYSlavePath(path) {
			t.Fatalf("linuxPTYSlavePath(%q) = false", path)
		}
	}
	for _, path := range []string{
		"", "dev/pts/1", "/dev/pts/", "/dev/pts/../1", "/dev/pts/1/2",
		"/dev/pts/ptmx", "/dev/tty1", "/tmp/fifo", "/dev/pts/1 (deleted)",
	} {
		if linuxPTYSlavePath(path) {
			t.Fatalf("linuxPTYSlavePath(%q) = true", path)
		}
	}
}
