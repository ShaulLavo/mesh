package worker

import (
	"errors"
	"fmt"
	"io"
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
	"github.com/shaul/mesh/internal/session/execution"
	"github.com/shaul/mesh/internal/updategate"
)

const (
	workerReadyTimeout = 5 * time.Second
	sessionIDAttempts  = 16
)

// LaunchConfig describes a detached worker process. SessionsDir is explicit so
// daemon tests and recovery-mode clients can use the same launcher safely.
type LaunchConfig struct {
	Reservation    *session.Reservation
	Execution      *execution.Binding
	RecoveredFrom  string
	SessionsDir    string
	HostID         string
	Executable     string
	Command        []string
	Cwd            string
	Env            []string
	Cols           int
	Rows           int
	Term           string
	Depth          int
	ownerPublisher func(string, string) error
	Owner          string
	Label          string
}

// Launched is a worker that has published its metadata and is accepting local
// protocol connections.
type Launched struct {
	Meta Meta
	Dir  string
}

// StartedError is a launch that failed after the worker process started. The
// worker may still run its command, so whoever launched it keeps owning ID
// instead of treating the session as never started.
type StartedError struct {
	ID  string
	Err error
}

func (e *StartedError) Error() string { return e.Err.Error() }
func (e *StartedError) Unwrap() error { return e.Err }

