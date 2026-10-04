package cli

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
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
	stateDir, err := paths.StateDirPath()
	if err != nil {
		return nil, fmt.Errorf("locate sessions state directory: %w", err)
	}
	root := filepath.Join(stateDir, "s")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) && sessionDirectoryAbsent(root) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read sessions: %w", err)
	}

	rows := make([]sessionListingRow, len(entries))
	jobs := make(chan int)
	var probes sync.WaitGroup
	for range min(8, len(entries)) {
		probes.Go(func() {
			for i := range jobs {
				rows[i] = readSessionListingRow(filepath.Join(root, entries[i].Name()))
			}
		})
	}
	for i, entry := range entries {
		if entry.IsDir() {
			jobs <- i
		}
	}
	close(jobs)
	probes.Wait()
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].activeAt.After(rows[j].activeAt) })
	var out []Session
	for _, row := range rows {
		if row.current.Dir != "" {
			out = append(out, row.current)
		}
	}
	return out, nil
}

func sessionDirectoryAbsent(path string) bool {
	for {
		_, err := os.Lstat(path)
		if !errors.Is(err, os.ErrNotExist) {
			info, err := os.Stat(path)
			return err == nil && info.IsDir()
		}
		parent := filepath.Dir(path)
		if parent == path {
			return false
		}
		path = parent
	}
}

type sessionListingRow struct {
	current  Session
	activeAt time.Time
}

func readSessionListingRow(dir string) sessionListingRow {
	current, ok := readLocalSession(dir)
	if !ok {
		return sessionListingRow{}
	}
	activity := protocol.SessionInfo{CreatedAt: current.CreatedAt, LastAttachedAt: current.LastAttachedAt}
	if saved, err := recovery.Read(current.Dir); err == nil && saved.SessionID == current.ID {
		activity.Recovery = &saved
	}
	return sessionListingRow{current: current, activeAt: activity.LastActiveAt()}
}

func readLocalSession(dir string) (Session, bool) {
	if _, err := os.Lstat(paths.Forgotten(dir)); err == nil {
		return Session{}, false
	}
	// Unpublished workers may have metadata before their socket accepts clients.
	if _, err := os.Lstat(paths.Launching(dir)); err == nil {
		return Session{}, false
	}
	meta, err := worker.ReadMeta(dir)
	if err != nil {
		return Session{}, false
	}
	liveness := probeLiveness(dir, &meta)
	return Session{Meta: meta, Dir: dir, Liveness: liveness}, true
}

// ErrNoLocalSession reports that an ID names no session on this host. It is
// distinct from a failure to read the session directory: the caller falls back
// to the remote catalog on the former and must never swallow the latter.
var ErrNoLocalSession = errors.New("session not found on this host")

// Find returns the session with the given ID.
func Find(id string) (Session, error) {
	id = session.NormalizeID(id)
	name := filepath.Base(id)
	if name == "." || name == ".." || name != id {
		return Session{}, fmt.Errorf("no session %s on this host: %w", id, ErrNoLocalSession)
	}
	dir, err := paths.SessionDir(name)
	if err != nil {
		return Session{}, fmt.Errorf("locate session %s: %w", id, err)
	}
	entry, err := os.Lstat(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Session{}, fmt.Errorf("read session %s directory: %w", id, err)
	}
	if err == nil && entry.IsDir() {
		if current, ok := readLocalSession(dir); ok && current.ID == id {
			return current, nil
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
