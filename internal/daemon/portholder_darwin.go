//go:build darwin

package daemon

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// portHolder names the process listening on a TCP port. macOS has no /proc,
// and lsof is part of the base system.
func portHolder(port uint16) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "/usr/sbin/lsof", "-nP", "-iTCP:"+strconv.Itoa(int(port)), "-sTCP:LISTEN", "-Fpc").Output() //nolint:gosec // fixed binary; the only argument is a formatted port number
	if err != nil {
		return "another process"
	}
	pid, command := "", ""
	for _, line := range strings.Split(string(output), "\n") {
		switch {
		case strings.HasPrefix(line, "p") && pid == "":
			pid = line[1:]
		case strings.HasPrefix(line, "c") && command == "":
			command = line[1:]
		}
	}
	if pid == "" {
		return "another process"
	}
	if command == "" {
		return "pid " + pid
	}
	return fmt.Sprintf("pid %s (%s)", pid, command)
}