// LaunchDetached starts a worker in its own process session and waits only for
// readiness. The worker is deliberately not supervised by the caller.
func LaunchDetached(cfg LaunchConfig) (launched Launched, launchErr error) {
	if err := updategate.Check(filepath.Dir(cfg.SessionsDir)); err != nil {
		return Launched{}, workerExecutionError("launch.go LaunchDetached", err)
	}
	started := false
	defer func() {
		if cfg.Reservation != nil && !started && launchErr != nil {
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

	if cfg.Reservation == nil {
		purpose := session.Generic
		if cfg.Label != "" || cfg.Owner != "" {
			purpose = session.Managed
		}
		ref, err := Reserve(cfg.SessionsDir, purpose)
		if err != nil {
			return Launched{}, err
		}
		cfg.Reservation = &ref
	}
	id, dir, err := launchSessionDir(cfg)
	if err != nil {
		return Launched{}, err
	}
	cfg.Env = withSessionIdentity(cfg.Env, cfg.HostID, id)
	cleanupReserved := cfg.Reservation.Purpose != session.Recovery && cfg.Reservation.Purpose != session.Execution
	defer func() {
		if cleanupReserved {
			launchErr = errors.Join(launchErr, cleanupLaunchReservation(dir, cfg.Owner != ""))
		}
	}()
	if cfg.Owner != "" {
		if err := cfg.publishOwner(dir); err != nil {
			return Launched{}, fmt.Errorf("launch worker %s: publish owner: %w", id, err)
		}
	}
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
		"--instance", cfg.Reservation.Instance, "--boot", cfg.Reservation.BootID, "--purpose", string(cfg.Reservation.Purpose),
	}
	if cfg.Execution != nil {
		args = append(args, "--request-hash", cfg.Execution.RequestHash, "--launch-digest", cfg.Execution.LaunchDigest)
	}
	if cfg.RecoveredFrom != "" {
		args = append(args, "--recovered-from", cfg.RecoveredFrom)
	}
	if cfg.Label != "" {
		args = append(args, "--label", cfg.Label)
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

	if err := waitForWorker(dir, workerReadyTimeout, cmd.Process); err != nil {
		return Launched{}, &StartedError{ID: id, Err: fmt.Errorf("launch worker %s: readiness (see %s): %w", id, logPath, err)}
	}
	meta, err := ReadMeta(dir)
	if err != nil {
		return Launched{}, &StartedError{ID: id, Err: fmt.Errorf("launch worker %s: read metadata: %w", id, err)}
	}
	return Launched{Meta: meta, Dir: dir}, nil
}

func launchSessionDir(cfg LaunchConfig) (string, string, error) {
	if cfg.Reservation == nil {
		return "", "", fmt.Errorf("launch worker: exact reservation required")
	}
	ref := *cfg.Reservation
	if err := validateReservation(cfg.SessionsDir, ref); err != nil {
		return "", "", err
	}
	if (ref.Purpose == session.Execution) != (cfg.Execution != nil) {
		return "", "", fmt.Errorf("launch worker: reservation purpose mismatch")
	}
	if cfg.RecoveredFrom != "" && ref.Purpose != session.Recovery {
		return "", "", fmt.Errorf("launch worker: recovery purpose required")
	}
	if cfg.Execution != nil {
		if err := validateExecutionLaunch(cfg, ref); err != nil {
			return "", "", err
		}
	}
	dir := filepath.Join(cfg.SessionsDir, ref.ID)
	if meta, err := ReadMeta(dir); err == nil {
		if ref.Purpose != session.Recovery || meta.BootID == "" || meta.BootID == ref.BootID {
			return "", "", fmt.Errorf("launch worker: reservation already published")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", err
	}
	return ref.ID, dir, nil
}

func waitForWorker(dir string, timeout time.Duration, process *os.Process) error {
	started := time.Now()
	deadline := started.Add(timeout)
	for {
		phase := "launch-marker-unreadable"
		_, markerErr := os.Lstat(paths.Launching(dir))
		if errors.Is(markerErr, os.ErrNotExist) {
			phase = "socket-dial-failing"
			conn, err := net.DialTimeout("unix", paths.Socket(dir), 200*time.Millisecond)
			if err == nil {
				_ = conn.Close()
				return nil
			}
			// A worker that no first attach holds open can publish, run a
			// command that exits at once, record the exit and close its socket
			// before this dial. That session launched; it has also ended.
			if meta, metaErr := ReadMeta(dir); metaErr == nil && meta.State == StateExited {
				return nil
			}
			markerErr = err
		} else if markerErr == nil {
			phase = "launch-marker-present"
			markerErr = fmt.Errorf("worker is still publishing state")
		}
		if time.Now().After(deadline) {
			return readinessTimeout(dir, process, started, deadline, phase, markerErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func cleanupLaunchReservation(dir string, owned bool) error {
	if !owned {
		_ = os.Remove(paths.Launching(dir))
		_ = os.Remove(paths.Log(dir))
		_ = os.Remove(filepath.Join(dir, ".reservation"))
		_ = os.Remove(filepath.Join(dir, ".barrier"))
		if err := os.Remove(dir); err != nil {
			return fmt.Errorf("remove reserved directory: %w", err)
		}
		return nil
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("open reservation: %w", err)
	}
	defer func() { _ = root.Close() }()
	var errs []error
	for _, name := range []string{".owner", ".launching", ".reservation", ".barrier", "worker.log"} {
		if err := root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("remove reservation files: %w", err)
	}
	if err := os.Remove(dir); err != nil {
		return fmt.Errorf("remove reservation: %w", err)
	}
	return syncOwnerDir(filepath.Dir(dir))
}

type ownerMarkerFile interface {
	Write([]byte) (int, error)
	Sync() error
	Close() error
}

type ownerMarkerIO struct {
	open    func(*os.Root) (ownerMarkerFile, error)
	syncDir func(string) error
}

func (cfg LaunchConfig) publishOwner(dir string) error {
	publish := cfg.ownerPublisher
	if publish == nil {
		publish = writeOwnerMarker
	}
	return publish(dir, cfg.Owner)
}

func writeOwnerMarker(dir, marker string) error {
	return publishOwnerMarker(dir, marker, ownerMarkerIO{
		open: func(root *os.Root) (ownerMarkerFile, error) {
			return root.OpenFile(".owner", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		},
		syncDir: syncOwnerDir,
	})
}

func publishOwnerMarker(dir, marker string, ops ownerMarkerIO) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("open owner directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	file, err := ops.open(root)
	if err != nil {
		return fmt.Errorf("create owner marker: %w", err)
	}
	written, writeErr := file.Write([]byte(marker))
	if written != len(marker) && writeErr == nil {
		writeErr = io.ErrShortWrite
	}
	syncErr := file.Sync()
	if err := errors.Join(writeErr, syncErr, file.Close()); err != nil {
		return fmt.Errorf("write owner marker: %w", err)
	}
	if err := ops.syncDir(dir); err != nil {
		return err
	}
	return ops.syncDir(filepath.Dir(dir))
}

func syncOwnerDir(dir string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("open directory for owner sync: %w", err)
	}
	defer func() { _ = root.Close() }()
	file, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("open owner sync: %w", err)
	}
	return errors.Join(file.Sync(), file.Close())
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
	// MeshTerminalIDVariable names the terminal tab a Mesh client runs in. It
	// is stripped here, never set: a client inside a session must not inherit
	// the outer tab's identity and rebind it to a nested session.
	MeshTerminalIDVariable = "MESH_TERMINAL_ID"
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
	terminalPrefix := MeshTerminalIDVariable + "="
	out := make([]string, 0, len(env)+2)
	for _, entry := range env {
		if !strings.HasPrefix(entry, hostPrefix) && !strings.HasPrefix(entry, sessionPrefix) && !strings.HasPrefix(entry, terminalPrefix) {
			out = append(out, entry)
		}
	}
	if hostID != "" {
		out = append(out, hostPrefix+hostID)
	}
	return append(out, sessionPrefix+sessionID)
}
