package update

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// openLockAt opens name inside directory, whose Name is its path. Errors name
// the full path: a sandbox that denies writes to the Mesh state directory
// otherwise surfaces as a bare ".lock" with no hint of where it lives.
func openLockAt(directory *os.File, name string) (int, error) {
	const flags = unix.O_RDWR | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
	// macOS can return ENOENT when nonexclusive openat creators race. Exclusive
	// creation elects one creator; the others open its persistent lock inode.
	fd, err := unix.Openat(int(directory.Fd()), name, flags|unix.O_CREAT|unix.O_EXCL, 0600)
	if errors.Is(err, unix.EEXIST) {
		fd, err = unix.Openat(int(directory.Fd()), name, flags, 0)
	}
	if err == nil {
		return fd, nil
	}
	path := filepath.Join(directory.Name(), name)
	if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
		return -1, fmt.Errorf("local permissions blocked opening lock %s: %w", path, err)
	}
	return -1, fmt.Errorf("open lock %s: %w", path, err)
}
