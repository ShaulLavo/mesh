package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/worker"
)

// Catalog reconstructs the durable view of local workers from their session
// directories.
type Catalog struct {
	sessionsDir string
	host        storage.Host
	store       CatalogStore
	probe       WorkerProbe
	bootID      func() string
	now         func() time.Time

	reconcileGate chan struct{}
}

// NewCatalog validates and retains the boundaries used by a local catalog.
func NewCatalog(cfg CatalogConfig) (*Catalog, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if err := validateCatalogConfig(cfg); err != nil {
		return nil, err
	}
	return &Catalog{
		sessionsDir:   cfg.SessionsDir,
		host:          cloneHost(cfg.Host),
		store:         cfg.Store,
		probe:         cfg.Probe,
		bootID:        cfg.BootID,
		now:           cfg.Now,
		reconcileGate: make(chan struct{}, 1),
	}, nil
}

// Reconcile replaces the stored active view with one complete observation of
// the worker directories.
func (c *Catalog) Reconcile(ctx context.Context) error {
	if err := validContext(ctx); err != nil {
		return fmt.Errorf("daemon: reconcile catalog: %w", err)
	}
	select {
	case c.reconcileGate <- struct{}{}:
		defer func() { <-c.reconcileGate }()
	case <-ctx.Done():
		return fmt.Errorf("daemon: reconcile catalog: %w", ctx.Err())
	}

	observed, err := c.scan(ctx)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("daemon: reconcile host %s: %w", c.host.ID, err)
	}
	host := cloneHost(c.host)
	host.LastSeenAt = c.now().UTC()
	if host.LastSeenAt.IsZero() || host.LastSeenAt.UnixMilli() < 0 {
		return fmt.Errorf("daemon: reconcile host %s: clock returned invalid observation time", host.ID)
	}
	observed, retired := c.retireFinishedSessions(observed)
	if err := c.store.ReconcileHost(ctx, host, observed); err != nil {
		return fmt.Errorf("daemon: reconcile host %s sessions: %w", c.host.ID, err)
	}
	if len(retired) > 0 {
		if _, err := c.store.RetireSessions(ctx, c.host.ID, retired); err != nil {
			// The directories are already gone, so the next reconcile will not
			// re-observe these. Report and continue rather than failing a scan
			// that otherwise succeeded.
			log.Printf("daemon: retire session rows for host %s: %v", c.host.ID, err)
		}
	}
	return nil
}

// List returns the durable sessions for the local host.
func (c *Catalog) List(ctx context.Context) ([]storage.Session, error) {
	if err := validContext(ctx); err != nil {
		return nil, fmt.Errorf("daemon: list host %s sessions: %w", c.host.ID, err)
	}
	sessions, err := c.store.ListHostSessions(ctx, c.host.ID)
	if err != nil {
		return nil, fmt.Errorf("daemon: list host %s sessions: %w", c.host.ID, err)
	}
	return sessions, nil
}

// Get returns one durable session for the local host.
func (c *Catalog) Get(ctx context.Context, id storage.SessionID) (storage.Session, error) {
	if err := validContext(ctx); err != nil {
		return storage.Session{}, fmt.Errorf("daemon: get session %s/%s: %w", c.host.ID, id, err)
	}
	if strings.TrimSpace(string(id)) == "" {
		return storage.Session{}, errors.New("daemon: get session: empty session ID")
	}
	parsed, err := session.ParseID(string(id))
	if err != nil {
		return storage.Session{}, fmt.Errorf("daemon: get session %s/%s: %w", c.host.ID, id, err)
	}
	if parsed != string(id) {
		return storage.Session{}, fmt.Errorf("daemon: get session %s/%s: session ID is not canonical", c.host.ID, id)
	}
	session, err := c.store.GetSession(ctx, c.host.ID, id)
	if err != nil {
		return storage.Session{}, fmt.Errorf("daemon: get session %s/%s: %w", c.host.ID, id, err)
	}
	return session, nil
}

