package tui

import (
	"path"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/shaul/mesh/internal/recovery"
)

func endedSession(current session) bool {
	return current.state == "interrupted" || current.state == "exited"
}

func recoveryActionLabel(current session) string {
	if current.recovery != nil && current.recovery.Remote != nil {
		return "Reconnect to target"
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
	if !ok || !endedSession(current) {
		return true, nil
	}
	action := recovery.ActionShell
	if key == "c" {
		action = recovery.ActionCommand
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
	return details
}

func cloneRecovery(source *recovery.Record) *recovery.Record {
	if source == nil {
		return nil
	}
	cloned := *source
	cloned.Lines = append([]string(nil), source.Lines...)
	cloned.Command = append([]string(nil), source.Command...)
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
