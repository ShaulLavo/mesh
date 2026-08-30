package worker

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/session"
)

const (
	workerReadyTimeout = 5 * time.Second
	sessionIDAttempts  = 16
)

// LaunchConfig describes a detached worker process. SessionsDir is explicit so
// daemon tests and recovery-mode clients can use the same launcher safely.
type LaunchConfig struct {
	SessionsDir string
	Executable  string
	Command     []string
	Cwd         string
	Env         []string
	Cols        int
	Rows        int
}

// Launched is a worker that has published its metadata and is accepting local
// protocol connections.
type Launched struct {
	Meta Meta
	Dir  string
}

// LaunchDetached starts a worker in its own process session and waits only for
// readiness. The worker is deliberately not supervised by the caller.
func LaunchDetached(cfg LaunchConfig) (Launched, error) {
	if cfg.SessionsDir == "" {
		return Launched{}, fmt.Errorf("launch worker: empty sessions directory")
	}
	if len(cfg.Command) == 0 || cfg.Command[0] == "" {
		return Launched{}, fmt.Errorf("launch worker: no command")
	}
	if cfg.Cols <= 0 {
		cfg.Cols = 80
	}
	if cfg.Rows <= 0 {
		cfg.Rows = 24
	}
	if err := validateTerminalSize(cfg.Cols, cfg.Rows); err != nil {
		return Launched{}, fmt.Errorf("launch worker: invalid terminal size: %w", err)
	}
	if cfg.Executable == "" {
		var err error
		cfg.Executable, err = os.Executable()
		if err != nil {
			return Launched{}, fmt.Errorf("launch worker: locate mesh binary: %w", err)
		}
	}
	if cfg.Env == nil {
		cfg.Env = os.Environ()
	}
	if err := os.MkdirAll(cfg.SessionsDir, 0o700); err != nil {
		return Launched{}, fmt.Errorf("launch worker: create sessions directory: %w", err)
	}

	id, dir, err := reserveSessionDir(cfg.SessionsDir)
	if err != nil {
		return Launched{}, err
	}
	cleanupReserved := true
	defer func() {
		if cleanupReserved {
			cleanupReservedSession(dir)
		}
	}()
	logPath := paths.Log(dir)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // path is the fixed log file in a newly reserved private session directory
	if err != nil {
		return Launched{}, fmt.Errorf("launch worker %s: open log %s: %w", id, logPath, err)
	}
	defer logFile.Close() //nolint:errcheck // the child owns a duplicate descriptor after Start

	args := []string{
		"session-worker",
		"--id", id,
		"--dir", dir,
		"--cols", fmt.Sprint(cfg.Cols),
		"--rows", fmt.Sprint(cfg.Rows),
		"--",
	}
	args = append(args, cfg.Command...)
	cmd := exec.Command(cfg.Executable, args...) //nolint:gosec // executable is the running Mesh binary or an explicit internal test override; no shell is used
	cmd.Dir = cfg.Cwd
	cmd.Env = append([]string(nil), cfg.Env...)
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return Launched{}, fmt.Errorf("launch worker %s: start: %w", id, err)
	}
	// From this point the worker may own a live command. Never remove its state;
	// the launching marker keeps incomplete state out of catalog scans, and the
	// worker removes it only after metadata and the socket are both ready.
	cleanupReserved = false
	// Waiting only reaps the worker. Setsid keeps the session independent when
	// the launcher exits, and meta.json remains authoritative for its outcome.
	go func() { _ = cmd.Wait() }()

	if err := waitForWorker(paths.Socket(dir), paths.Launching(dir), workerReadyTimeout); err != nil {
		return Launched{}, fmt.Errorf("launch worker %s: readiness (see %s): %w", id, logPath, err)
	}
	meta, err := ReadMeta(dir)
	if err != nil {
		return Launched{}, fmt.Errorf("launch worker %s: read metadata: %w", id, err)
	}
	return Launched{Meta: meta, Dir: dir}, nil
}

func reserveSessionDir(root string) (string, string, error) {
	for range sessionIDAttempts {
		id, err := session.NewID()
		if err != nil {
			return "", "", err
		}
		dir := filepath.Join(root, id)
		if err := os.Mkdir(dir, 0o700); err == nil {
			if err := os.WriteFile(paths.Launching(dir), nil, 0o600); err != nil {
				_ = os.Remove(dir)
				return "", "", fmt.Errorf("launch worker %s: mark reserved session directory: %w", id, err)
			}
			return id, dir, nil
		} else if !os.IsExist(err) {
			return "", "", fmt.Errorf("launch worker %s: create session directory: %w", id, err)
		}
	}
	return "", "", fmt.Errorf("launch worker: could not reserve a session ID after %d attempts", sessionIDAttempts)
}

func waitForWorker(socketPath, launchingPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		_, markerErr := os.Lstat(launchingPath)
		if errors.Is(markerErr, os.ErrNotExist) {
			conn, err := net.DialTimeout("unix", socketPath, 200*time.Millisecond)
			if err == nil {
				_ = conn.Close()
				return nil
			}
			markerErr = err
		} else if markerErr == nil {
			markerErr = fmt.Errorf("worker is still publishing state")
		}
		if time.Now().After(deadline) {
			return markerErr
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func cleanupReservedSession(dir string) {
	_ = os.Remove(paths.Launching(dir))
	_ = os.Remove(paths.Log(dir))
	_ = os.Remove(dir)
}
