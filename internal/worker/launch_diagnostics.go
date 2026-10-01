package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/shaul/mesh/internal/paths"
)

const (
	readinessDiagnosticBytes = 4 << 10
	readinessUnknown         = "unknown"
)

func readinessTimeout(dir string, process *os.Process, started, deadline time.Time, phase string, cause error) error {
	elapsed := time.Since(started)
	pid, alive := readinessProcess(process)
	state := readinessMetaState(dir)
	log := readinessStartupLog(dir)
	return fmt.Errorf("%w (elapsed=%s deadline=%s phase=%s workerPID=%d workerAlive=%s metaState=%s startupLog=%s)",
		cause, elapsed, deadline.UTC().Format(time.RFC3339Nano), phase, pid, alive, state, log)
}

func readinessProcess(process *os.Process) (int, string) {
	if process == nil {
		return 0, readinessUnknown
	}
	// Keep the original process handle: metadata names the PTY child, and a
	// numeric PID alone could name a different process after the worker is reaped.
	err := process.Signal(syscall.Signal(0))
	if err == nil || errors.Is(err, syscall.EPERM) {
		return process.Pid, "true"
	}
	if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
		return process.Pid, "false"
	}
	return process.Pid, readinessUnknown
}

func readinessMetaState(dir string) string {
	data, err := readReadinessFile(filepath.Join(dir, "meta.json"), false)
	if err != nil {
		return readinessUnknown
	}
	var meta struct {
		State string `json:"state"`
	}
	if json.Unmarshal(data, &meta) != nil {
		return readinessUnknown
	}
	switch meta.State {
	case StateRunning, StateDetached, StateExited, StateInterrupted:
		return meta.State
	default:
		return readinessUnknown
	}
}

func readinessStartupLog(dir string) string {
	data, err := readReadinessFile(paths.Log(dir), true)
	if err != nil {
		return "unavailable"
	}
	if len(data) == 0 {
		return "empty"
	}
	var facts []string
	for line := range strings.SplitSeq(string(data), "\n") {
		fact := readinessLogFact(line)
		if fact != "" && !slices.Contains(facts, fact) {
			facts = append(facts, fact)
		}
	}
	if len(facts) == 0 {
		return "unclassified"
	}
	return strings.Join(facts, ",")
}

func readinessLogFact(line string) string {
	_, message, ok := strings.Cut(line, "worker: ")
	if !ok {
		return ""
	}
	// Emit operation identities only; worker errors can contain user commands,
	// environment values or subprocess output after these fixed prefixes.
	switch {
	case strings.HasPrefix(message, "own scope "):
		_, detail, _ := strings.Cut(message, ".scope: ")
		if strings.HasPrefix(detail, "not joined within ") {
			return "scope-not-joined"
		}
		return "scope-call-failed"
	case strings.HasPrefix(message, "open pty: "):
		return "pty-open-failed"
	case strings.HasPrefix(message, "start "):
		return "child-start-failed"
	case strings.HasPrefix(message, "write meta: "):
		return "meta-write-failed"
	case strings.HasPrefix(message, "sync meta: "):
		return "meta-sync-failed"
	case strings.HasPrefix(message, "close meta: "):
		return "meta-close-failed"
	case strings.HasPrefix(message, "commit meta: "):
		return "meta-commit-failed"
	case strings.HasPrefix(message, "sync session directory: "):
		return "directory-sync-failed"
	case strings.HasPrefix(message, "listen on "):
		return "socket-listen-failed"
	case strings.HasPrefix(message, "publish session "):
		return "session-publish-failed"
	default:
		return ""
	}
}

func readReadinessFile(path string, tail bool) ([]byte, error) {
	// Atomic no-follow/nonblocking open prevents a replaced FIFO or symlink
	// from turning timeout reporting into another wait or an unrelated read.
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0) //nolint:gosec // fixed worker-owned log or metadata path
	if err != nil {
		return nil, fmt.Errorf("worker: open readiness file: %w", err)
	}
	defer file.Close() //nolint:errcheck // diagnostic read result takes precedence
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("worker: inspect readiness file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("worker: readiness file is not regular")
	}
	length := min(info.Size(), int64(readinessDiagnosticBytes))
	offset := int64(0)
	if tail {
		offset = info.Size() - length
	}
	data := make([]byte, length)
	n, err := file.ReadAt(data, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("worker: read readiness file: %w", err)
	}
	return data[:n], nil
}
