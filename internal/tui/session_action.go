package tui

import (
	"context"
	"fmt"

	tea "charm.land/bubbletea/v2"

	"github.com/shaul/mesh/internal/cli"
)

type sessionActionTarget struct {
	hostAlias string
	sessionID string
}

type sessionActionState struct {
	target     sessionActionTarget
	action     cli.PickerSessionAction
	generation uint64
	phase      sessionActionPhase
}

type sessionActionPhase uint8

const (
	sessionActionIdle sessionActionPhase = iota
	sessionActionRunning
	sessionActionReconcilingSuccess
	sessionActionReconcilingFailure
	sessionActionFailed
)

type sessionActionResultMsg struct {
	target     sessionActionTarget
	action     cli.PickerSessionAction
	generation uint64
	err        error
}

func (m *model) startSessionAction(action cli.PickerSessionAction) tea.Cmd {
	if sessionActionBusy(m.sessionAction) {
		if m.notice == "" {
			m.notice = sessionActionNotice(m.sessionAction)
		}
		return nil
	}
	currentHost, currentSession, ok := m.currentSession()
	if !ok {
		return nil
	}
	if m.act == nil {
		m.notice = "Session actions are unavailable in this client."
		return nil
	}

	m.actionSeq++
	target := sessionActionTarget{hostAlias: currentHost.alias, sessionID: currentSession.id}
	m.sessionAction = sessionActionState{target: target, action: action, generation: m.actionSeq, phase: sessionActionRunning}
	m.notice = fmt.Sprintf("%s %s...", sessionActionProgress(action), currentSession.id)
	requestContext, cancel := context.WithCancel(m.ctx)
	m.cancelAction = cancel
	act := m.act
	request := cli.PickerSessionActionRequest{HostAlias: target.hostAlias, SessionID: target.sessionID, Action: action}
	generation := m.actionSeq
	return func() tea.Msg {
		defer cancel()
		err := act(requestContext, request)
		return sessionActionResultMsg{target: target, action: action, generation: generation, err: err}
	}
}

func (m model) applySessionAction(message sessionActionResultMsg) (model, tea.Cmd) {
	if m.screen != sessionScreen || m.sessionAction.phase != sessionActionRunning || message.generation != m.sessionAction.generation || message.target != m.sessionAction.target || message.action != m.sessionAction.action {
		return m, nil
	}
	m.cancelAction = nil
	if message.err != nil {
		m.sessionAction.phase = sessionActionReconcilingFailure
		m.notice = fmt.Sprintf("%s %s failed: %s", sessionActionVerb(message.action), message.target.sessionID, message.err)
	} else {
		m.sessionAction.phase = sessionActionReconcilingSuccess
		m.notice = fmt.Sprintf("%s %s; refreshing...", sessionActionCompleted(message.action), message.target.sessionID)
	}
	return m, m.restartCatalogRefreshLoop()
}

func sessionActionConfirmed(action sessionActionState, refreshed host) bool {
	if action.phase != sessionActionReconcilingSuccess && action.phase != sessionActionReconcilingFailure && action.phase != sessionActionFailed || refreshed.stale {
		return false
	}
	state, exists := sessionState(refreshed.sessions, action.target.sessionID)
	switch action.action {
	case cli.PickerKillSession:
		return !exists || state != "running" && state != "detached"
	case cli.PickerRemoveSession:
		return !exists
	default:
		return false
	}
}

// reconcileSessionAction consumes the first authoritative post-action catalog.
// Success remains busy until the requested state is visible. Failure becomes
// retryable after one refresh, while its notice remains until the user moves on.
func (m *model) reconcileSessionAction(refreshed host) bool {
	if sessionActionConfirmed(m.sessionAction, refreshed) {
		m.sessionAction = sessionActionState{}
		return true
	}
	if m.sessionAction.phase == sessionActionReconcilingFailure {
		m.sessionAction.phase = sessionActionFailed
	}
	return false
}

func sessionActionBusy(action sessionActionState) bool {
	return action.phase == sessionActionRunning || action.phase == sessionActionReconcilingSuccess || action.phase == sessionActionReconcilingFailure
}

func sessionActionNotice(action sessionActionState) string {
	switch action.phase {
	case sessionActionRunning:
		return fmt.Sprintf("%s %s...", sessionActionProgress(action.action), action.target.sessionID)
	case sessionActionReconcilingSuccess:
		return fmt.Sprintf("%s %s; refreshing...", sessionActionCompleted(action.action), action.target.sessionID)
	case sessionActionReconcilingFailure:
		return fmt.Sprintf("%s %s failed; checking current state...", sessionActionVerb(action.action), action.target.sessionID)
	default:
		return ""
	}
}

func (m *model) invalidateSessionAction() {
	if m.cancelAction != nil {
		m.cancelAction()
		m.cancelAction = nil
	}
	m.actionSeq++
	m.sessionAction = sessionActionState{}
}

func sessionActionVerb(action cli.PickerSessionAction) string {
	switch action {
	case cli.PickerKillSession:
		return "kill"
	case cli.PickerRemoveSession:
		return "remove"
	default:
		return "session action"
	}
}

func sessionActionProgress(action cli.PickerSessionAction) string {
	switch action {
	case cli.PickerKillSession:
		return "Killing"
	case cli.PickerRemoveSession:
		return "Removing"
	default:
		return "Updating"
	}
}

func sessionActionCompleted(action cli.PickerSessionAction) string {
	switch action {
	case cli.PickerKillSession:
		return "Killed"
	case cli.PickerRemoveSession:
		return "Removed"
	default:
		return "Updated"
	}
}
