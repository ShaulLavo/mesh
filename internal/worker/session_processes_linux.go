//go:build linux

package worker

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// cgroupProcsLimit bounds one scope's cgroup.procs; an app session with more
// processes than fit is refused rather than half attributed.
const cgroupProcsLimit = 256 << 10

// SessionProcesses lists the live processes of the session whose command is
// leader. A worker that moved into its own systemd scope (IsolateSession) has
// every descendant in that cgroup, including ones that left the kernel session
// with setsid. Without a scope, the members of the kernel session the command
// leads are the closest answer; a process that daemonized out of it is missed.
func SessionProcesses(id string, leader int) ([]int, error) {
	if leader <= 1 {
		return nil, fmt.Errorf("session %s: invalid command process %d", id, leader)
	}
	cgroup, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", leader))
	if err != nil {
		return nil, fmt.Errorf("session %s: read command cgroup: %w", id, err)
	}
	if path, ok := sessionScopePath(id, string(cgroup)); ok {
		return scopeProcesses(id, path)
	}
	return kernelSessionMembers(id, leader)
}

// sessionScopePath returns the session's own scope from a /proc/PID/cgroup
// file, or false when the process lives in some other unit.
func sessionScopePath(id, cgroup string) (string, bool) {
	path := unifiedCgroupPath(cgroup)
	if !strings.HasSuffix(path, "/"+sessionScopeUnit(id)) {
		return "", false
	}
	return path, true
}

func scopeProcesses(id, path string) ([]int, error) {
	file, err := os.Open("/sys/fs/cgroup" + path + "/cgroup.procs") //nolint:gosec // the path is this session's own scope, read from the kernel
	if err != nil {
		return nil, fmt.Errorf("session %s: open scope processes: %w", id, err)
	}
	defer file.Close() //nolint:errcheck // read-only
	contents, err := io.ReadAll(io.LimitReader(file, cgroupProcsLimit+1))
	if err != nil {
		return nil, fmt.Errorf("session %s: read scope processes: %w", id, err)
	}
	if len(contents) > cgroupProcsLimit {
		return nil, fmt.Errorf("session %s: scope process list exceeds %d bytes", id, cgroupProcsLimit)
	}
	return parseLinuxChildProcessIDs(contents), nil
}

func kernelSessionMembers(id string, leader int) ([]int, error) {
	proc, err := os.Open("/proc")
	if err != nil {
		return nil, fmt.Errorf("session %s: list processes: %w", id, err)
	}
	defer proc.Close() //nolint:errcheck // read-only
	names, err := proc.Readdirnames(-1)
	if err != nil {
		return nil, fmt.Errorf("session %s: list processes: %w", id, err)
	}
	var members []int
	for _, name := range names {
		pid, err := strconv.Atoi(name)
		if err != nil || pid <= 0 {
			continue
		}
		if session, ok := linuxProcessSession(pid); ok && session == leader {
			members = append(members, pid)
		}
	}
	if len(members) == 0 {
		return nil, errors.New("session " + id + ": command process is gone")
	}
	return members, nil
}

// linuxProcessSession reads the kernel session ID, the fourth field after the
// command name in /proc/PID/stat.
func linuxProcessSession(pid int) (int, bool) {
	stat := string(readLinuxProcessFile(fmt.Sprintf("/proc/%d/stat", pid), linuxProcessStatLimit))
	end := strings.LastIndex(stat, ") ")
	if end < 0 {
		return 0, false
	}
	fields := strings.Fields(stat[end+2:])
	if len(fields) < 4 {
		return 0, false
	}
	session, err := strconv.Atoi(fields[3])
	return session, err == nil && session > 0
}
