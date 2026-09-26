//go:build linux

package daemon

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// portHolder names the process listening on a TCP port, reading /proc the way
// ss -p does. A process owned by another user hides its descriptors, so the
// answer then says only that the port is taken.
func portHolder(port uint16) string {
	inodes := listeningInodes(port)
	if len(inodes) == 0 {
		return "another process"
	}
	processes, err := os.ReadDir("/proc")
	if err != nil {
		return "another process"
	}
	for _, process := range processes {
		pid, err := strconv.Atoi(process.Name())
		if err != nil {
			continue
		}
		descriptors, err := os.ReadDir(filepath.Join("/proc", process.Name(), "fd"))
		if err != nil {
			continue
		}
		for _, descriptor := range descriptors {
			target, err := os.Readlink(filepath.Join("/proc", process.Name(), "fd", descriptor.Name()))
			if err != nil || !strings.HasPrefix(target, "socket:[") {
				continue
			}
			if _, held := inodes[strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")]; held {
				return describeProcess(pid)
			}
		}
	}
	return "a process this user cannot inspect"
}

func listeningInodes(port uint16) map[string]struct{} {
	inodes := make(map[string]struct{})
	wanted := fmt.Sprintf(":%04X", port)
	for _, table := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		file, err := os.Open(table) //nolint:gosec // one of two fixed kernel tables
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			// local_address is field 1, state field 3 (0A is LISTEN), inode field 9.
			if len(fields) < 10 || fields[3] != "0A" || !strings.HasSuffix(fields[1], wanted) {
				continue
			}
			inodes[fields[9]] = struct{}{}
		}
		_ = file.Close()
	}
	return inodes
}

func describeProcess(pid int) string {
	command := ""
	if cmdline, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline")); err == nil {
		command = strings.TrimSpace(strings.ReplaceAll(string(cmdline), "\x00", " "))
	}
	if command == "" {
		if comm, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "comm")); err == nil {
			command = strings.TrimSpace(string(comm))
		}
	}
	if command == "" {
		return fmt.Sprintf("pid %d", pid)
	}
	if len(command) > 120 {
		command = command[:120] + "…"
	}
	return fmt.Sprintf("pid %d (%s)", pid, command)
}
