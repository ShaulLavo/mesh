//go:build darwin

package worker

import (
	"context"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	darwinProcessObservationTimeout = 300 * time.Millisecond
	darwinProcessOutputLimit        = 8 << 10
	darwinProcessListOutputLimit    = 16 << 10
)

func defaultProcessObserver(ptyFD, leaderPID int) processObservation {
	ctx, cancel := context.WithTimeout(context.Background(), darwinProcessObservationTimeout)
	defer cancel()

	for _, pid := range darwinProcessObservationCandidateIDs(ctx, ptyFD, leaderPID) {
		observation := observeDarwinProcess(ctx, pid)
		if observation.directory != "" || observation.command != "" {
			return observation
		}
	}
	return processObservation{}
}

func darwinProcessObservationCandidateIDs(ctx context.Context, ptyFD, leaderPID int) []int {
	foregroundGroupID := foregroundProcessGroupID(ptyFD)
	var processes []observedProcessState
	if foregroundGroupID > 0 {
		output := runDarwinProcessTool(
			ctx,
			darwinProcessListOutputLimit,
			"/bin/ps",
			"-g", strconv.Itoa(foregroundGroupID),
			"-o", "pid=",
			"-o", "pgid=",
			"-o", "state=",
		)
		processes = parsePSProcessStates(output)
	}
	candidates := orderedLiveProcessGroupMemberIDs(foregroundGroupID, processes)
	return appendSessionLeaderFallback(candidates, leaderPID)
}

func observeDarwinProcess(ctx context.Context, pid int) processObservation {
	type result struct {
		kind  string
		value string
	}
	results := make(chan result, 2)
	var commands sync.WaitGroup
	commands.Add(2)
	go func() {
		defer commands.Done()
		output := runDarwinProcessTool(ctx, darwinProcessOutputLimit, "/usr/sbin/lsof", "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn")
		results <- result{kind: "directory", value: parseLsofDirectory(output)}
	}()
	go func() {
		defer commands.Done()
		output := runDarwinProcessTool(ctx, darwinProcessOutputLimit, "/bin/ps", "-p", strconv.Itoa(pid), "-o", "command=")
		if strings.TrimSpace(output) == "" {
			output = runDarwinProcessTool(ctx, darwinProcessOutputLimit, "/bin/ps", "-p", strconv.Itoa(pid), "-o", "comm=")
		}
		results <- result{kind: "command", value: strings.TrimSpace(output)}
	}()
	commands.Wait()
	close(results)

	var observation processObservation
	for result := range results {
		switch result.kind {
		case "directory":
			observation.directory = result.value
		case "command":
			observation.command = result.value
		}
	}
	return observation
}

func runDarwinProcessTool(ctx context.Context, limit int, path string, args ...string) string {
	if limit <= 0 {
		return ""
	}
	output := &boundedOutput{remaining: limit}
	command := exec.CommandContext(ctx, path, args...) //nolint:gosec // fixed system tools receive only a kernel-observed numeric PID
	command.Stdout = output
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return ""
	}
	return output.String()
}

func parseLsofDirectory(output string) string {
	for line := range strings.SplitSeq(output, "\n") {
		if strings.HasPrefix(line, "n/") {
			return strings.TrimPrefix(line, "n")
		}
	}
	return ""
}

type boundedOutput struct {
	contents  strings.Builder
	remaining int
}

func (output *boundedOutput) Write(p []byte) (int, error) {
	written := len(p)
	if len(p) > output.remaining {
		p = p[:output.remaining]
	}
	_, _ = output.contents.Write(p)
	output.remaining -= len(p)
	return written, nil
}

func (output *boundedOutput) String() string {
	return output.contents.String()
}
