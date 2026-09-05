//go:build linux || darwin

package worker

import (
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

type observedProcessState struct {
	pid     int
	groupID int
	alive   bool
}

func foregroundProcessGroupID(ptyFD int) int {
	if ptyFD < 0 {
		return 0
	}
	groupID, err := unix.IoctlGetInt(ptyFD, unix.TIOCGPGRP)
	if err != nil || groupID <= 0 {
		return 0
	}
	return groupID
}

func orderedLiveProcessGroupMemberIDs(groupID int, processes []observedProcessState) []int {
	if groupID <= 0 {
		return nil
	}
	leaderAlive := false
	members := make([]int, 0, len(processes))
	seen := make(map[int]struct{}, len(processes))
	for _, process := range processes {
		if !process.alive || process.groupID != groupID || process.pid <= 0 {
			continue
		}
		if _, exists := seen[process.pid]; exists {
			continue
		}
		seen[process.pid] = struct{}{}
		if process.pid == groupID {
			leaderAlive = true
			continue
		}
		members = append(members, process.pid)
	}
	slices.Sort(members)
	if leaderAlive {
		members = append([]int{groupID}, members...)
	}
	return members
}

func appendSessionLeaderFallback(candidates []int, leaderPID int) []int {
	if leaderPID <= 0 || slices.Contains(candidates, leaderPID) {
		return candidates
	}
	return append(candidates, leaderPID)
}

func parsePSProcessStates(output string) []observedProcessState {
	processes := make([]observedProcessState, 0)
	for line := range strings.SplitSeq(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 0 {
			continue
		}
		groupID, err := strconv.Atoi(fields[1])
		if err != nil || groupID <= 0 {
			continue
		}
		processes = append(processes, observedProcessState{
			pid:     pid,
			groupID: groupID,
			alive:   liveProcessState(fields[2]),
		})
	}
	return processes
}

func liveProcessState(state string) bool {
	if state == "" {
		return false
	}
	switch state[0] {
	case 'X', 'x', 'Z':
		return false
	default:
		return true
	}
}
