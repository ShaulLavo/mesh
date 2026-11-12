package updatebootstrap

import (
	"errors"
	"strconv"
	"strings"
)

// Attribute all output before resolving any file so a bad trailing header cannot
// leave earlier processes accepted from an untrustworthy command result.
func mappedProcessSets(output string, pids []int) ([]string, error) {
	indices := make(map[int]int, len(pids))
	for i, pid := range pids {
		indices[pid] = i
	}
	sets := make([]strings.Builder, len(pids))
	seen := make(map[int]bool, len(pids))
	current := -1
	for _, line := range strings.Split(output, "\n") {
		if line == "" {
			continue
		}
		if line[0] != 'p' && current < 0 {
			return nil, errors.New("mapped process records precede process identity")
		}
		if line[0] != 'p' {
			sets[current].WriteString(line)
			sets[current].WriteByte('\n')
			continue
		}
		pid, err := mappedProcessPID(line[1:])
		index, requested := indices[pid]
		if err != nil || !requested || seen[pid] {
			return nil, errors.New("cannot attribute mapped process image records")
		}
		seen[pid] = true
		current = index
	}
	outputSets := make([]string, len(sets))
	for i := range sets {
		outputSets[i] = sets[i].String()
	}
	return outputSets, nil
}

func mappedProcessPID(value string) (int, error) {
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return 0, errors.New("invalid mapped process ID")
		}
	}
	pid, err := strconv.Atoi(value)
	if err != nil || pid <= 0 {
		return 0, errors.New("invalid mapped process ID")
	}
	return pid, nil
}
