//go:build linux

package worker

import (
	"fmt"
	"strconv"
	"strings"
)

func readAncestorProcess(pid int) (ancestorProcess, bool) {
	processDir := fmt.Sprintf("/proc/%d", pid)
	stat := string(readLinuxProcessFile(processDir+"/stat", linuxProcessStatLimit))
	close := strings.LastIndex(stat, ") ")
	if close < 0 {
		return ancestorProcess{}, false
	}
	fields := strings.Fields(stat[close+2:])
	if len(fields) < 2 {
		return ancestorProcess{}, false
	}
	parentID, err := strconv.Atoi(fields[1])
	if err != nil || parentID < 0 {
		return ancestorProcess{}, false
	}
	command := readLinuxProcessFile(processDir+"/cmdline", linuxProcessCommandLimit)
	command = []byte(strings.TrimRight(string(command), "\x00"))
	args := strings.Split(string(command), "\x00")
	if len(args) == 1 && args[0] == "" {
		args = nil
	}
	return ancestorProcess{parentID: parentID, args: args}, true
}
