package worker

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/recovery"
	"github.com/shaul/mesh/internal/session"
)

const (
	workerReadyTimeout = 5 * time.Second
	sessionIDAttempts  = 16
)

// LaunchConfig describes a detached worker process. SessionsDir is explicit so
// daemon tests and recovery-mode clients can use the same launcher safely.
type LaunchConfig struct {
	ReservedID    string
	RecoveredFrom string
	SessionsDir   string
	HostID        string
	Executable    string
	Command       []string
	Cwd           string
	Env           []string
	Cols          int
	Rows          int
	Term          string
	Depth         int
}

// Launched is a worker that has published its metadata and is accepting local
// protocol connections.
type Launched struct {
	Meta Meta
	Dir  string
}

// LaunchDetached starts a worker in its own process session and waits only for
// readiness. The worker is deliberately not supervised by the caller.
func LaunchDetached(cfg LaunchConfig) (launched Launched, launchErr error) {
	started := false
	defer func() {
		if cfg.ReservedID != "" && !started && launchErr != nil {
			launchErr = &recovery.LaunchFailure{Err: launchErr}
		}
	}()
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
	cfg.Env = withTerm(cfg.Env, cfg.Term)
	cfg.Env = withDepth(cfg.Env, cfg.Depth)
	if err := os.MkdirAll(cfg.SessionsDir, 0o700); err != nil {
		return Launched{}, fmt.Errorf("launch worker: create sessions directory: %w", err)
	}

	id, dir, err := launchSessionDir(cfg)
	if err != nil {
		return Launched{}, err
	}
	cfg.Env = withSessionIdentity(cfg.Env, cfg.HostID, id)
	cleanupReserved := cfg.ReservedID == ""
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
	}
	if cfg.RecoveredFrom != "" {
		args = append(args, "--recovered-from", cfg.RecoveredFrom)
	}
	args = append(args, "--")
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
	started = true
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

func launchSessionDir(cfg LaunchConfig) (string, string, error) {
	if cfg.ReservedID == "" {
		return reserveSessionDir(cfg.SessionsDir)
	}
	id, err := session.ParseID(cfg.ReservedID)
	if err != nil || id != cfg.ReservedID {
		return "", "", fmt.Errorf("launch worker: invalid reserved session ID %q", cfg.ReservedID)
	}
	dir := filepath.Join(cfg.SessionsDir, id)
	if _, err := os.Lstat(paths.Launching(dir)); err != nil {
		return "", "", fmt.Errorf("launch worker %s: read reservation: %w", id, err)
	}
	if err := validateReservedMetadata(dir, id); err != nil {
		return "", "", err
	}
	return id, dir, nil
}

func validateReservedMetadata(dir, id string) error {
	meta, err := ReadMeta(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("launch worker %s: read reserved metadata: %w", id, err)
	}
	boot := BootID()
	if meta.ID == id && meta.BootID != "" && boot != "" && meta.BootID != boot {
		return nil
	}
	return fmt.Errorf("launch worker %s: reserved directory already contains current metadata", id)
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

// withTerm sets TERM for the session. The daemon runs as a service and has no
// TERM, so without this the shell starts with none and startup code that
// branches on the terminal type takes the wrong path silently: on Arch, the
// prompt falls back to the /etc/bash.bashrc default.
func withTerm(env []string, term string) []string {
	if term == "" {
		return env
	}
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(entry, "TERM=") {
			out = append(out, entry)
		}
	}
	return append(out, "TERM="+term)
}

// MeshDepthVariable tells a Mesh client started inside a session how deep it
// is. Without it a nested client cannot know that something upstream is already
// reading its keystrokes.
const MeshDepthVariable = "MESH_DEPTH"

const (
	MeshHostIDVariable    = "MESH_HOST_ID"
	MeshSessionIDVariable = "MESH_SESSION_ID"
)

func withDepth(env []string, depth int) []string {
	if depth <= 0 {
		return env
	}
	out := make([]string, 0, len(env)+1)
	prefix := MeshDepthVariable + "="
	for _, entry := range env {
		if !strings.HasPrefix(entry, prefix) {
			out = append(out, entry)
		}
	}
	return append(out, prefix+strconv.Itoa(depth))
}

func withSessionIdentity(env []string, hostID, sessionID string) []string {
	hostPrefix := MeshHostIDVariable + "="
	sessionPrefix := MeshSessionIDVariable + "="
	out := make([]string, 0, len(env)+2)
	for _, entry := range env {
		if !strings.HasPrefix(entry, hostPrefix) && !strings.HasPrefix(entry, sessionPrefix) {
			out = append(out, entry)
		}
	}
	if hostID != "" {
		out = append(out, hostPrefix+hostID)
	}
	return append(out, sessionPrefix+sessionID)
}
