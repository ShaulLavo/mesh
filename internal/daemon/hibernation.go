package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/agentresume"
	"github.com/shaul/mesh/internal/procmem"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/worker"
)

const (
	// A stop waits out the worker's hangup grace before it escalates.
	hibernationTimeout = 20 * time.Second
	// Pickers list every second or so; memory is not worth a process scan
	// that often, and a few seconds of staleness is invisible in a size.
	memorySampleTTL = 10 * time.Second
)

// hibernationInterval scans a few times per idle period, so a session stops
// within about a quarter of the configured time after it qualifies.
func hibernationInterval(idle time.Duration) time.Duration {
	return min(max(idle/4, time.Second), time.Minute)
}

// hibernator stops registered agents whose sessions have sat detached and
// quiet for the configured time, and remembers each refusal so a session the
// worker keeps declining is logged once rather than every scan.
type hibernator struct {
	lifecycle *lifecycle
	idle      time.Duration
	now       func() time.Time

	mu      sync.Mutex
	refused map[storage.SessionID]string
}

func (h *hibernator) run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.scan(ctx)
		}
	}
}

func (h *hibernator) scan(ctx context.Context) int {
	sessions, err := h.lifecycle.catalog.List(ctx)
	if err != nil {
		log.Printf("daemon: hibernation scan: %v", err)
		return 0
	}
	now := h.now()
	stopped := 0
	for _, stored := range sessions {
		if stored.State != storage.StateDetached {
			continue
		}
		dir := filepath.Join(h.lifecycle.sessionsDir, string(stored.ID))
		if !hibernationCandidate(dir, string(h.lifecycle.host.ID), h.idle, now) {
			continue
		}
		if h.hibernate(ctx, stored.ID) {
			stopped++
		}
	}
	if stopped > 0 {
		if err := h.lifecycle.catalog.Reconcile(ctx); err != nil && ctx.Err() == nil {
			log.Printf("daemon: publish hibernated sessions: %v", err)
		}
	}
	return stopped
}

func (h *hibernator) hibernate(ctx context.Context, id storage.SessionID) bool {
	operation, cancel := context.WithTimeout(ctx, hibernationTimeout)
	defer cancel()
	_, err := h.lifecycle.forwardOneShot(operation, protocol.Control{
		Type: protocol.TypeHibernate, RequestID: "hibernate-" + string(id),
		SessionID: string(id), HibernateIdleMillis: h.idle.Milliseconds(),
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	if err == nil {
		delete(h.refused, id)
		log.Printf("daemon: hibernated session %s after %s detached and quiet", id, h.idle)
		return true
	}
	if h.refused[id] != err.Error() {
		h.refused[id] = err.Error()
		log.Printf("daemon: session %s stays running: %v", id, err)
	}
	return false
}

// hibernationCandidate reads only what the worker already persisted. The
// worker re-checks every condition against its live state before stopping.
func hibernationCandidate(dir, hostID string, idle time.Duration, now time.Time) bool {
	meta, err := worker.ReadMeta(dir)
	if err != nil || meta.State != worker.StateDetached || meta.DetachedAt == nil || now.Sub(*meta.DetachedAt) < idle {
		return false
	}
	record, err := recovery.Read(dir)
	if err != nil || record.HostID != hostID || record.SessionID != meta.ID {
		return false
	}
	if record.Agent == nil || record.Agent.Lifecycle != agentresume.Active {
		return false
	}
	return record.LastOutputAt.IsZero() || now.Sub(record.LastOutputAt) >= idle
}

// memorySampler caches per-session process-tree sizes between list calls.
type memorySampler struct {
	mu      sync.Mutex
	sampled time.Time
	sizes   map[string]uint64
}

func (m *memorySampler) sizesFor(sessionsDir string, sessions []storage.Session, now time.Time) map[string]uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sizes != nil && now.Sub(m.sampled) < memorySampleTTL {
		return m.sizes
	}
	table := procmem.Snapshot()
	sizes := make(map[string]uint64, len(sessions))
	for _, stored := range sessions {
		if stored.State != storage.StateRunning && stored.State != storage.StateDetached {
			continue
		}
		meta, err := worker.ReadMeta(filepath.Join(sessionsDir, string(stored.ID)))
		if err != nil || meta.PID <= 0 {
			continue
		}
		sizes[meta.ID] = worker.SessionMemory(table, meta.ID, meta.PID)
	}
	m.sizes, m.sampled = sizes, now
	return sizes
}

// addHibernationInfo reports detach time for live sessions and the marker
// for an exited session that has not been woken into a replacement yet.
func addHibernationInfo(dir string, meta worker.Meta, metaErr error, info *protocol.SessionInfo) {
	if metaErr == nil && info.State == string(storage.StateDetached) && meta.DetachedAt != nil {
		detachedAt := *meta.DetachedAt
		info.DetachedAt = &detachedAt
	}
	if info.State != string(storage.StateExited) || info.ReplacementID != "" {
		return
	}
	hibernation, err := recovery.ReadHibernation(dir)
	if err == nil {
		info.Hibernated = &hibernation
		return
	}
	if !errors.Is(err, os.ErrNotExist) {
		info.RecoveryError = fmt.Sprintf("hibernation record: %v", err)
	}
}
