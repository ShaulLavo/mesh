package tui

import (
	"path"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/shaul/mesh/internal/agentresume"
	"github.com/shaul/mesh/internal/recovery"
)

func endedSession(current session) bool {
	return current.state == "interrupted" || current.state == "exited"
}

func recoveryActionLabel(current session) string {
	if current.recovery != nil && current.recovery.Remote != nil {
		return "Reconnect to target"
	}
	if hasPendingAgent(current) {
		return "Resume conversation"
	}
	if current.recovery != nil && current.recovery.Agent != nil && current.recovery.Agent.Lifecycle != agentresume.Closed {
		return "Resume " + string(current.recovery.Agent.Provider)
	}
	if len(current.command) == 0 {
		return "Open shell"
	}
	switch path.Base(current.command[0]) {
	case "bash", "zsh", "sh", "fish", "dash", "ksh":
		if !interactiveShellArguments(current.command[1:]) {
			return "Open shell"
		}
		return "Recover shell"
	default:
		return "Open shell"
	}
}

func interactiveShellArguments(arguments []string) bool {
	for _, argument := range arguments {
		if argument != "-l" && argument != "-i" && argument != "-il" && argument != "-li" && argument != "--login" {
			return false
		}
	}
	return true
}

func (m *model) selectRecoveryAction(key string) (bool, tea.Cmd) {
	if m.screen != sessionScreen || m.fullPreview {
		return false, nil
	}
	_, current, ok := m.currentSession()
	if !ok {
		return true, nil
	}
	if key == "a" && !canResumeAgent(current) || key != "a" && !endedSession(current) {
		return true, nil
	}
	if m.currentHost().stale {
		m.notice = m.currentHost().alias + " is offline; wake it first"
		return true, nil
	}
	action := recovery.ActionShell
	if key == "c" {
		action = recovery.ActionCommand
	}
	if key == "a" {
		action = recovery.ActionAgent
	}
	m.selection = attachSelection{hostAlias: m.currentHost().alias, sessionID: current.id, relaunch: true, recoveryAction: action}
	return true, tea.Quit
}

func savedRecoveryDetails(current session) inspectionDetails {
	details := inspectionDetails{
		directoryLabel: "saved path", directory: safeText(current.cwd),
		foreground: "ended", title: "unavailable", attachment: safeText(current.state),
		output: "checkpoint unavailable", screenStatus: "Previous output unavailable",
		preview: []string{"No checkpoint was saved. Recovery uses the launch directory."},
	}
	if current.recovery == nil {
		details.directorySource = "launch-only"
		if current.recoveryError != "" {
			details.preview = []string{safeText(current.recoveryError)}
		}
		return details
	}
	saved := current.recovery
	details.directory = safeText(saved.ShellDirectory)
	details.directorySource = safeText(string(saved.DirectorySource))
	if saved.DirectorySource != recovery.DirectoryShell {
		details.directorySource += " fallback"
	}
	details.title = safeText(saved.Title)
	details.output = "launch-only recovery"
	if saved.CheckpointAt.IsZero() {
		details.directorySource = "launch-only"
	} else {
		details.output = "saved " + saved.CheckpointAt.Format(time.RFC3339)
		details.screenStatus = "Previous output · " + saved.CheckpointAt.Format(time.RFC3339)
		details.preview = make([]string, len(saved.Lines))
		for index, line := range saved.Lines {
			details.preview[index] = safeText(line)
		}
		if len(details.preview) == 0 {
			details.preview = []string{"No output was saved at this checkpoint."}
		}
	}
	if saved.Restart != nil {
		details.foreground = "Restart command: " + safeText(strings.Join(saved.Restart.Argv, " "))
	}
	if saved.Remote != nil {
		details.foreground = "Target: " + safeText(saved.Remote.HostID) + "/" + safeText(saved.Remote.SessionID)
	}
	if saved.Agent != nil && saved.Remote == nil {
		details.foreground = string(saved.Agent.Provider) + " conversation: " + safeText(saved.Agent.ConversationID) + " · " + string(saved.Agent.Lifecycle)
	}
	if current.agentStatus != "" {
		details.output = "conversation " + safeText(current.agentStatus) + " · " + details.output
	}
	return details
}

func canResumeAgent(current session) bool {
	if endedSession(current) && hasPendingAgent(current) {
		return true
	}
	return current.recovery != nil && current.recovery.Agent != nil && (endedSession(current) || current.recovery.Agent.Lifecycle != agentresume.Active)
}

func hasPendingAgent(current session) bool {
	return current.recovery != nil && current.recovery.Agent == nil && current.recovery.Remote == nil && current.agentStatus == "unverified"
}

func (m model) recoveryHints(current session, action string) string {
	if canResumeAgent(current) {
		return m.styles.hints(hint{"enter", action}, hint{"a", "Resume conversation"}, hint{"s", "Open shell"}, hint{"space", "output"}, hint{"esc", "hosts"})
	}
	return m.styles.hints(hint{"enter", action}, hint{"s", "Open shell"}, hint{"c", "Restart command"}, hint{"space", "output"}, hint{"x", "forget"}, hint{"esc", "hosts"})
}

func cloneRecovery(source *recovery.Record) *recovery.Record {
	if source == nil {
		return nil
	}
	cloned := *source
	cloned.Lines = append([]string(nil), source.Lines...)
	cloned.Command = append([]string(nil), source.Command...)
	if source.Agent != nil {
		agent := *source.Agent
		agent.Options = append([]string(nil), source.Agent.Options...)
		cloned.Agent = &agent
	}
	if source.AgentResume != nil {
		receipt := *source.AgentResume
		cloned.AgentResume = &receipt
	}
	if source.Restart != nil {
		restart := *source.Restart
		restart.Argv = append([]string(nil), source.Restart.Argv...)
		cloned.Restart = &restart
	}
	if source.Remote != nil {
		remote := *source.Remote
		cloned.Remote = &remote
	}
	return &cloned
}

func groupRecoveryAttempts(sessions []session) []session {
	byID := make(map[string]session, len(sessions))
	for _, current := range sessions {
		byID[current.id] = current
	}
	children := make(map[string][]string)
	parents := make(map[string]string)
	for _, current := range sessions {
		if _, exists := byID[current.replacementID]; !exists || current.replacementID == current.id || !endedSession(current) {
			continue
		}
		parents[current.id] = current.replacementID
		children[current.replacementID] = append(children[current.replacementID], current.id)
	}
	var ordered []session
	seen := make(map[string]bool)
	for _, current := range sessions {
		if parents[current.id] == "" {
			ordered = appendRecoveryGroup(ordered, current.id, byID, children, seen)
		}
	}
	// Corrupt cycles retain every record, but never loop or hide a session.
	for _, current := range sessions {
		ordered = appendRecoveryGroup(ordered, current.id, byID, children, seen)
	}
	return ordered
}

func appendRecoveryGroup(ordered []session, root string, byID map[string]session, children map[string][]string, seen map[string]bool) []session {
	pending := []string{root}
	for len(pending) > 0 {
		id := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if seen[id] {
			continue
		}
		seen[id] = true
		current := byID[id]
		current.previousAttempt = id != root
		ordered = append(ordered, current)
		pending = append(pending, children[id]...)
	}
	return ordered
}
