package updatebootstrap

import (
	"context"
	"debug/buildinfo"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
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
	output, err := exec.CommandContext(ctx, "lsof", "-a", "-p", strconv.Itoa(pid), "-d", "txt", "-F", "finD").Output() //nolint:gosec // fixed tool and flags; decimal PID is obtained from LOCAL_PEERPID
	if err != nil {
		return executableImage{}, err
	}
	return parseMappedMeshImage(string(output), pid)
}

func parseMappedMeshImage(output string, pid int) (executableImage, error) {
	var inode, device uint64
	var path string
	var deviceKnown bool
	resolve := func() (executableImage, bool) {
		if !deviceKnown || inode == 0 || path == "" {
			return executableImage{}, false
		}
		return mappedMeshImage(path, device, inode, pid)
	}
	for _, line := range strings.Split(output, "\n") {
		if line == "" {
			continue
		}
		switch line[0] {
		case 'f':
			if image, ok := resolve(); ok {
				return image, nil
			}
			path, inode, device, deviceKnown = "", 0, 0, false
		case 'i':
			inode, _ = strconv.ParseUint(line[1:], 10, 64)
		case 'D':
			var err error
			device, err = strconv.ParseUint(line[1:], 0, 64)
			deviceKnown = err == nil
		case 'n':
			path = line[1:]
		}
	}
	if image, ok := resolve(); ok {
		return image, nil
	}
	return executableImage{}, errors.New("cannot prove the connected process's loaded Mesh executable")
}

func mappedMeshImage(path string, device, inode uint64, pid int) (executableImage, bool) {
	if image, ok := loadedMeshImage(path, device, inode, pid); ok {
		return image, true
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return executableImage{}, false
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".mesh-update-") || !strings.HasSuffix(entry.Name(), ".previous") {
			continue
		}
		candidate := filepath.Join(filepath.Dir(path), entry.Name())
		if image, ok := loadedMeshImage(candidate, device, inode, pid); ok {
			image.Installed = path
			return image, true
		}
	}
	return executableImage{}, false
}

func loadedMeshImage(path string, device, inode uint64, pid int) (executableImage, bool) {
	var stat unix.Stat_t
	if unix.Stat(path, &stat) != nil || stat.Ino != inode || darwinDevice(stat.Dev) != device {
		return executableImage{}, false
	}
	info, err := buildinfo.ReadFile(path)
	if err != nil || !strings.HasPrefix(info.Path, "github.com/shaul/mesh/") {
		return executableImage{}, false
	}
	return executableImage{Path: path, Installed: path, PID: pid}, true
}

func darwinDevice(device int32) uint64 {
	return uint64(uint32(device)) //nolint:gosec // dev_t is signed in Stat_t; lsof reports the same 32-bit identifier as unsigned hexadecimal
}
