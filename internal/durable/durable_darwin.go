package durable

import (
	"os"

	"golang.org/x/sys/unix"
)

// Go's File.Sync on Darwin is F_FULLFSYNC, a full drive cache flush that costs
// 15 to 20 ms on the Mac's SSD. A plain fsync moves the same changes to the
// drive without that flush.
func syncFile(file *os.File) error {
	if err := unix.Fsync(int(file.Fd())); err != nil {
		return &os.PathError{Op: "fsync", Path: file.Name(), Err: err}
	}
	return nil
}

func barrier(file *os.File) error {
	if _, err := unix.FcntlInt(file.Fd(), unix.F_FULLFSYNC, 0); err != nil {
		return &os.PathError{Op: "F_FULLFSYNC", Path: file.Name(), Err: err}
	}
	return nil
}
