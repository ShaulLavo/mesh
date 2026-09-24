package procmem

import (
	"bufio"
	"bytes"
	"os"
	"strconv"
	"strings"
)

// Snapshot reads every process's parent once from /proc.
func Snapshot() Table {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return Table{}
	}
	children := make(map[int][]int)
	parents := make(map[int]int)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		parent, ok := linuxParent(pid)
		if !ok {
			continue
		}
		children[parent] = append(children[parent], pid)
		parents[pid] = parent
	}
	return Table{parents: parents, children: children, sample: linuxProportional}
}

func linuxParent(pid int) (int, bool) {
	contents, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	// The command name is parenthesised and may itself contain spaces or
	// parentheses, so fields are counted from the last closing one.
	end := bytes.LastIndexByte(contents, ')')
	if end < 0 {
		return 0, false
	}
	fields := strings.Fields(string(contents[end+1:]))
	if len(fields) < 2 {
		return 0, false
	}
	parent, err := strconv.Atoi(fields[1])
	return parent, err == nil
}

// linuxProportional counts resident and swapped pages, each divided among the
// processes sharing them, so summing sessions never counts a page twice.
func linuxProportional(pid int) (uint64, bool) {
	file, err := os.Open("/proc/" + strconv.Itoa(pid) + "/smaps_rollup")
	if err != nil {
		return 0, false
	}
	defer file.Close() //nolint:errcheck // read-only procfs handle
	var total uint64
	found := false
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		name, rest, ok := strings.Cut(line, ":")
		if !ok || name != "Pss" && name != "SwapPss" {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) != 2 || fields[1] != "kB" {
			continue
		}
		kilobytes, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		total += kilobytes << 10
		found = true
	}
	return total, found
}
