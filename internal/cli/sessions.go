package cli

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/charmbracelet/x/term"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/worker"
)

// Liveness distinguishes a stopped worker from an inconclusive socket probe.
type Liveness uint8

const (
	LivenessUnknown Liveness = iota
	LivenessAlive
	LivenessGone
)

// Session combines worker metadata with the latest socket observation.
type Session struct {
	worker.Meta
	Dir      string
	Liveness Liveness
}

// State returns the state to display, reconciling what the worker last wrote
// with what is actually true now.
func (s Session) State() string {
	switch {
	case s.Liveness == LivenessAlive && s.Meta.State == worker.StateDetached:
		return worker.StateDetached
	case s.Liveness == LivenessAlive:
		return worker.StateRunning
	case s.Meta.State == worker.StateExited:
		return worker.StateExited
	case s.Liveness == LivenessUnknown:
		return s.Meta.State
	default:
		// A session that claimed to be running but has no socket did not exit
		// cleanly: its worker was killed or the machine rebooted underneath it.
		return worker.StateInterrupted
	}
}

// List returns every session this host knows about, most recently updated first.
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
	activity := make(map[string]time.Time, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if _, err := os.Lstat(paths.Forgotten(dir)); err == nil {
			continue
		}
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
		liveness := probeLiveness(dir, &meta)
		out = append(out, Session{Meta: meta, Dir: dir, Liveness: liveness})
		row := protocol.SessionInfo{CreatedAt: meta.CreatedAt, LastAttachedAt: meta.LastAttachedAt}
		if saved, err := recovery.Read(dir); err == nil && saved.SessionID == meta.ID {
			row.Recovery = &saved
		}
		activity[meta.ID] = row.LastActiveAt()
	}
	sort.SliceStable(out, func(i, j int) bool { return activity[out[i].ID].After(activity[out[j].ID]) })
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

// Latest returns the most recent session that may still accept attachment.
func Latest() (Session, error) {
	all, err := List()
	if err != nil {
		return Session{}, err
	}
	for _, s := range all {
		if s.Liveness != LivenessGone && liveState(s.State()) {
			return s, nil
		}
	}
	return Session{}, fmt.Errorf("no live sessions on this host")
}

var probeWorker = func(socket string) error {
	conn, err := net.DialTimeout("unix", socket, 500*time.Millisecond)
	if err != nil {
		return fmt.Errorf("probe worker socket %s: %w", socket, err)
	}
	_ = conn.Close()
	return nil
}

func probeLiveness(dir string, meta *worker.Meta) Liveness {
	bootID := worker.BootID()
	if meta.BootID != "" && bootID != "" && meta.BootID != bootID {
		return LivenessGone
	}
	err := probeWorker(paths.Socket(dir))
	if err == nil {
		return LivenessAlive
	}
	if !errors.Is(err, syscall.ENOENT) && !errors.Is(err, syscall.ECONNREFUSED) {
		return LivenessUnknown
	}
	// The worker writes its exit before removing the socket. A failed dial
	// can therefore make the metadata read before the probe stale.
	refreshed, err := worker.ReadMeta(dir)
	if err != nil {
		return LivenessUnknown
	}
	*meta = refreshed
	return LivenessGone
}

// Spawn starts a detached worker for command and returns the new session once
// its socket is accepting connections.
func Spawn(command []string, cwd string) (Session, error) {
	stateDir, err := paths.StateDir()
	if err != nil {
		return Session{}, err
	}
	host, _, err := identity.LoadOrCreate(stateDir)
	if err != nil {
		return Session{}, fmt.Errorf("load local host identity: %w", err)
	}
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
		HostID:      host.ID,
		Command:     command,
		Cwd:         cwd,
		Env:         os.Environ(),
		Term:        clientTerm(),
		Depth:       SessionDepth() + 1,
		Cols:        cols,
		Rows:        rows,
	})
	if err != nil {
		return Session{}, err
	}
	return Session{Meta: launched.Meta, Dir: launched.Dir, Liveness: LivenessAlive}, nil
}
