package worker

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"path/filepath"
	"reflect"
	"slices"
	"time"

	"github.com/shaul/mesh/internal/agentresume"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
)

type agentInvocation struct {
	token      string
	launch     agentresume.Launch
	expectedID string
	explicit   bool
	lookupOnly bool
	resuming   bool
	registered bool
	conn       net.Conn
}

func (w *Worker) loadExpectedAgent() {
	if w.cfg.RecoveredFrom == "" {
		return
	}
	plan, err := recovery.PendingAgent(filepath.Dir(w.cfg.Dir), w.cfg.HostID, w.cfg.RecoveredFrom, w.cfg.ID)
	if err != nil {
		log.Printf("worker: read agent recovery plan for %s: %v", w.cfg.ID, err)
		return
	}
	w.agentExpected = plan
}

func (w *Worker) serveAgent(conn net.Conn, request protocol.Control) {
	invocation, err := w.beginAgent(conn, request)
	if err != nil {
		w.writeRecoveryResponse(conn, request, protocol.TypeAgentBegun, err)
		return
	}
	defer w.agentReaders.Done()
	defer w.retireAgent(invocation)
	response := protocol.Control{Type: protocol.TypeAgentBegun, RequestID: request.RequestID,
		SessionID: w.cfg.ID, AgentToken: invocation.token, AgentHostID: w.cfg.HostID}
	_ = conn.SetWriteDeadline(time.Now().Add(attachmentWriteTimeout))
	if err := protocol.NewWriter(conn).WriteControlMsg(response); err != nil {
		return
	}
	_ = conn.SetWriteDeadline(time.Time{})
	frame, err := protocol.NewReader(conn).ReadFrame()
	if err != nil || frame.Kind != protocol.KindControl {
		return
	}
	finish, err := protocol.DecodeControl(frame.Payload)
	if err != nil || finish.Type != protocol.TypeAgentFinish {
		return
	}
	verified, err := w.finishAgent(invocation, finish)
	if err != nil {
		w.writeRecoveryResponse(conn, finish, protocol.TypeOK, err)
		return
	}
	_ = conn.SetWriteDeadline(time.Now().Add(attachmentWriteTimeout))
	_ = protocol.NewWriter(conn).WriteControlMsg(protocol.Control{Type: protocol.TypeOK,
		RequestID: finish.RequestID, SessionID: w.cfg.ID, AgentVerified: verified})
}

func (w *Worker) beginAgent(conn net.Conn, request protocol.Control) (*agentInvocation, error) {
	if err := w.validateAgentBegin(conn, request); err != nil {
		return nil, err
	}
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, fmt.Errorf("worker: create agent invocation: %w", err)
	}
	invocation := &agentInvocation{token: hex.EncodeToString(token[:]), launch: *request.AgentLaunch,
		expectedID: request.AgentExpectedID, explicit: request.AgentExplicit, lookupOnly: request.AgentLookupOnly, conn: conn}
	invocation.launch.Options = slices.Clone(invocation.launch.Options)
	w.agentMu.Lock()
	defer w.agentMu.Unlock()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.finished || w.reaped || w.checkpointWriter == nil {
		return nil, fmt.Errorf("worker: agent recovery is unavailable")
	}
	invocation.resuming = w.agentExpected != nil && !invocation.lookupOnly &&
		invocation.expectedID == w.agentExpected.ConversationID && reflect.DeepEqual(invocation.launch, w.agentExpected.Launch)
	previous := w.agentInvocation
	w.agentInvocation = invocation
	w.agentReaders.Add(1)
	if previous != nil {
		_ = previous.conn.Close()
	}
	return invocation, nil
}

