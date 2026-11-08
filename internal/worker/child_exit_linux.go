package worker

import (
	"errors"
	"golang.org/x/sys/unix"
)

func childExitSupported() error { return nil }
func observeChildExit(pid int) error {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return workerExecutionError("observe child exit", err)
	}
}
