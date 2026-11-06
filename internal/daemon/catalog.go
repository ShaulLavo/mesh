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

	"golang.org/x/sys/unix"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/worker"
)

// Catalog reconstructs the durable view of local workers from their session
// directories.
type Catalog struct {
	sessionsDir   string
	host          storage.Host
	store         CatalogStore
	probe         WorkerProbe
	bootID        func() string
	now           func() time.Time
	onReconcile   func([]storage.Session)
	onChange      func(SessionDiff)
	onObservation func(error)

	metadata        map[storage.SessionID]directoryRevision
	scannedMetadata map[storage.SessionID]directoryRevision
	// scannedExitedAt is each exited session's validated exit time, from the
	// metadata read that produced it in the latest scan. Retention ages
	// sessions by it, so one that ran for weeks is not retired the moment it
	// ends.
	scannedExitedAt map[storage.SessionID]time.Time
	previous        map[storage.SessionID]storage.Session
	lastSeenAt      time.Time
	persistedAt     time.Time
	reconcileGate   chan struct{}
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
		onReconcile:   cfg.OnReconcile,
		onChange:      cfg.OnChange,
		onObservation: cfg.OnObservation,
		reconcileGate: make(chan struct{}, 1),
	}, nil
}

// Reconcile replaces the stored active view with one complete observation of
// the worker directories.
func (c *Catalog) Reconcile(ctx context.Context) (resultErr error) {
	if err := validContext(ctx); err != nil {
		return fmt.Errorf("daemon: reconcile catalog: %w", err)
	}
	select {
	case c.reconcileGate <- struct{}{}:
		defer func() { <-c.reconcileGate }()
	case <-ctx.Done():
		return fmt.Errorf("daemon: reconcile catalog: %w", ctx.Err())
	}

	defer func() {
		if c.onObservation != nil {
			c.onObservation(resultErr)
		}
	}()
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
	if err := c.loadPrevious(ctx); err != nil {
		return err
	}
	observed, retired := c.retireFinishedSessions(observed)
	next, changes, diff := c.sessionChanges(observed, retired)
	c.lastSeenAt = host.LastSeenAt
	if c.persistedAt.IsZero() || host.LastSeenAt.Sub(c.persistedAt) >= hostPersistenceInterval {
		changes.Host = &host
	}
	if changes.Host != nil || len(changes.Sessions) > 0 || len(changes.Retired) > 0 {
		if err := c.store.ApplyHostChanges(ctx, c.host.ID, changes); err != nil {
			return fmt.Errorf("daemon: reconcile host %s sessions: %w", c.host.ID, err)
		}
	}
	c.previous = next
	if changes.Host != nil {
		c.persistedAt = host.LastSeenAt
	}
	diff.MetadataChanged = c.metadataChanges(next)
	c.metadata = c.scannedMetadata
	c.publishDiff(diff)
	if c.onReconcile != nil {
		c.onReconcile(observed)
	}
	for _, id := range retired {
		if err := removeUnclaimedRetiredSession(filepath.Join(c.sessionsDir, string(id))); err != nil {
			log.Printf("daemon: retire session %s directory: %v", id, err)
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

	c.scannedMetadata = make(map[storage.SessionID]directoryRevision)
	c.scannedExitedAt = make(map[storage.SessionID]time.Time)
	currentBootID := c.bootID()
	observed := make([]storage.Session, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("daemon: scan session directory %s: %w", c.sessionsDir, err)
		}

		if err := c.observeDirectory(entry); err != nil {
			return nil, err
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
				// Forgotten metadata may already be gone after an interrupted
				// retirement. Keep its intent in the scan until cleanup finishes.
				if _, markerErr := os.Lstat(paths.Forgotten(dir)); markerErr == nil {
					if forgotten, ok := quarantinedSession(c.host.ID, entry.Name(), c.now()); ok {
						observed = append(observed, forgotten)
					}
				}
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
				session, meta = refreshedSession, refreshed
			}
		}
		if session.State == storage.StateExited && meta.ExitedAt != nil {
			c.scannedExitedAt[session.ID] = *meta.ExitedAt
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
		ID:             storage.SessionID(meta.ID),
		HostID:         hostID,
		Command:        append([]string(nil), meta.Command...),
		Cwd:            meta.Cwd,
		CreatedAt:      meta.CreatedAt,
		LastAttachedAt: cloneLifecycleTime(meta.LastAttachedAt),
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
	// Legacy exited history is bounded; saved recovery data requires an
	// explicit forget so background reconciliation cannot erase prior work.
	sessionRetention = 7 * 24 * time.Hour
	// maxRetainedTerminalSessions bounds the same growth by count, for a host
	// that churns through sessions faster than the age cap retires them.
	maxRetainedTerminalSessions = 500
)

type retentionCandidate struct {
	index    int
	orderAt  time.Time
	exitedAt time.Time
	history  labelledExit
}

// retireFinishedSessions bounds ended history even when its service was
// disabled or deleted and will never call the post-start prune again.
func (c *Catalog) retireFinishedSessions(observed []storage.Session) ([]storage.Session, []storage.SessionID) {
	var finished []retentionCandidate
	retire := make(map[int]struct{})
	history := make(map[string][]labelledExit)
	indices := make(map[storage.SessionID]int)
	for i, current := range observed {
		if current.State != storage.StateExited && current.State != storage.StateInterrupted {
			continue
		}
		dir := filepath.Join(c.sessionsDir, string(current.ID))
		if ownerPinned(dir) {
			continue
		}
		if _, err := os.Lstat(paths.Forgotten(dir)); err == nil {
			retire[i] = struct{}{}
			continue
		}
		candidate, eligible := c.retentionCandidate(current, i)
		if !eligible {
			continue
		}
		finished = append(finished, candidate)
		if candidate.history.label == "" {
			continue
		}
		label := candidate.history.label
		history[label] = append(history[label], candidate.history)
		indices[current.ID] = i
	}
	for _, attempts := range history {
		sortLabelledHistory(attempts)
		for _, id := range prunedHistory(attempts) {
			retire[indices[id]] = struct{}{}
		}
	}
	markExpiredHistory(finished, c.now(), retire)
	return retainedSessions(observed, retire)
}

func (c *Catalog) retentionCandidate(current storage.Session, index int) (retentionCandidate, bool) {
	dir := filepath.Join(c.sessionsDir, string(current.ID))
	candidate := retentionCandidate{index: index, orderAt: current.CreatedAt}
	exit, labelled := c.eligibleLabelledExit(current.ID, "")
	if labelled {
		candidate.history, candidate.exitedAt = exit, exit.exitedAt
		if !exit.exitedAt.IsZero() {
			candidate.orderAt = exit.exitedAt
		}
		return candidate, true
	}
	if current.State != storage.StateExited || hasRecoveryHistory(dir) {
		return candidate, false
	}
	// Labelled records rejected by the shared classifier remain protected.
	meta, err := worker.ReadMeta(dir)
	if err == nil && meta.Label != "" {
		return candidate, false
	}
	candidate.exitedAt = c.scannedExitedAt[current.ID]
	if candidate.exitedAt.IsZero() {
		candidate.exitedAt = current.CreatedAt
	}
	candidate.orderAt = candidate.exitedAt
	return candidate, true
}

func markExpiredHistory(finished []retentionCandidate, now time.Time, retire map[int]struct{}) {
	remaining := finished[:0]
	for _, current := range finished {
		if !current.exitedAt.IsZero() && now.Sub(current.exitedAt) > sessionRetention {
			retire[current.index] = struct{}{}
			continue
		}
		if _, selected := retire[current.index]; selected {
			continue
		}
		remaining = append(remaining, current)
	}
	sort.Slice(remaining, func(i, j int) bool { return remaining[i].orderAt.Before(remaining[j].orderAt) })
	excess := max(0, len(remaining)-maxRetainedTerminalSessions)
	for _, current := range remaining[:excess] {
		retire[current.index] = struct{}{}
	}
}

func retainedSessions(observed []storage.Session, retire map[int]struct{}) ([]storage.Session, []storage.SessionID) {
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
		retired = append(retired, current.ID)
	}
	return kept, retired
}

func hasRecoveryHistory(dir string) bool {
	if _, err := os.Lstat(filepath.Join(dir, "recovery.lock")); !errors.Is(err, os.ErrNotExist) { //nolint:gosec // The caller supplies a catalog session directory; this adds a constant filename.
		return true
	}
	return hasSavedRecovery(dir)
}

func hasSavedRecovery(dir string) bool {
	// Presence protects damaged and unsupported records too. A failed lookup
	// cannot establish that a session has no recovery data worth retaining.
	for _, name := range []string{"recovery.json", "recovery-intent.json", "recovery-command.json"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) { //nolint:gosec // The caller supplies a catalog session directory; name comes from the constant list above.
			return true
		}
	}
	meta, err := worker.ReadMeta(dir)
	if err != nil {
		return !errors.Is(err, os.ErrNotExist)
	}
	return meta.RecoveredFrom != ""
}

