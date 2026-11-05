package daemon

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"time"

	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/worker"
)

// labelledExit is one ended session of a route, as its worker recorded it.
type labelledExit struct {
	id        storage.SessionID
	createdAt time.Time
	ran       time.Duration
}

// PruneLabelled is a route's automatic history retention, run after each
// start. Of the other exited sessions carrying label it keeps the newest
// labelledHistoryLimit by creation, plus the newest one that ran at least
// labelledLongRun, and retires the rest. Live, interrupted, owner-pinned and
// recovery-protected sessions are never candidates. It is automatic cleanup,
// not `mesh rm`: every candidate is checked again under its recovery lock
// before its row is retired, and recovery that claims it first keeps it. It
// returns only the sessions whose removal completed.
func (c *Catalog) PruneLabelled(ctx context.Context, label string, current storage.SessionID) ([]storage.SessionID, error) {
	if err := validContext(ctx); err != nil {
		return nil, fmt.Errorf("daemon: prune %s history: %w", label, err)
	}
	select {
	case c.reconcileGate <- struct{}{}:
		defer func() { <-c.reconcileGate }()
	case <-ctx.Done():
		return nil, fmt.Errorf("daemon: prune %s history: %w", label, ctx.Err())
	}
	stored, err := c.store.ListHostSessions(ctx, c.host.ID)
	if err != nil {
		return nil, fmt.Errorf("daemon: prune %s history: list sessions: %w", label, err)
	}
	var removed []storage.SessionID
	for _, id := range prunedHistory(c.labelledHistory(stored, label, current)) {
		retired, err := c.retireLabelledExit(ctx, id, label)
		if err != nil {
			return removed, err
		}
		if retired {
			removed = append(removed, id)
		}
	}
	return removed, nil
}

// labelledHistory is the route's eligible ended sessions, newest first.
func (c *Catalog) labelledHistory(stored []storage.Session, label string, current storage.SessionID) []labelledExit {
	var history []labelledExit
	for _, session := range stored {
		if session.State != storage.StateExited || session.ID == current {
			continue
		}
		if exit, eligible := c.eligibleLabelledExit(session.ID, label); eligible {
			history = append(history, exit)
		}
	}
	sort.Slice(history, func(i, j int) bool {
		if !history[i].createdAt.Equal(history[j].createdAt) {
			return history[i].createdAt.After(history[j].createdAt)
		}
		return history[i].id > history[j].id
	})
	return history
}

// eligibleLabelledExit reads the worker's own record. Only an exited session
// with this label and a valid exit, no owner pin and no recovery history may
// be pruned automatically.
func (c *Catalog) eligibleLabelledExit(id storage.SessionID, label string) (labelledExit, bool) {
	dir := filepath.Join(c.sessionsDir, string(id))
	meta, err := worker.ReadMeta(dir)
	if err != nil || meta.ID != string(id) || meta.Label != label || meta.State != worker.StateExited {
		return labelledExit{}, false
	}
	if meta.ExitedAt == nil || meta.ExitedAt.Before(meta.CreatedAt) || ownerPinned(dir) || hasRecoveryHistory(dir) {
		return labelledExit{}, false
	}
	return labelledExit{id: id, createdAt: meta.CreatedAt, ran: meta.ExitedAt.Sub(meta.CreatedAt)}, true
}

// prunedHistory selects what history, newest first, does not keep.
func prunedHistory(history []labelledExit) []storage.SessionID {
	if len(history) <= labelledHistoryLimit {
		return nil
	}
	longRun := -1
	for index, exit := range history {
		if exit.ran >= labelledLongRun {
			longRun = index
			break
		}
	}
	pruned := make([]storage.SessionID, 0, len(history)-labelledHistoryLimit)
	for index := labelledHistoryLimit; index < len(history); index++ {
		if index != longRun {
			pruned = append(pruned, history[index].id)
		}
	}
	return pruned
}

// retireLabelledExit removes one selected session if it is still eligible.
// Recovery may claim it at any moment until this holds its lock, so the lock
// comes before the durable row is retired, and saved recovery is checked
// again under it.
func (c *Catalog) retireLabelledExit(ctx context.Context, id storage.SessionID, label string) (bool, error) {
	dir := filepath.Join(c.sessionsDir, string(id))
	if _, eligible := c.eligibleLabelledExit(id, label); !eligible {
		return false, nil
	}
	claim, claimed, err := claimAutomaticRemoval(dir)
	if err != nil {
		return false, fmt.Errorf("daemon: prune session %s: %w", id, err)
	}
	if !claimed {
		return false, nil
	}
	if hasSavedRecovery(dir) || ownerPinned(dir) {
		claim.release(true)
		return false, nil
	}
	retired, err := c.store.RetireSessions(ctx, c.host.ID, []storage.SessionID{id})
	if err != nil || retired == 0 {
		claim.release(true)
		if err != nil {
			return false, fmt.Errorf("daemon: prune session %s: retire its row: %w", id, err)
		}
		return false, nil
	}
	delete(c.previous, id)
	c.publishDiff(SessionDiff{Removed: []storage.SessionID{id}})
	if err := removeRetiredSession(dir); err != nil {
		// The row is gone but the directory is not. The next reconcile lists
		// it again from its metadata, and a later prune retries it.
		claim.release(true)
		log.Printf("daemon: prune session %s directory: %v", id, err)
		return false, nil
	}
	claim.release(false)
	return true, nil
}
