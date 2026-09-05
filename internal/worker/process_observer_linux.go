//go:build linux

package worker

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

const (
	linuxProcessCommandLimit  = 4 << 10
	linuxProcessStatLimit     = 4 << 10
	linuxProcessChildrenLimit = 16 << 10
	linuxProcessTreeLimit     = 1024
	linuxProcessScanLimit     = 16 << 10
	linuxProcessMemberLimit   = 64
)

func defaultProcessObserver(ptyFD, leaderPID int) processObservation {
	for _, pid := range linuxProcessObservationCandidateIDs(ptyFD, leaderPID) {
		observation := observeLinuxProcess(pid)
		if observation.directory != "" || observation.command != "" {
			return observation
		}
	}
	return processObservation{}
}

func linuxProcessObservationCandidateIDs(ptyFD, leaderPID int) []int {
	foregroundGroupID := foregroundProcessGroupID(ptyFD)
	processes := make([]observedProcessState, 0)
	if foregroundGroupID > 0 {
		if groupLeader, ok := readLinuxProcessState(foregroundGroupID); ok {
			processes = append(processes, groupLeader)
		}
		processes = append(processes, observeLinuxProcessTree(leaderPID)...)
	}
	candidates := orderedLiveProcessGroupMemberIDs(foregroundGroupID, processes)
	if foregroundGroupID > 0 && len(candidates) == 0 {
		candidates = orderedLiveProcessGroupMemberIDs(foregroundGroupID, scanLinuxProcessGroup(foregroundGroupID))
	}
	return appendSessionLeaderFallback(candidates, leaderPID)
}

func observeLinuxProcess(pid int) processObservation {
	processDir := fmt.Sprintf("/proc/%d", pid)
	directory, _ := os.Readlink(processDir + "/cwd")
	command := formatLinuxCommandLine(readLinuxProcessFile(processDir+"/cmdline", linuxProcessCommandLimit))
	if command == "" {
		command = strings.TrimSpace(string(readLinuxProcessFile(processDir+"/comm", linuxProcessCommandLimit)))
	}
	return processObservation{
		directory: directory,
		command:   command,
	}
}

func observeLinuxProcessTree(rootPID int) []observedProcessState {
	if rootPID <= 0 {
		return nil
	}
	queue := []int{rootPID}
	seen := make(map[int]struct{}, linuxProcessTreeLimit)
	processes := make([]observedProcessState, 0)
	for index := 0; index < len(queue) && len(seen) < linuxProcessTreeLimit; index++ {
		pid := queue[index]
		if pid <= 0 {
			continue
		}
		if _, exists := seen[pid]; exists {
			continue
		}
		seen[pid] = struct{}{}
		if process, ok := readLinuxProcessState(pid); ok {
			processes = append(processes, process)
		}
		childrenPath := fmt.Sprintf("/proc/%d/task/%d/children", pid, pid)
		children := parseLinuxChildProcessIDs(readLinuxProcessFile(childrenPath, linuxProcessChildrenLimit))
		for _, childPID := range children {
			if len(queue) >= linuxProcessTreeLimit {
				break
			}
			queue = append(queue, childPID)
		}
	}
	return processes
}

func readLinuxProcessState(pid int) (observedProcessState, bool) {
	filename := fmt.Sprintf("/proc/%d/stat", pid)
	return parseLinuxProcessState(readLinuxProcessFile(filename, linuxProcessStatLimit))
}

// scanLinuxProcessGroup is a bounded fallback for foreground jobs whose
// surviving members have been reparented outside the session leader's visible
// child tree. The common case stays on the much smaller targeted tree above.
func scanLinuxProcessGroup(groupID int) []observedProcessState {
	proc, err := os.Open("/proc")
	if err != nil {
		return nil
	}
	defer proc.Close() //nolint:errcheck // best-effort observation
	names, _ := proc.Readdirnames(linuxProcessScanLimit)
	processes := make([]observedProcessState, 0)
	for _, name := range names {
		pid, err := strconv.Atoi(name)
		if err != nil || pid <= 0 {
			continue
		}
		process, ok := readLinuxProcessState(pid)
		if !ok || !process.alive || process.groupID != groupID {
			continue
		}
		processes = append(processes, process)
		if len(processes) == linuxProcessMemberLimit {
			break
		}
	}
	return processes
}

func parseLinuxProcessState(contents []byte) (observedProcessState, bool) {
	text := string(contents)
	open := strings.IndexByte(text, '(')
	close := strings.LastIndex(text, ") ")
	if open <= 0 || close <= open {
		return observedProcessState{}, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(text[:open]))
	if err != nil || pid <= 0 {
		return observedProcessState{}, false
	}
	fields := strings.Fields(text[close+2:])
	if len(fields) < 3 {
		return observedProcessState{}, false
	}
	groupID, err := strconv.Atoi(fields[2])
	if err != nil || groupID <= 0 {
		return observedProcessState{}, false
	}
	return observedProcessState{
		pid:     pid,
		groupID: groupID,
		alive:   liveProcessState(fields[0]),
	}, true
}

func parseLinuxChildProcessIDs(contents []byte) []int {
	children := make([]int, 0)
	for _, field := range strings.Fields(string(contents)) {
		pid, err := strconv.Atoi(field)
		if err == nil && pid > 0 {
			children = append(children, pid)
		}
	}
	return children
}

func readLinuxProcessFile(filename string, limit int) []byte {
	if limit <= 0 {
		return nil
	}
	file, err := os.Open(filename) //nolint:gosec // fixed /proc fields for a kernel-observed process ID
	if err != nil {
		return nil
	}
	defer file.Close() //nolint:errcheck // best-effort observation
	contents, err := io.ReadAll(io.LimitReader(file, int64(limit)))
	if err != nil {
		return nil
	}
	return contents
}

func formatLinuxCommandLine(contents []byte) string {
	if len(contents) > linuxProcessCommandLimit {
		contents = contents[:linuxProcessCommandLimit]
	}
	return strings.TrimSpace(strings.ReplaceAll(string(contents), "\x00", " "))
}