func (w *Worker) validateAgentBegin(conn net.Conn, request protocol.Control) error {
	if err := w.validateAgentTarget(request); err != nil {
		return err
	}
	if request.AgentLaunch == nil {
		return fmt.Errorf("worker: missing agent launch")
	}
	if err := agentresume.ValidateLaunch(*request.AgentLaunch); err != nil {
		return err
	}
	if request.AgentExplicit && request.AgentExpectedID == "" {
		return fmt.Errorf("worker: explicit binding requires an expected conversation")
	}
	if request.AgentLookupOnly && !request.AgentExplicit {
		return fmt.Errorf("worker: native lookup requires explicit binding")
	}
	if request.AgentExpectedID != "" {
		candidate := agentresume.Recipe{Version: 1, Launch: *request.AgentLaunch, ConversationID: request.AgentExpectedID,
			InvocationToken: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", RegisteredAt: time.Now(), Lifecycle: "explicit"}
		if err := agentresume.ValidateRecipe(candidate); err != nil {
			return err
		}
	}
	validate := w.agentCaller
	if validate == nil {
		validate = w.validateAgentCaller
	}
	return validate(conn, request.AgentPID)
}

func (w *Worker) validateAgentTarget(request protocol.Control) error {
	if request.SessionID != w.cfg.ID || request.AgentHostID != w.cfg.HostID || w.cfg.HostID == "" {
		return fmt.Errorf("worker: agent registration requires this exact host and session")
	}
	return nil
}

func (w *Worker) writeAgentEvent(conn net.Conn, request protocol.Control) {
	err := w.registerAgentEvent(request)
	w.writeRecoveryResponse(conn, request, protocol.TypeAgentRegistered, err)
}

func (w *Worker) registerAgentEvent(request protocol.Control) error {
	if err := w.validateAgentTarget(request); err != nil {
		return err
	}
	if request.AgentEvent == nil {
		return fmt.Errorf("worker: missing agent lifecycle event")
	}
	if err := agentresume.ValidateEvent(*request.AgentEvent); err != nil {
		return err
	}
	w.agentMu.Lock()
	defer w.agentMu.Unlock()
	invocation := w.agentInvocation
	if invocation == nil || invocation.token != request.AgentToken {
		return fmt.Errorf("worker: agent invocation is no longer current")
	}
	if request.AgentProvider != invocation.launch.Provider {
		return fmt.Errorf("worker: hook provider does not match the current invocation")
	}
	event := *request.AgentEvent
	if event.Subagent || event.Kind == "end" {
		return nil
	}
	if !invocation.registered && invocation.expectedID != "" && event.ConversationID != invocation.expectedID {
		return fmt.Errorf("worker: provider did not acknowledge the expected conversation")
	}
	if err := w.saveAgentStart(invocation, event); err != nil {
		return err
	}
	invocation.registered = true
	return nil
}

func (w *Worker) saveAgentStart(invocation *agentInvocation, event agentresume.Event) error {
	recipe := agentresume.Recipe{Version: 1, Launch: invocation.launch, ConversationID: event.ConversationID,
		InvocationToken: invocation.token, RegisteredAt: w.currentTime().Round(0), Lifecycle: "active", Explicit: invocation.explicit}
	recipe.Directory = event.Directory
	if invocation.explicit {
		recipe.Lifecycle = "explicit"
	}
	if err := agentresume.ValidateRecipe(recipe); err != nil {
		return err
	}
	w.mu.Lock()
	if w.finished || w.reaped || w.checkpointWriter == nil {
		w.mu.Unlock()
		return fmt.Errorf("worker: agent recovery is unavailable")
	}
	w.recoveryState.Agent = &recipe
	w.recoveryState.Remote = nil
	if w.recoveryState.AgentResume == nil && invocation.resuming {
		w.recoveryState.AgentResume = &agentresume.Receipt{SourceSessionID: w.cfg.RecoveredFrom,
			Provider: recipe.Provider, ConversationID: recipe.ConversationID, InvocationToken: invocation.token, VerifiedAt: recipe.RegisteredAt}
	}
	w.mu.Unlock()
	return <-w.checkpoint()
}

func (w *Worker) finishAgent(invocation *agentInvocation, request protocol.Control) (bool, error) {
	w.agentMu.Lock()
	defer w.agentMu.Unlock()
	if request.SessionID != w.cfg.ID || request.AgentToken != invocation.token || w.agentInvocation != invocation {
		return false, fmt.Errorf("worker: agent invocation is no longer current")
	}
	w.mu.Lock()
	recipe := w.recoveryState.Agent
	if recipe == nil || recipe.InvocationToken != invocation.token {
		w.mu.Unlock()
		return invocation.registered, nil
	}
	closed := *recipe
	closed.Lifecycle = "closed"
	w.recoveryState.Agent = &closed
	w.mu.Unlock()
	return invocation.registered, <-w.checkpoint()
}

func (w *Worker) retireAgent(invocation *agentInvocation) {
	w.agentMu.Lock()
	defer w.agentMu.Unlock()
	if w.agentInvocation == invocation {
		w.agentInvocation = nil
	}
}

func (w *Worker) closeAgents() {
	w.agentMu.Lock()
	if w.agentInvocation != nil {
		_ = w.agentInvocation.conn.Close()
	}
	w.agentMu.Unlock()
	w.agentReaders.Wait()
}
