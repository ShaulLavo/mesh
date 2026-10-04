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
	var pid int
	var probeErr error
	err = raw.Control(func(fd uintptr) {
		pid, probeErr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	})
	if err != nil {
		return 0, fmt.Errorf("inspect Unix socket credentials: %w", err)
	}
	return int32(pid), probeErr //nolint:gosec // LOCAL_PEERPID returns the native signed 32-bit process identifier
}
