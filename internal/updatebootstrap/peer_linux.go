package updatebootstrap

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func peerImage(conn net.Conn) (executableImage, error) {
	socket, ok := conn.(syscall.Conn)
	if !ok {
		return executableImage{}, errors.New("bootstrap requires a local Unix socket")
	}
	raw, err := socket.SyscallConn()
	if err != nil {
		return executableImage{}, err
	}
	var credentials *unix.Ucred
	var probeErr error
	if err = raw.Control(func(fd uintptr) {
		credentials, probeErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return executableImage{}, err
	}
	if probeErr != nil {
		return executableImage{}, probeErr
	}
	path := fmt.Sprintf("/proc/%d/exe", credentials.Pid)
	installed, err := os.Readlink(path)
	return executableImage{Path: path, Installed: installed, PID: int(credentials.Pid)}, err
}
