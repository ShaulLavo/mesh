package worker

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func childExitSupported() error { return nil }

// The caller is the sole reaper of its own unreaped child. Register before checking
// zombie state, since EVFILT_PROC is edge-triggered and a fast exit can precede it.
func observeChildExit(pid int) error {
	fd, err := unix.Kqueue()
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	change := unix.Kevent_t{Ident: uint64(pid), Filter: unix.EVFILT_PROC, Flags: unix.EV_ADD | unix.EV_ONESHOT, Fflags: unix.NOTE_EXIT}
	_, registerErr := unix.Kevent(fd, []unix.Kevent_t{change}, nil, nil)
	if registerErr != nil && !errors.Is(registerErr, unix.ESRCH) {
		return registerErr
	}
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return err
	}
	if int(info.Proc.P_pid) != pid {
		return fmt.Errorf("worker: child exit identity unavailable")
	}
	if int(info.Eproc.Ppid) != os.Getpid() {
		return unix.ECHILD
	}
	// SZOMB is the public Darwin sys/proc.h state. No other state authorizes reaping.
	if info.Proc.P_stat == 5 {
		return nil
	}
	if registerErr != nil {
		return fmt.Errorf("worker: register child exit: %w", registerErr)
	}
	return awaitChildExit(fd, pid)
}
func awaitChildExit(fd, pid int) error {
	events := make([]unix.Kevent_t, 1)
	for {
		n, err := unix.Kevent(fd, nil, events, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if n != 1 {
			continue
		}
		event := events[0]
		if event.Flags&unix.EV_ERROR != 0 {
			return fmt.Errorf("worker: child exit event: %w", unix.Errno(event.Data))
		}
		if event.Ident != uint64(pid) || event.Filter != unix.EVFILT_PROC || event.Fflags&unix.NOTE_EXIT == 0 {
			return fmt.Errorf("worker: invalid child exit event")
		}
		return nil
	}
}
