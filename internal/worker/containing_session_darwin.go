//go:build darwin

package worker

import (
	"context"
	"strconv"
	"strings"
	"time"
)

func readAncestorProcess(pid int) (ancestorProcess, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	output := runDarwinProcessTool(
		ctx,
		darwinProcessOutputLimit,
		"/bin/ps",
		"-ww",
		"-p", strconv.Itoa(pid),
		"-o", "ppid=",
		"-o", "command=",
	)
	fields := strings.Fields(output)
	if len(fields) < 2 {
		return ancestorProcess{}, false
	}
	parentID, err := strconv.Atoi(fields[0])
	if err != nil || parentID < 0 {
		return ancestorProcess{}, false
	}
	return ancestorProcess{parentID: parentID, args: fields[1:]}, true
}
