package cli

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"sort"
	"syscall"
	"time"

	"github.com/charmbracelet/x/term"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/worker"
)

// Session is a local session as the CLI sees it: what the worker recorded,
// plus whether it is answering right now.
type Session struct {
	worker.Meta
	Dir   string
	Alive bool
}

// State returns the state to display, reconciling what the worker last wrote
// with what is actually true now.
func (s Session) State() string {
	switch {
	case s.Alive:
		return worker.StateRunning
	case s.Meta.State == worker.StateExited:
		return worker.StateExited
	default:
		// A session that claimed to be running but has no socket did not exit
		// cleanly: its worker was killed or the machine rebooted underneath it.
		return worker.StateInterrupted
	}
}

// List returns every session this host knows about, newest first.
func List() ([]Session, error) {
	root, err := paths.SessionsDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read sessions: %w", err)
	}

	var out []Session
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := root + string(os.PathSeparator) + e.Name()
		meta, err := worker.ReadMeta(dir)
		if err != nil {
			continue // half-created or hand-deleted; not our problem to report
		}
		out = append(out, Session{Meta: meta, Dir: dir, Alive: alive(dir)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// Find returns the session with the given ID.
func Find(id string) (Session, error) {
	id = session.NormalizeID(id)
	all, err := List()
	if err != nil {
		return Session{}, err
	}
	for _, s := range all {
		if s.ID == id {
			return s, nil
		}
	}
	return Session{}, fmt.Errorf("no session %s on this host", id)
}

// Latest returns the most recent session that is still answering.
func Latest() (Session, error) {
	all, err := List()
	if err != nil {
		return Session{}, err
	}
	for _, s := range all {
		if s.Alive {
			return s, nil
		}
	}
	return Session{}, fmt.Errorf("no live sessions on this host")
}

// alive reports whether a worker is listening on the session's socket. The
// socket file existing is not enough: a killed worker leaves one behind.
func alive(dir string) bool {
	conn, err := net.DialTimeout("unix", paths.Socket(dir), 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// Spawn starts a detached worker for command and returns the new session once
// its socket is accepting connections.
func Spawn(command []string, cwd string) (Session, error) {
	if len(command) == 0 {
		return Session{}, fmt.Errorf("spawn: no command")
	}
	id, err := session.NewID()
	if err != nil {
		return Session{}, err
	}
	dir, err := paths.SessionDir(id)
	if err != nil {
		return Session{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Session{}, fmt.Errorf("create session dir: %w", err)
	}

	exe, err := os.Executable()
	if err != nil {
		return Session{}, fmt.Errorf("locate mesh binary: %w", err)
	}
	logf, err := os.OpenFile(paths.Log(dir), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return Session{}, fmt.Errorf("open worker log: %w", err)
	}
	defer logf.Close() //nolint:errcheck // the child holds its own descriptor

	cols, rows := 80, 24
	if w, h, err := term.GetSize(os.Stdout.Fd()); err == nil && w > 0 {
		cols, rows = w, h
	}

	args := []string{"session-worker", "--id", id, "--dir", dir,
		"--cols", fmt.Sprint(cols), "--rows", fmt.Sprint(rows), "--"}
	args = append(args, command...)

	cmd := exec.Command(exe, args...)
	cmd.Dir = cwd
	cmd.Env = os.Environ()
	cmd.Stdin = nil
	cmd.Stdout = logf
	cmd.Stderr = logf
	// Setsid is the whole point: the worker leaves our session and loses our
	// controlling terminal, so closing the CLI cannot take it down.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return Session{}, fmt.Errorf("start worker: %w", err)
	}
	// We never Wait: the worker is not ours to supervise. Releasing lets it be
	// reparented to init when this process exits.
	_ = cmd.Process.Release()

	if err := waitForSocket(paths.Socket(dir), 5*time.Second); err != nil {
		return Session{}, fmt.Errorf("session %s never came up (see %s): %w", id, paths.Log(dir), err)
	}
	meta, err := worker.ReadMeta(dir)
	if err != nil {
		return Session{}, fmt.Errorf("read session %s: %w", id, err)
	}
	return Session{Meta: meta, Dir: dir, Alive: true}, nil
}

func waitForSocket(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.DialTimeout("unix", path, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
}
