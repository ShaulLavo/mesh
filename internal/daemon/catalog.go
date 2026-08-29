package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
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

	reconcileGate chan struct{}
}

// NewCatalog validates and retains the boundaries used by a local catalog.
func NewCatalog(cfg CatalogConfig) (*Catalog, error) {
	if err := validateCatalogConfig(cfg); err != nil {
		return nil, err
	}
	return &Catalog{
		sessionsDir:   cfg.SessionsDir,
		host:          cloneHost(cfg.Host),
		store:         cfg.Store,
		probe:         cfg.Probe,
		bootID:        cfg.BootID,
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
	if _, err := c.store.UpsertHost(ctx, c.host); err != nil {
		return fmt.Errorf("daemon: reconcile host %s: %w", c.host.ID, err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("daemon: reconcile host %s: %w", c.host.ID, err)
	}
	if err := c.store.ReconcileHost(ctx, c.host.ID, observed); err != nil {
		return fmt.Errorf("daemon: reconcile host %s sessions: %w", c.host.ID, err)
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
	if _, err := protocol.NewSessionID(string(id)); err != nil {
		return storage.Session{}, fmt.Errorf("daemon: get session %s/%s: %w", c.host.ID, id, err)
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
			return nil, fmt.Errorf("daemon: read session %s metadata: %w", entry.Name(), err)
		}
		session, probe, err := sessionFromMeta(c.host.ID, entry.Name(), meta)
		if err != nil {
			return nil, err
		}

		if session.State == storage.StateRunning && meta.BootID != "" && currentBootID != "" && meta.BootID != currentBootID {
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
	if _, err := protocol.NewSessionID(meta.ID); err != nil {
		return storage.Session{}, false, fmt.Errorf("daemon: invalid session %s ID: %w", directory, err)
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
	case worker.StateRunning:
		if meta.ExitedAt != nil || meta.ExitCode != nil {
			return storage.Session{}, false, fmt.Errorf("daemon: running session %s has exit fields", meta.ID)
		}
		session.State = storage.StateRunning
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
