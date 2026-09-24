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
	claimed, err := w.hibernate(request)
	if claimed {
		// Held until the reply is written, like killAndAcknowledge: finish
		// waits on it, so the process cannot exit with the answer unsent.
		defer w.killResponders.Done()
	}
	if err != nil {
		response.Type, response.Message = protocol.TypeError, err.Error()
	}
	_ = conn.SetWriteDeadline(time.Now().Add(attachmentWriteTimeout))
	_ = protocol.NewWriter(conn).WriteControlMsg(response)
}

// hibernate ends the session like kill, except that the agent's recipe stays
// active. Recovery's default action then resumes the same conversation, so
// every client that already offers "Resume" on an ended session wakes it.
// A true claimed result means the caller owns one killResponders slot.
func (w *Worker) hibernate(request protocol.Control) (bool, error) {
	idle := time.Duration(request.HibernateIdleMillis) * time.Millisecond
	if idle < 0 {
		return false, fmt.Errorf("worker: session %s: negative hibernation idle time", w.cfg.ID)
	}
	marker, err := w.claimHibernation(idle)
	if err != nil {
		return false, fmt.Errorf("worker: session %s: %w", w.cfg.ID, err)
	}
	if err := recovery.WriteHibernation(w.cfg.Dir, marker); err != nil {
		w.mu.Lock()
		w.hibernating = false
		w.mu.Unlock()
		return true, fmt.Errorf("worker: session %s: record hibernation: %w", w.cfg.ID, err)
	}
	if w.stopForHibernation != nil {
		w.stopForHibernation()
		return true, nil
	}
	w.kill()
	return true, nil
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
		// The detach time was read before these locks; an attachment since
		// then means someone just looked at this session.
		if w.lastAttachedAt.After(detachedAt) {
			return recovery.Hibernation{}, errors.New("session was attached since it was last detached")
		}
		if !w.lastOutputAt.IsZero() && now.Sub(w.lastOutputAt) < idle {
			return recovery.Hibernation{}, errors.New("session produced output too recently")
		}
	}
	w.hibernating = true
	// Registered under w.mu after the finished check, as killAndAcknowledge
	// does, so finish can never be waiting when the count rises.
	w.killResponders.Add(1)
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