func ownerPinned(dir string) bool {
	root, err := os.OpenRoot(dir)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		return true
	}
	defer func() { _ = root.Close() }()
	_, err = root.Lstat(".owner")
	return !errors.Is(err, os.ErrNotExist)
}

func removeUnclaimedRetiredSession(dir string) error {
	if ownerPinned(dir) {
		return errors.New("app worker owner still pins session directory")
	}
	if _, err := os.Lstat(paths.Forgotten(dir)); err == nil {
		return removeRetiredSession(dir)
	}
	// Recovery may have started after pruning selected this legacy session.
	claim, claimed, err := claimAutomaticRemoval(dir)
	if err != nil || !claimed {
		return err
	}
	if hasSavedRecovery(dir) {
		claim.release(true)
		return nil
	}
	err = removeRetiredSession(dir)
	claim.release(err != nil)
	return err
}

// automaticClaim is automatic cleanup's hold on a session's recovery lock,
// the same lock recovery takes, so the two never act on one session at once.
type automaticClaim struct {
	file *os.File
	path string
}

// claimAutomaticRemoval creates and locks a session's recovery lock. It
// claims nothing when the lock already exists: that belongs to recovery, or
// marks history that automatic cleanup must keep.
func claimAutomaticRemoval(dir string) (*automaticClaim, bool, error) {
	path := filepath.Join(dir, "recovery.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600) //nolint:gosec // fixed lock name in a catalog-observed session directory
	if errors.Is(err, os.ErrExist) || errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("claim automatic cleanup: %w", err)
	}
	err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		// Recovery opened the new file and locked it first; it is recovery's.
		_ = file.Close()
		return nil, false, nil
	}
	if err != nil {
		_ = os.Remove(path)
		_ = file.Close()
		return nil, false, fmt.Errorf("lock automatic cleanup: %w", err)
	}
	return &automaticClaim{file: file, path: path}, true, nil
}

