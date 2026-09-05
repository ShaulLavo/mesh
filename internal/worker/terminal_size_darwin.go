//go:build darwin

package worker

import (
	"context"
	"strconv"
	"strings"
	"time"
)

const (
	darwinTerminalSizeTimeout     = 300 * time.Millisecond
	darwinTerminalSizeOutputLimit = 4 << 10
)

func readSessionLeaderTerminalSize(pid int) (cols, rows int, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), darwinTerminalSizeTimeout)
	defer cancel()
	output := runDarwinProcessTool(
		ctx,
		darwinTerminalSizeOutputLimit,
		"/usr/sbin/lsof",
		"-a", "-p", strconv.Itoa(pid), "-d", "0", "-Fn",
	)
	device := darwinTerminalDeviceFromLsof(output)
	if device == "" {
		return 0, 0, false
	}
	return readTerminalDeviceSize(device)
}

func darwinTerminalDeviceFromLsof(output string) string {
	device := ""
	names := 0
	for line := range strings.SplitSeq(output, "\n") {
		path, found := strings.CutPrefix(line, "n")
		if !found {
			continue
		}
		names++
		if names > 1 || !darwinPTYSlavePath(path) {
			return ""
		}
		device = path
	}
	return device
}

func darwinPTYSlavePath(path string) bool {
	const prefix = "/dev/ttys"
	name, found := strings.CutPrefix(path, prefix)
	if !found || name == "" {
		return false
	}
	for _, character := range name {
		if character >= '0' && character <= '9' ||
			character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' {
			continue
		}
		return false
	}
	return true
}
