//go:build linux || darwin

package worker

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

func (w *Worker) validateAgentCaller(conn net.Conn, pid int) error {
	peerPID, err := agentPeerPID(conn)
	if err != nil || pid <= 0 || peerPID != pid {
		return fmt.Errorf("worker: agent helper does not match the local socket peer")
	}
	location, ok := containingSessionWorkerFromAncestors(pid, readAncestorProcess)
	if !ok || location.SessionID != w.cfg.ID || location.Dir != w.cfg.Dir {
		return fmt.Errorf("worker: agent helper is not inside this worker")
	}
	if w.pty == nil {
		return fmt.Errorf("worker: agent helper has no foreground terminal")
	}
	group, err := unix.Getpgid(pid)
	if err != nil || group <= 0 || group != foregroundProcessGroupID(int(w.pty.Fd())) {
		return fmt.Errorf("worker: automatic capture requires the foreground agent; use a separate Mesh session for background agents")
	}
	return nil
}

func agentPeerPID(conn net.Conn) (int, error) {
	local, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("agent registration requires a Unix socket")
	}
	raw, err := local.SyscallConn()
	if err != nil {
		return 0, err
	}
	var pid int
	var peerErr error
	err = raw.Control(func(fd uintptr) { pid, peerErr = socketPeerPID(int(fd)) })
	if err != nil {
		return 0, err
	}
	return pid, peerErr
}
