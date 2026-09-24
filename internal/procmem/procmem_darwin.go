package procmem

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Snapshot reads parentage, resident size and command lines from one ps
// invocation. macOS exposes no proportional size without elevated access.
func Snapshot() Table {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "/bin/ps", "-A", "-ww", "-o", "pid=,ppid=,rss=,command=").Output()
	if err != nil {
		return Table{}
	}
	children := make(map[int][]int)
	parents := make(map[int]int)
	resident := make(map[int]uint64)
	commands := make(map[int][]string)
	for _, line := range strings.Split(string(output), "\n") {
		// ps separates arguments with spaces, which is exact for the fixed
		// worker argv this is used to recognise and merely conservative for
		// arguments that themselves contain spaces.
		fields := strings.Fields(line)
		if len(fields) < 4 {
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
		commands[pid] = fields[3:]
	}
	return Table{parents: parents, children: children,
		sample: func(pid int) (uint64, bool) {
			bytes, ok := resident[pid]
			return bytes, ok
		},
		command: func(pid int) []string { return commands[pid] },
	}
}