func (c *Catalog) scan(ctx context.Context) ([]storage.Session, error) {
	entries, err := os.ReadDir(c.sessionsDir)
	if err != nil {
		return nil, fmt.Errorf("daemon: read session directory %s: %w", c.sessionsDir, err)
	}

	currentBootID := c.bootID()
	observed := make([]storage.Session, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("daemon: scan session directory %s: %w", c.sessionsDir, err)
		}

		dir := filepath.Join(c.sessionsDir, entry.Name())
		if _, err := os.Lstat(paths.Launching(dir)); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("daemon: inspect launching session %s: %w", entry.Name(), err)
		}
		meta, err := worker.ReadMeta(dir)
		if err != nil {
			// A directory with no metadata at all is a reservation whose marker
			// has not landed yet. It belongs to a create still in flight, so it
			// is not this scan's to report on.
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			// Anything else is a damaged record, most likely a torn write from
			// power loss. Quarantine it: one unreadable directory must not take
			// down a daemon that has healthy sessions to coordinate, and this
			// runs before the Unix socket exists, so failing here would leave no
			// way in to repair it. It is reported as interrupted rather than
			// dropped, because omitting it entirely would let ReconcileHost
			// declare live sessions dead.
			quarantined, ok := quarantinedSession(c.host.ID, entry.Name(), c.now())
			if !ok {
				log.Printf("daemon: ignoring session directory %s with unreadable metadata: %v", entry.Name(), err)
				continue
			}
			log.Printf("daemon: session %s has unreadable metadata, recording it as interrupted: %v", entry.Name(), err)
			observed = append(observed, quarantined)
			continue
		}
		// A record that decodes but contradicts itself is not a torn write; it
		// means something wrote a session directory Mesh does not understand.
		// That stays fatal, so the scan never mutates on a view it cannot trust.
		session, probe, err := sessionFromMeta(c.host.ID, entry.Name(), meta)
		if err != nil {
			return nil, err
		}

		if (session.State == storage.StateRunning || session.State == storage.StateDetached) && meta.BootID != "" && currentBootID != "" && meta.BootID != currentBootID {
			session.State = storage.StateInterrupted
			probe = false
		}
		if probe {
			probeErr := c.probe.Probe(ctx, paths.Socket(dir))
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("daemon: probe session %s: %w", meta.ID, err)
			}
			if probeErr != nil {
				if !workerDefinitelyUnavailable(probeErr) {
					return nil, fmt.Errorf("daemon: probe session %s inconclusive: %w", meta.ID, probeErr)
				}
				// A clean worker records exited metadata before removing its socket.
				// Reread after a definitive dial failure so stale running metadata
				// cannot turn a normal exit into an interruption.
				refreshed, err := worker.ReadMeta(dir)
				if err != nil {
					return nil, fmt.Errorf("daemon: reread unavailable session %s metadata: %w", meta.ID, err)
				}
				refreshedSession, stillRunning, err := sessionFromMeta(c.host.ID, entry.Name(), refreshed)
				if err != nil {
					return nil, err
				}
				if stillRunning && refreshed.BootID != "" && currentBootID != "" && refreshed.BootID != currentBootID {
					refreshedSession.State = storage.StateInterrupted
					stillRunning = false
				}
				if stillRunning {
					refreshedSession.State = storage.StateInterrupted
				}
				session = refreshedSession
			}
		}
		observed = append(observed, session)
	}

	sort.Slice(observed, func(i, j int) bool {
		return observed[i].ID < observed[j].ID
	})
	return observed, nil
}

func workerDefinitelyUnavailable(err error) bool {
	return errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)
}

