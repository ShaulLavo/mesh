package updatebootstrap

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func peerImage(ctx context.Context, conn net.Conn) (executableImage, error) {
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
	return processImage(ctx, int(credentials.Pid))
}

func processImage(ctx context.Context, pid int) (executableImage, error) {
	if err := ctx.Err(); err != nil {
		return executableImage{}, fmt.Errorf("inspect process image: %w", err)
	}
	path := fmt.Sprintf("/proc/%d/exe", pid)
	installed, err := os.Readlink(path)
	if err != nil {
		return executableImage{}, fmt.Errorf("resolve executing process image: %w", err)
	}
	return executableImage{Path: path, Installed: installed, PID: pid}, nil
}