// release ends the claim. A claim that removed nothing unlinks the lock file
// it created while still holding it, so a skipped or failed removal leaves no
// recovery marker that would protect the session forever.
func (c *automaticClaim) release(unlink bool) {
	if unlink {
		_ = os.Remove(c.path)
	}
	_ = unix.Flock(int(c.file.Fd()), unix.LOCK_UN)
	_ = c.file.Close()
}

func removeRetiredSession(dir string) error {
	if ownerPinned(dir) {
		return errors.New("app worker owner still pins session directory")
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	marker := paths.Forgotten(dir)
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if path == marker {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	// Remove the durable intent only after metadata is gone. A crash during
	// cleanup can then never turn a forgotten session back into a visible row.
	if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Remove(dir)
}

// Remove prevents reconciliation from restoring a retired session.
func (c *Catalog) Remove(ctx context.Context, id storage.SessionID) error {
	if err := validContext(ctx); err != nil {
		return fmt.Errorf("daemon: remove session %s: %w", id, err)
	}
	parsed, err := session.ParseID(string(id))
	if err != nil {
		return fmt.Errorf("daemon: remove session %s: %w", id, err)
	}
	if parsed != string(id) {
		return fmt.Errorf("daemon: remove session %s: ID is not canonical", id)
	}
	select {
	case c.reconcileGate <- struct{}{}:
		defer func() { <-c.reconcileGate }()
	case <-ctx.Done():
		return fmt.Errorf("daemon: remove session %s: %w", id, ctx.Err())
	}
	if ownerPinned(filepath.Join(c.sessionsDir, string(id))) {
		return fmt.Errorf("daemon: session %s remains reserved by its app", id)
	}
	removed, err := c.store.RetireSessions(ctx, c.host.ID, []storage.SessionID{id})
	if err != nil {
		return fmt.Errorf("daemon: retire session %s: %w", id, err)
	}
	if removed == 0 {
		return fmt.Errorf("daemon: session %s was not removed; it may have restarted", id)
	}
	delete(c.previous, id)
	c.publishDiff(SessionDiff{Removed: []storage.SessionID{id}})
	if err := removeRetiredSession(filepath.Join(c.sessionsDir, string(id))); err != nil {
		return fmt.Errorf("daemon: remove session directory %s: %w", id, err)
	}
	return nil
}
