package worker

import (
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/shaul/mesh/internal/agentresume"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
)

// ErrNotHibernatable reports a session with no running, registered agent. Only
// such a session can stop and later reopen exactly where it was.
var ErrNotHibernatable = errors.New("no running agent conversation is registered in this session")

func (w *Worker) hibernateAndAcknowledge(conn net.Conn, request protocol.Control) {
	response := protocol.Control{Type: protocol.TypeOK, RequestID: request.RequestID, SessionID: w.cfg.ID}
	if err := w.hibernate(request); err != nil {
		response.Type, response.Message = protocol.TypeError, err.Error()
	}
	_ = conn.SetWriteDeadline(time.Now().Add(attachmentWriteTimeout))
	_ = protocol.NewWriter(conn).WriteControlMsg(response)
}

// hibernate ends the session like kill, except that the agent's recipe stays
// active. Recovery's default action then resumes the same conversation, so
// every client that already offers "Resume" on an ended session wakes it.
func (w *Worker) hibernate(request protocol.Control) error {
	idle := time.Duration(request.HibernateIdleMillis) * time.Millisecond
	if idle < 0 {
		return fmt.Errorf("worker: session %s: negative hibernation idle time", w.cfg.ID)
	}
	marker, err := w.claimHibernation(idle)
	if err != nil {
		return fmt.Errorf("worker: session %s: %w", w.cfg.ID, err)
	}
	if err := recovery.WriteHibernation(w.cfg.Dir, marker); err != nil {
		w.mu.Lock()
		w.hibernating = false
		w.mu.Unlock()
		return fmt.Errorf("worker: session %s: record hibernation: %w", w.cfg.ID, err)
	}
	w.killResponders.Add(1)
	defer w.killResponders.Done()
	w.kill()
	return nil
}

// claimHibernation re-checks eligibility under the worker's own locks: the
// daemon's view comes from disk and can be a checkpoint behind.
func (w *Worker) claimHibernation(idle time.Duration) (recovery.Hibernation, error) {
	detachedAt := w.detachedSince()
	w.agentMu.Lock()
	defer w.agentMu.Unlock()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.finished || w.reaped {
		return recovery.Hibernation{}, errors.New("session has already ended")
	}
	if w.hibernating {
		return recovery.Hibernation{}, errors.New("session is already hibernating")
	}
	if w.client != nil {
		return recovery.Hibernation{}, errors.New("session is attached; detach it first")
	}
	invocation, recipe := w.agentInvocation, w.recoveryState.Agent
	if invocation == nil || !invocation.registered || recipe == nil ||
		recipe.InvocationToken != invocation.token || recipe.Lifecycle != agentresume.Active {
		return recovery.Hibernation{}, ErrNotHibernatable
	}
	reason := recovery.HibernateRequest
	if idle > 0 {
		reason = recovery.HibernateIdle
		now := w.currentTime()
		if detachedAt.IsZero() || now.Sub(detachedAt) < idle {
			return recovery.Hibernation{}, errors.New("session has not been detached long enough")
		}
		if !w.lastOutputAt.IsZero() && now.Sub(w.lastOutputAt) < idle {
			return recovery.Hibernation{}, errors.New("session produced output too recently")
		}
	}
	w.hibernating = true
	return recovery.Hibernation{Version: 1, At: w.currentTime().UTC().Round(0), Reason: reason,
		Provider: recipe.Provider, ConversationID: recipe.ConversationID}, nil
}

func (w *Worker) detachedSince() time.Time {
	w.metaMu.Lock()
	defer w.metaMu.Unlock()
	meta, err := ReadMeta(w.cfg.Dir)
	if err != nil || meta.State != StateDetached || meta.DetachedAt == nil {
		return time.Time{}
	}
	return *meta.DetachedAt
}
