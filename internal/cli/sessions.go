package cli

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
		dir := filepath.Join(root, e.Name())
		// A directory still carrying the launching marker has not published
		// itself: its metadata may be written but its socket is not accepting
		// yet, so listing it reports a live session as interrupted. The
		// daemon's catalog already skips these; this is the same rule.
		if _, err := os.Lstat(paths.Launching(dir)); err == nil {
			continue
		}
		meta, err := worker.ReadMeta(dir)
		if err != nil {
			continue // half-created or hand-deleted; not our problem to report
		}
		out = append(out, Session{Meta: meta, Dir: dir, Alive: alive(dir)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// ErrNoLocalSession reports that an ID names no session on this host. It is
// distinct from a failure to read the session directory: the caller falls back
// to the remote catalog on the former and must never swallow the latter.
var ErrNoLocalSession = errors.New("session not found on this host")

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
	return Session{}, fmt.Errorf("no session %s on this host: %w", id, ErrNoLocalSession)
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
	sessionsDir, err := paths.SessionsDir()
	if err != nil {
		return Session{}, err
	}

	cols, rows := 80, 24
	if w, h, err := term.GetSize(os.Stdout.Fd()); err == nil && w > 0 {
		cols, rows = w, h
	}
	launched, err := worker.LaunchDetached(worker.LaunchConfig{
		SessionsDir: sessionsDir,
		Command:     command,
		Cwd:         cwd,
		Env:         os.Environ(),
		Cols:        cols,
		Rows:        rows,
	})
	if err != nil {
		return Session{}, err
	}
	return Session{Meta: launched.Meta, Dir: launched.Dir, Alive: true}, nil
}

// RemoveLocal deletes what a finished local session left on disk. The caller
// establishes that it is finished; a live worker's directory holds its socket
// and metadata and removing it would orphan the process.
func RemoveLocal(s Session) error {
	if strings.TrimSpace(s.Dir) == "" {
		return fmt.Errorf("session %s has no directory", s.ID)
	}
	root, err := paths.SessionsDir()
	if err != nil {
		return err
	}
	// Only ever a session directory directly under the sessions root.
	if filepath.Dir(filepath.Clean(s.Dir)) != filepath.Clean(root) {
		return fmt.Errorf("session %s directory %s is outside %s", s.ID, s.Dir, root)
	}
	if err := os.RemoveAll(s.Dir); err != nil {
		return fmt.Errorf("remove session %s: %w", s.ID, err)
	}
	return nil
}
