package daemon

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/shaul/mesh/internal/storage"
)

const hostPersistenceInterval = time.Minute

// SessionDiff describes a committed catalog change. Added and Changed carry
// complete stored records; absent active workers change to interrupted.
type SessionDiff struct {
	Added   []storage.Session
	Changed []storage.Session
	Removed []storage.SessionID
}

func (c *Catalog) loadPrevious(ctx context.Context) error {
	if c.previous != nil {
		return nil
	}
	sessions, err := c.store.ListHostSessions(ctx, c.host.ID)
	if err != nil {
		return fmt.Errorf("daemon: load host %s sessions: %w", c.host.ID, err)
	}
	c.previous = make(map[storage.SessionID]storage.Session, len(sessions))
	for _, session := range sessions {
		c.previous[session.ID] = session
	}
	return nil
}

func (c *Catalog) sessionChanges(observed []storage.Session, retired []storage.SessionID) (map[storage.SessionID]storage.Session, storage.HostChanges, SessionDiff) {
	next := c.observedView(observed)
	changes := storage.HostChanges{Retired: retired}
	// Retiring a previously active record still passes through interrupted, so
	// the store's finished-only deletion guard remains effective.
	for _, id := range retired {
		if session, ok := next[id]; ok && session.State == storage.StateInterrupted && c.previous[id].State != storage.StateInterrupted {
			changes.Sessions = append(changes.Sessions, session)
		}
		delete(next, id)
	}
	diff := diffSessions(c.previous, next)
	changes.Sessions = append(changes.Sessions, diff.Added...)
	changes.Sessions = append(changes.Sessions, diff.Changed...)
	return next, changes, diff
}

func (c *Catalog) observedView(observed []storage.Session) map[storage.SessionID]storage.Session {
	next := maps.Clone(c.previous)
	seen := make(map[storage.SessionID]bool, len(observed))
	for _, session := range observed {
		previous := c.previous[session.ID]
		// Match SQLite's monotonic fields so stale metadata cannot write forever.
		if previous.LastAttachedAt != nil && (session.LastAttachedAt == nil || previous.LastAttachedAt.After(*session.LastAttachedAt)) {
			session.LastAttachedAt = previous.LastAttachedAt
		}
		session.LastOutputSequence = max(previous.LastOutputSequence, session.LastOutputSequence)
		session.CreatedAt = time.UnixMilli(session.CreatedAt.UnixMilli()).UTC()
		if session.LastAttachedAt != nil {
			attached := time.UnixMilli(session.LastAttachedAt.UnixMilli()).UTC()
			session.LastAttachedAt = &attached
		}
		next[session.ID] = session
		seen[session.ID] = true
	}
	for id, session := range c.previous {
		if seen[id] || (session.State != storage.StateRunning && session.State != storage.StateDetached) {
			continue
		}
		session.State, session.ExitCode = storage.StateInterrupted, nil
		next[id] = session
	}
	return next
}

func diffSessions(previous, next map[storage.SessionID]storage.Session) SessionDiff {
	var diff SessionDiff
	for _, id := range slices.Sorted(maps.Keys(next)) {
		current := next[id]
		before, exists := previous[id]
		if exists && sameStoredSession(before, current) {
			continue
		}
		if !exists {
			diff.Added = append(diff.Added, current)
			continue
		}
		diff.Changed = append(diff.Changed, current)
	}
	for _, id := range slices.Sorted(maps.Keys(previous)) {
		if _, exists := next[id]; !exists {
			diff.Removed = append(diff.Removed, id)
		}
	}
	return diff
}

func sameStoredSession(a, b storage.Session) bool {
	return a.ID == b.ID && a.HostID == b.HostID && slices.Equal(a.Command, b.Command) &&
		a.Cwd == b.Cwd && a.State == b.State && a.CreatedAt.UnixMilli() == b.CreatedAt.UnixMilli() &&
		sameStoredTime(a.LastAttachedAt, b.LastAttachedAt) && sameExitCode(a.ExitCode, b.ExitCode) &&
		a.LastOutputSequence == b.LastOutputSequence
}

func sameStoredTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.UnixMilli() == b.UnixMilli()
}

func sameExitCode(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func (c *Catalog) publishDiff(diff SessionDiff) {
	if c.onChange == nil || (len(diff.Added) == 0 && len(diff.Changed) == 0 && len(diff.Removed) == 0) {
		return
	}
	// A subscriber owns its records, including their pointer and slice fields.
	for i := range diff.Added {
		diff.Added[i] = cloneStoredSession(diff.Added[i])
	}
	for i := range diff.Changed {
		diff.Changed[i] = cloneStoredSession(diff.Changed[i])
	}
	c.onChange(diff)
}

func cloneStoredSession(session storage.Session) storage.Session {
	session.Command = slices.Clone(session.Command)
	session.LastAttachedAt = cloneLifecycleTime(session.LastAttachedAt)
	session.ExitCode = cloneLifecycleInt(session.ExitCode)
	return session
}

// FlushHost persists current liveness when the daemon is shutting down. The
// caller supplies a fresh context because the runtime context is cancelled.
func (c *Catalog) FlushHost(ctx context.Context) error {
	if err := validContext(ctx); err != nil {
		return fmt.Errorf("daemon: flush host: %w", err)
	}
	select {
	case c.reconcileGate <- struct{}{}:
		defer func() { <-c.reconcileGate }()
	case <-ctx.Done():
		return fmt.Errorf("daemon: flush host: %w", ctx.Err())
	}
	host := cloneHost(c.host)
	host.LastSeenAt = c.now().UTC()
	if host.LastSeenAt.Before(c.lastSeenAt) {
		host.LastSeenAt = c.lastSeenAt
	}
	if host.LastSeenAt.Equal(c.persistedAt) {
		return nil
	}
	if err := c.store.ApplyHostChanges(ctx, host.ID, storage.HostChanges{Host: &host}); err != nil {
		return fmt.Errorf("daemon: flush host %s: %w", host.ID, err)
	}
	c.persistedAt = host.LastSeenAt
	return nil
}
