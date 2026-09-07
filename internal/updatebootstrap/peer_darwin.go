package updatebootstrap

import (
	"context"
	"debug/buildinfo"
	"errors"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

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
	var pid int
	var probeErr error
	if err = raw.Control(func(fd uintptr) { pid, probeErr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID) }); err != nil {
		return executableImage{}, err
	}
	if probeErr != nil {
		return executableImage{}, probeErr
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "lsof", "-a", "-p", strconv.Itoa(pid), "-d", "txt", "-F", "in").Output()
	if err != nil {
		return executableImage{}, err
	}
	var inode uint64
	for _, line := range strings.Split(string(output), "\n") {
		if strings.HasPrefix(line, "i") {
			inode, _ = strconv.ParseUint(line[1:], 10, 64)
		}
		if !strings.HasPrefix(line, "n") {
			continue
		}
		if image, ok := loadedMeshImage(line[1:], inode, pid); ok {
			return image, nil
		}
	}
	return executableImage{}, errors.New("cannot prove the connected process's loaded Mesh executable")
}

func loadedMeshImage(path string, inode uint64, pid int) (executableImage, bool) {
	var stat unix.Stat_t
	if unix.Stat(path, &stat) != nil || stat.Ino != inode {
		return executableImage{}, false
	}
	info, err := buildinfo.ReadFile(path)
	if err != nil || !strings.HasPrefix(info.Path, "github.com/shaul/mesh/") {
		return executableImage{}, false
	}
	return executableImage{Path: path, Installed: path, PID: pid}, true
}