func sessionFromMeta(hostID storage.HostID, directory string, meta worker.Meta) (storage.Session, bool, error) {
	if meta.ID != directory {
		return storage.Session{}, false, fmt.Errorf("daemon: session directory %s contains metadata for %q", directory, meta.ID)
	}
	if strings.TrimSpace(meta.ID) == "" {
		return storage.Session{}, false, fmt.Errorf("daemon: session directory %q has an empty ID", directory)
	}
	parsed, err := session.ParseID(meta.ID)
	if err != nil {
		return storage.Session{}, false, fmt.Errorf("daemon: invalid session %s ID: %w", directory, err)
	}
	if parsed != meta.ID {
		return storage.Session{}, false, fmt.Errorf("daemon: invalid session %s ID %q is not canonical", directory, meta.ID)
	}
	if meta.PID <= 0 {
		return storage.Session{}, false, fmt.Errorf("daemon: session %s has invalid PID %d", meta.ID, meta.PID)
	}
	if len(meta.Command) == 0 || meta.Command[0] == "" {
		return storage.Session{}, false, fmt.Errorf("daemon: session %s has no command", meta.ID)
	}
	if meta.CreatedAt.IsZero() || meta.CreatedAt.UnixMilli() < 0 {
		return storage.Session{}, false, fmt.Errorf("daemon: session %s has invalid creation time", meta.ID)
	}

	session := storage.Session{
		ID:        storage.SessionID(meta.ID),
		HostID:    hostID,
		Command:   append([]string(nil), meta.Command...),
		Cwd:       meta.Cwd,
		CreatedAt: meta.CreatedAt,
	}
	switch meta.State {
	case worker.StateRunning, worker.StateDetached:
		if meta.ExitedAt != nil || meta.ExitCode != nil {
			return storage.Session{}, false, fmt.Errorf("daemon: live session %s has exit fields", meta.ID)
		}
		// Both are alive and both are probed; they differ only in whether a
		// client is currently watching.
		session.State = storage.StateRunning
		if meta.State == worker.StateDetached {
			session.State = storage.StateDetached
		}
		return session, true, nil
	case worker.StateExited:
		if meta.ExitedAt == nil || meta.ExitCode == nil {
			return storage.Session{}, false, fmt.Errorf("daemon: exited session %s has incomplete exit fields", meta.ID)
		}
		if meta.ExitedAt.IsZero() || meta.ExitedAt.UnixMilli() < 0 || meta.ExitedAt.Before(meta.CreatedAt) {
			return storage.Session{}, false, fmt.Errorf("daemon: exited session %s has invalid exit time", meta.ID)
		}
		exitCode := *meta.ExitCode
		session.State = storage.StateExited
		session.ExitCode = &exitCode
		return session, false, nil
	default:
		return storage.Session{}, false, fmt.Errorf("daemon: session %s has unsupported worker state %q", meta.ID, meta.State)
	}
}

func validateCatalogConfig(cfg CatalogConfig) error {
	if strings.TrimSpace(cfg.SessionsDir) == "" {
		return errors.New("daemon: catalog has an empty sessions directory")
	}
	if strings.TrimSpace(string(cfg.Host.ID)) == "" {
		return errors.New("daemon: catalog host has an empty ID")
	}
	if strings.TrimSpace(cfg.Host.MeshIdentity) == "" {
		return fmt.Errorf("daemon: catalog host %s has an empty Mesh identity", cfg.Host.ID)
	}
	if cfg.Host.Alias != nil && *cfg.Host.Alias == "" {
		return fmt.Errorf("daemon: catalog host %s has an empty alias", cfg.Host.ID)
	}
	if cfg.Host.TailscaleName != nil && *cfg.Host.TailscaleName == "" {
		return fmt.Errorf("daemon: catalog host %s has an empty Tailscale name", cfg.Host.ID)
	}
	if cfg.Host.LastSeenAt.IsZero() || cfg.Host.LastSeenAt.UnixMilli() < 0 {
		return fmt.Errorf("daemon: catalog host %s has an invalid last seen time", cfg.Host.ID)
	}
	if cfg.Store == nil {
		return errors.New("daemon: catalog has no store")
	}
	if cfg.Probe == nil {
		return errors.New("daemon: catalog has no worker probe")
	}
	if cfg.BootID == nil {
		return errors.New("daemon: catalog has no boot ID reader")
	}
	return nil
}

func validContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("nil context")
	}
	return ctx.Err()
}

