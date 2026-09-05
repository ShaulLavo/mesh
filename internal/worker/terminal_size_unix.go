//go:build linux || darwin

package worker

import (
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const terminalDeviceOpenFlags = unix.O_RDONLY |
	unix.O_NOCTTY |
	unix.O_NONBLOCK |
	unix.O_CLOEXEC |
	unix.O_NOFOLLOW

func readTerminalDeviceSize(path string) (cols, rows int, ok bool) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || !strings.HasPrefix(path, "/dev/") {
		return 0, 0, false
	}
	fd, err := unix.Open(path, terminalDeviceOpenFlags, 0)
	if err != nil {
		return 0, 0, false
	}
	defer func() { _ = unix.Close(fd) }()

	var status unix.Stat_t
	if err := unix.Fstat(fd, &status); err != nil || status.Mode&unix.S_IFMT != unix.S_IFCHR {
		return 0, 0, false
	}
	winsize, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
	if err != nil {
		return 0, 0, false
	}
	return int(winsize.Col), int(winsize.Row), true
}
