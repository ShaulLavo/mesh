package worker

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/agentresume"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
)

// hibernationTestWorker is detached since detachedAt with one registered
// Claude conversation, and records stops instead of signalling a fake PID.
func hibernationTestWorker(t *testing.T, detachedAt time.Time) (*Worker, net.Conn, string, *int) {
	t.Helper()
	now := detachedAt.Add(10 * time.Hour)
	w := agentTestWorkerAt(t, func() time.Time { return now })
	stops := 0
	w.stopForHibernation = func() { stops++ }
	if err := WriteMeta(w.cfg.Dir, Meta{ID: w.cfg.ID, PID: 4321, Command: []string{"/bin/bash"}, State: StateDetached,
		CreatedAt: detachedAt.Add(-time.Hour), DetachedAt: &detachedAt}); err != nil {
		t.Fatal(err)
	}
	conn, token := beginTestAgent(t, w, agentTestLaunch(agentresume.Claude), "", false)
	if response := agentTestEvent(t, w, token, "exact-conversation"); response.Type != protocol.TypeAgentRegistered {
		t.Fatalf("registration = %+v", response)
	}
	return w, conn, token, &stops
}

func hibernateRequest(t *testing.T, w *Worker, idle time.Duration) protocol.Control {
	t.Helper()
	return inspectRequest(t, w, protocol.Control{Type: protocol.TypeHibernate, RequestID: "h1",
		SessionID: w.cfg.ID, HibernateIdleMillis: idle.Milliseconds()})
}

func TestHibernateStopsTheAgentButKeepsItsConversationResumable(t *testing.T) {
	detachedAt := time.Date(2026, 9, 24, 8, 0, 0, 0, time.UTC)
	w, conn, token, stops := hibernationTestWorker(t, detachedAt)

	response := hibernateRequest(t, w, 0)
	if response.Type != protocol.TypeOK || response.RequestID != "h1" || response.SessionID != w.cfg.ID || *stops != 1 {
		t.Fatalf("hibernate = %+v after %d stops", response, *stops)
	}
	marker, err := recovery.ReadHibernation(w.cfg.Dir)
	if err != nil || marker.Reason != recovery.HibernateRequest || marker.Provider != agentresume.Claude || marker.ConversationID != "exact-conversation" {
		t.Fatalf("marker = %+v, %v", marker, err)
	}

	// The helper reports its provider's exit as the stop lands. That exit was
	// Mesh's doing, so it must not close the conversation.
	finish := agentLeaseRequest(t, conn, protocol.Control{Type: protocol.TypeAgentFinish, SessionID: w.cfg.ID, AgentToken: token})
	if finish.Type != protocol.TypeOK {
		t.Fatalf("finish = %+v", finish)
	}
	if saved := readTestAgent(t, w); saved.Agent == nil || saved.Agent.Lifecycle != agentresume.Active {
		t.Fatalf("recipe after hibernation = %+v", saved.Agent)
	}
	if again := hibernateRequest(t, w, 0); again.Type != protocol.TypeError || *stops != 1 {
		t.Fatalf("second hibernate = %+v after %d stops", again, *stops)
	}
}

func TestHibernateTurnsAwayAttachmentsWhileStopping(t *testing.T) {
	w, _, _, _ := hibernationTestWorker(t, time.Date(2026, 9, 24, 8, 0, 0, 0, time.UTC))
	w.mu.Lock()
	w.hibernating = true
	w.mu.Unlock()
	response := inspectRequest(t, w, protocol.Control{Type: protocol.TypeAttach, SessionID: w.cfg.ID})
	if response.Type != protocol.TypeError || !strings.Contains(response.Message, "hibernating") {
		t.Fatalf("attach during hibernation = %+v", response)
	}
}

func TestHibernateRefusesWithoutARunningRegisteredAgent(t *testing.T) {
	w := agentTestWorker(t)
	w.stopForHibernation = func() { t.Fatal("stopped a session without an agent") }
	response := hibernateRequest(t, w, 0)
	if response.Type != protocol.TypeError || response.Message != "worker: session 7K3D: "+ErrNotHibernatable.Error() {
		t.Fatalf("hibernate without agent = %+v", response)
	}
	if _, err := recovery.ReadHibernation(w.cfg.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refusal left a marker: %v", err)
	}

	conn, token := beginTestAgent(t, w, agentTestLaunch(agentresume.Codex), "", false)
	if response := agentTestEvent(t, w, token, "codex-conversation"); response.Type != protocol.TypeAgentRegistered {
		t.Fatalf("registration = %+v", response)
	}
	agentLeaseRequest(t, conn, protocol.Control{Type: protocol.TypeAgentFinish, SessionID: w.cfg.ID, AgentToken: token})
	if response := hibernateRequest(t, w, 0); response.Type != protocol.TypeError {
		t.Fatalf("hibernate after the user closed the agent = %+v", response)
	}
}

