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

func peerPID(conn net.Conn) (int, error) {
	socket, ok := conn.(syscall.Conn)
	if !ok {
		return 0, fmt.Errorf("inspect Unix peer process: %w", err)ors.New("bootstrap requires a local Unix socket")
	}
	raw, err := socket.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("inspect Unix peer process: %w", err)
	}
	var credentials *unix.Ucred
	var probeErr error
	if err = raw.Control(func(fd uintptr) {
		credentials, probeErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, fmt.Errorf("inspect Unix peer process: %w", err)
	}
	if probeErr != nil {
		return 0, fmt.Errorf("inspect Unix peer credentials: %w", probeErr)
	}
	return int(credentials.Pid), nil
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

func processImages(ctx context.Context, pids []int) []processImageResult {
	results := make([]processImageResult, len(pids))
	for i, pid := range pids {
		results[i].image, results[i].err = processImage(ctx, pid)
	}
	return results
}
