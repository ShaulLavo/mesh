package worker

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func childExitSupported() error { return nil }

// The caller is the sole reaper of its own unreaped child. Darwin can refuse
// EVFILT_PROC registration before the child becomes SZOMB. Subscribe to the
// parent's SIGCHLD first: exit sets SZOMB before sending it, even in that gap.
func observeChildExit(pid int) error {
	fd, err := unix.Kqueue()
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	change := unix.Kevent_t{Ident: uint64(unix.SIGCHLD), Filter: unix.EVFILT_SIGNAL, Flags: unix.EV_ADD | unix.EV_CLEAR}
	if _, err := unix.Kevent(fd, []unix.Kevent_t{change}, nil, nil); err != nil {
		return fmt.Errorf("worker: register child exit signal: %w", err)
	}
	return awaitChildExit(fd, pid)
}

func awaitChildExit(fd, pid int) error {
	for {
		info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
		if err != nil {
			return err
		}
		exited, err := childExitState(pid, info)
		if err != nil || exited {
			return err
		}
		if err := awaitChildExitSignal(fd); err != nil {
			return err
		}
	}
}

func childExitState(pid int, info *unix.KinfoProc) (bool, error) {
	if info == nil || int(info.Proc.P_pid) != pid {
		return false, fmt.Errorf("worker: child exit identity unavailable")
	}
	if int(info.Eproc.Ppid) != os.Getpid() {
		return false, unix.ECHILD
	}
	// SZOMB is the public Darwin sys/proc.h state. A signal alone is not proof:
	// stops, continuations and other children can also wake this observer.
	return info.Proc.P_stat == 5, nil
}

func awaitChildExitSignal(fd int) error {
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
			return fmt.Errorf("worker: child exit signal: %w", unix.Errno(event.Data))
		}
		if event.Ident != uint64(unix.SIGCHLD) || event.Filter != unix.EVFILT_SIGNAL {
			return fmt.Errorf("worker: invalid child exit signal")
		}
		return nil
	}
}
