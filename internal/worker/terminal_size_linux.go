//go:build linux

package worker

import (
	"fmt"
	"os"
	"strings"
)

func readSessionLeaderTerminalSize(pid int) (cols, rows int, ok bool) {
	device, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/0", pid))
	if err != nil || !linuxPTYSlavePath(device) {
		return 0, 0, false
	}
	return readTerminalDeviceSize(device)
}

func linuxPTYSlavePath(path string) bool {
	const prefix = "/dev/pts/"
	name, found := strings.CutPrefix(path, prefix)
	if !found || name == "" {
		return false
	}
	for _, character := range name {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}