func TestHibernateRefusesAnAttachedSession(t *testing.T) {
	w, _, _, stops := hibernationTestWorker(t, time.Date(2026, 9, 24, 8, 0, 0, 0, time.UTC))
	w.mu.Lock()
	w.client = &attachment{}
	w.mu.Unlock()
	response := hibernateRequest(t, w, 0)
	w.mu.Lock()
	w.client = nil
	w.mu.Unlock()
	if response.Type != protocol.TypeError || !strings.Contains(response.Message, "attached") || *stops != 0 {
		t.Fatalf("hibernate attached = %+v after %d stops", response, *stops)
	}
}

func TestIdleHibernationRechecksDetachAndOutputAgainstLiveClocks(t *testing.T) {
	detachedAt := time.Date(2026, 9, 24, 8, 0, 0, 0, time.UTC)
	w, _, _, stops := hibernationTestWorker(t, detachedAt)
	now := w.now()

	if response := hibernateRequest(t, w, 11*time.Hour); response.Type != protocol.TypeError || !strings.Contains(response.Message, "detached long enough") {
		t.Fatalf("hibernate before the idle time = %+v", response)
	}
	w.mu.Lock()
	w.lastOutputAt = now.Add(-time.Minute)
	w.mu.Unlock()
	if response := hibernateRequest(t, w, 6*time.Hour); response.Type != protocol.TypeError || !strings.Contains(response.Message, "output") {
		t.Fatalf("hibernate right after output = %+v", response)
	}
	w.mu.Lock()
	w.lastOutputAt = now.Add(-7 * time.Hour)
	w.mu.Unlock()
	if response := hibernateRequest(t, w, 6*time.Hour); response.Type != protocol.TypeOK || *stops != 1 {
		t.Fatalf("idle hibernate = %+v after %d stops", response, *stops)
	}
	if marker, err := recovery.ReadHibernation(w.cfg.Dir); err != nil || marker.Reason != recovery.HibernateIdle {
		t.Fatalf("idle marker = %+v, %v", marker, err)
	}
}

func TestHibernateReleasesItsClaimWhenTheMarkerCannotBeSaved(t *testing.T) {
	w, _, _, stops := hibernationTestWorker(t, time.Date(2026, 9, 24, 8, 0, 0, 0, time.UTC))
	blocker := filepath.Join(w.cfg.Dir, recovery.HibernationFile+".tmp")
	if err := os.Mkdir(blocker, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocker, "keep"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if response := hibernateRequest(t, w, 0); response.Type != protocol.TypeError || *stops != 0 {
		t.Fatalf("hibernate with an unwritable marker = %+v after %d stops", response, *stops)
	}
	w.mu.Lock()
	hibernating := w.hibernating
	w.mu.Unlock()
	if hibernating {
		t.Fatal("a failed marker write left the session refusing attachments")
	}
}

func TestDetachRecordsWhenTheLastClientLeft(t *testing.T) {
	attachedAt := time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC)
	detachedAt := attachedAt.Add(72 * time.Hour)
	w := recoveryTestWorkerAt(t, func() time.Time { return detachedAt })
	if err := WriteMeta(w.cfg.Dir, Meta{ID: w.cfg.ID, PID: 4321, Command: []string{"/bin/bash"}, State: StateRunning, CreatedAt: attachedAt}); err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	w.lastAttachedAt = attachedAt
	w.mu.Unlock()
	w.recordAttachment()
	meta, err := ReadMeta(w.cfg.Dir)
	if err != nil || meta.State != StateDetached || meta.DetachedAt == nil || !meta.DetachedAt.Equal(detachedAt) {
		t.Fatalf("detached meta = %+v, %v", meta, err)
	}
	w.mu.Lock()
	w.client = &attachment{}
	w.mu.Unlock()
	w.recordAttachment()
	w.mu.Lock()
	w.client = nil
	w.mu.Unlock()
	if meta, err := ReadMeta(w.cfg.Dir); err != nil || meta.State != StateRunning || meta.DetachedAt != nil {
		t.Fatalf("reattached meta = %+v, %v", meta, err)
	}
}

func TestIdleHibernationRefusesASessionAttachedSinceItsRecordedDetach(t *testing.T) {
	detachedAt := time.Date(2026, 9, 24, 8, 0, 0, 0, time.UTC)
	w, _, _, stops := hibernationTestWorker(t, detachedAt)
	w.mu.Lock()
	w.lastAttachedAt = w.now().Add(-time.Minute)
	w.mu.Unlock()
	if response := hibernateRequest(t, w, 6*time.Hour); response.Type != protocol.TypeError || !strings.Contains(response.Message, "attached since") || *stops != 0 {
		t.Fatalf("hibernate after a fresh attachment = %+v after %d stops", response, *stops)
	}
}
