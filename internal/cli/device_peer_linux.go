package cli

import (
	"fmt"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

func approvalPeerPID(conn net.Conn) (int32, error) {
	raw, err := conn.(syscall.Conn).SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("inspect Unix socket credentials: %w", err)
	}
	var pid int32
	var probeErr error
	err = raw.Control(func(fd uintptr) {
		credentials, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if err != nil {
			probeErr = err
			return
		}
		pid = credentials.Pid
	})
	if err != nil {
		return 0, fmt.Errorf("inspect Unix socket credentials: %w", err)
	}
	return pid, probeErr
}
