package durable

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// HasBarrier reports that Sync leaves changes in the drive cache, so a caller
// must issue Barrier on every device it synced.
const HasBarrier = true

// Go's File.Sync on Darwin is F_FULLFSYNC, a full drive cache flush that costs
// 15 to 20 ms on the Mac's SSD. A plain fsync moves the same changes to the
// drive without that flush. Both retry an interrupted call, as File.Sync does.
func syncFile(file *os.File) error {
	fd := int(file.Fd())
	err := retryInterrupted(func() error { return unix.Fsync(fd) })
	if err != nil {
		return &os.PathError{Op: "fsync", Path: file.Name(), Err: err}
	}
	return nil
}

// barrier falls back to a plain fsync where F_FULLFSYNC is unsupported, such
// as SMB mounts, exactly as File.Sync does.
func barrier(file *os.File) error {
	fd := file.Fd()
	err := retryInterrupted(func() error {
		_, err := unix.FcntlInt(fd, unix.F_FULLFSYNC, 0)
		if errors.Is(err, unix.ENOTSUP) {
			return unix.Fsync(int(fd))
		}
		return err
	})
	if err != nil {
		return &os.PathError{Op: "F_FULLFSYNC", Path: file.Name(), Err: err}
	}
	return nil
}

func retryInterrupted(call func() error) error {
	for {
		err := call()
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}
