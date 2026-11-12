package updatebootstrap

import (
	"context"
	"debug/buildinfo"
	"errors"
	"fmt"
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

func peerPID(conn net.Conn) (int, error) {
	socket, ok := conn.(syscall.Conn)
	if !ok {
		return 0, errors.New("bootstrap requires a local Unix socket")
	}
	raw, err := socket.SyscallConn()
	if err != nil {
		return 0, err
	}
	var pid int
	var probeErr error
	if err = raw.Control(func(fd uintptr) { pid, probeErr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID) }); err != nil {
		return 0, err
	}
	if probeErr != nil {
		return 0, probeErr
	}
	return pid, nil
}

var mappedImageCommand = func(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "lsof", args...).Output() //nolint:gosec // fixed native tool; flags and kernel peer PIDs are constructed locally
}

func processImage(ctx context.Context, pid int) (executableImage, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	output, err := mappedImageCommand(ctx, "-a", "-p", strconv.Itoa(pid), "-d", "txt", "-F", "finD")
	if err != nil || ctx.Err() != nil {
		return executableImage{}, fmt.Errorf("inspect mapped process image: %w", errors.Join(ctx.Err(), err))
	}
	return parseMappedMeshImage(string(output), pid)
}

func processImages(ctx context.Context, pids []int) []processImageResult {
	results := make([]processImageResult, len(pids))
	if len(pids) == 0 {
		return results
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	names := make([]string, len(pids))
	for i, pid := range pids {
		names[i] = strconv.Itoa(pid)
	}
	output, commandErr := mappedImageCommand(ctx, "-a", "-p", strings.Join(names, ","), "-d", "txt", "-F", "pfinD")
	if err := mappedCommandFailure(ctx, commandErr); err != nil {
		return failedProcessImages(results, err)
	}
	sets, err := mappedProcessSets(string(output), pids)
	if err != nil {
		return failedProcessImages(results, errors.Join(err, commandErr))
	}
	for i, pid := range pids {
		results[i].image, results[i].err = parseMappedMeshImage(sets[i], pid)
		if results[i].err != nil {
			results[i].err = errors.Join(results[i].err, commandErr)
		}
	}
	return results
}

func mappedCommandFailure(ctx context.Context, err error) error {
	var exit *exec.ExitError
	if ctx.Err() == nil && (err == nil || errors.As(err, &exit) && exit.ExitCode() == 1) {
		return nil
	}
	return fmt.Errorf("inspect mapped process image: %w", errors.Join(ctx.Err(), err))
}

func failedProcessImages(results []processImageResult, err error) []processImageResult {
	for i := range results {
		results[i].err = err
	}
	return results
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
