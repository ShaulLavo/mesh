//go:build !windows

package cli

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func duplicateAttachmentOutput(output *os.File) (*os.File, func(), error) {
	raw, err := output.SyscallConn()
	if err != nil {
		return nil, nil, fmt.Errorf("access terminal output: %w", err)
	}
	fd := -1
	var flags int
	var setupErr error
	err = raw.Control(func(source uintptr) {
		flags, setupErr = unix.FcntlInt(source, unix.F_GETFL, 0)
		if setupErr != nil {
			return
		}
		fd, setupErr = unix.Dup(int(source))
		if setupErr != nil {
			return
		}
		unix.CloseOnExec(fd)
		// NewFile registers a nonblocking descriptor with Go's poller, so
		// closing our copy interrupts a full pipe without closing stdout.
		setupErr = unix.SetNonblock(fd, true)
	})
	if err != nil || setupErr != nil {
		if fd >= 0 {
			_ = unix.Close(fd)
		}
		if err == nil {
			err = setupErr
		}
		return nil, nil, fmt.Errorf("duplicate terminal output: %w", err)
	}
	restoreFlags := func() {
		// Dup shares file status flags, but not the lifetime or deadlines.
		_ = raw.Control(func(source uintptr) { _, _ = unix.FcntlInt(source, unix.F_SETFL, flags) })
	}
	return os.NewFile(uintptr(fd), output.Name()), restoreFlags, nil
}
