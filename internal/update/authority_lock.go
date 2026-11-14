package update

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func openLockAt(directory int, name string) (int, error) {
	const flags = unix.O_RDWR | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
	// macOS can return ENOENT when nonexclusive openat creators race. Exclusive
	// creation elects one creator; the others open its persistent lock inode.
	fd, err := unix.Openat(directory, name, flags|unix.O_CREAT|unix.O_EXCL, 0600)
	if errors.Is(err, unix.EEXIST) {
		fd, err = unix.Openat(directory, name, flags, 0)
	}
	if err != nil {
		return -1, fmt.Errorf("open lock %s: %w", name, err)
	}
	return fd, nil
}