func cloneHost(host storage.Host) storage.Host {
	host.Alias = cloneCatalogString(host.Alias)
	host.TailscaleName = cloneCatalogString(host.TailscaleName)
	return host
}

func cloneCatalogString(value *string) *string {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

// quarantinedSession is the record kept for a session directory whose metadata
// cannot be trusted. The session is real — the directory exists and something
// created it — but its command, PID and creation time are unknowable, so it is
// reported as interrupted, which is exactly what "we cannot account for this
// process any more" means everywhere else in Mesh.
//
// It returns false when the directory name is not a session ID at all, in which
// case there is nothing to record and the caller skips it.
func quarantinedSession(hostID storage.HostID, directory string, now time.Time) (storage.Session, bool) {
	parsed, err := session.ParseID(directory)
	if err != nil || parsed != directory {
		return storage.Session{}, false
	}
	return storage.Session{
		ID:        storage.SessionID(directory),
		HostID:    hostID,
		Command:   []string{"unknown"},
		State:     storage.StateInterrupted,
		CreatedAt: now,
	}, true
}

const (
	// sessionRetention is how long a finished session's record and its output
	// stay on disk. Nothing pruned these before, so an always-on host grew
	// without bound: the 1 Hz reconciler re-read and re-upserted every row it
	// had ever seen, and past roughly fifteen thousand a reconcile no longer
	// fit in its tick, holding the write lock continuously.
	sessionRetention = 7 * 24 * time.Hour
	// maxRetainedTerminalSessions bounds the same growth by count, for a host
	// that churns through sessions faster than the age cap retires them.
	maxRetainedTerminalSessions = 500
)

// retireFinishedSessions removes finished sessions past the retention limits and
// returns the set to keep. Running and interrupted-but-recent sessions are never
// touched: this only retires records whose process is already gone.
//
// It prunes the directory as well as the row, because worker.log is the bulk of
// what a finished session occupies.
func (c *Catalog) retireFinishedSessions(observed []storage.Session) ([]storage.Session, []storage.SessionID) {
	now := c.now()
	type candidate struct {
		index    int
		retireAt time.Time
	}
	var finished []candidate
	for i, current := range observed {
		// Detached is alive. Retiring it would delete a session someone walked
		// away from and fully intends to come back to.
		if current.State == storage.StateRunning || current.State == storage.StateDetached {
			continue
		}
		finished = append(finished, candidate{index: i, retireAt: current.CreatedAt})
	}
	// Oldest first, so a count cap retires the least useful history.
	sort.Slice(finished, func(i, j int) bool { return finished[i].retireAt.Before(finished[j].retireAt) })

	retire := make(map[int]struct{})
	excess := len(finished) - maxRetainedTerminalSessions
	for rank, current := range finished {
		agedOut := !current.retireAt.IsZero() && now.Sub(current.retireAt) > sessionRetention
		overCap := rank < excess
		if agedOut || overCap {
			retire[current.index] = struct{}{}
		}
	}
	if len(retire) == 0 {
		return observed, nil
	}

	kept := make([]storage.Session, 0, len(observed)-len(retire))
	retired := make([]storage.SessionID, 0, len(retire))
	for i, current := range observed {
		if _, dropping := retire[i]; !dropping {
			kept = append(kept, current)
			continue
		}
		dir := filepath.Join(c.sessionsDir, string(current.ID))
		if err := os.RemoveAll(dir); err != nil {
			// Keep the record if its directory survives, so the next pass
			// retries rather than orphaning the files.
			log.Printf("daemon: retire session %s: %v", current.ID, err)
			kept = append(kept, current)
			continue
		}
		retired = append(retired, current.ID)
	}
	return kept, retired
}

// Retire deletes finished sessions for this host. The store refuses a running
// one in SQL, so a mistaken ID cannot remove live work.
func (c *Catalog) Retire(ctx context.Context, ids []storage.SessionID) (int64, error) {
	if err := validContext(ctx); err != nil {
		return 0, fmt.Errorf("daemon: retire sessions for %s: %w", c.host.ID, err)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	return c.store.RetireSessions(ctx, c.host.ID, ids)
}
