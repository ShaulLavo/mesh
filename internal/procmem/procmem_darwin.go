package procmem

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Snapshot reads parentage and resident size from one ps invocation. macOS
// exposes no proportional size without elevated access.
func Snapshot() Table {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "/bin/ps", "-A", "-o", "pid=,ppid=,rss=").Output()
	if err != nil {
		return Table{}
	}
	children := make(map[int][]int)
	parents := make(map[int]int)
	resident := make(map[int]uint64)
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		parent, parentErr := strconv.Atoi(fields[1])
		kilobytes, sizeErr := strconv.ParseUint(fields[2], 10, 64)
		if pidErr != nil || parentErr != nil || sizeErr != nil {
			continue
		}
		children[parent] = append(children[parent], pid)
		parents[pid] = parent
		resident[pid] = kilobytes << 10
	}
	return Table{parents: parents, children: children, sample: func(pid int) (uint64, bool) {
		bytes, ok := resident[pid]
		return bytes, ok
	}}
}

// commandLine splits on spaces, which is exact for the fixed worker argv this
// is used to recognise and merely conservative for anything else.
func commandLine(pid int) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "/bin/ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return nil
	}
	return strings.Fields(string(output))
}
