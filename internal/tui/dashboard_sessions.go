package tui

import (
	"context"
	"path"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/shaul/mesh/internal/cli"
)

const dashboardSessionRefresh = 10 * time.Second

type dashboardSessionTarget struct{ hostID, sessionID string }
type dashboardSessionSummariesMsg map[dashboardSessionTarget]sessionLiveSummary

func (m *dashboardModel) inspectSessions() tea.Cmd {
	if m.inspect == nil || m.inspectionPending || m.now.Before(m.nextInspection) {
		return nil
	}
	targets := m.visibleSessionTargets()
	if len(targets) == 0 {
		return nil
	}
	m.inspectionPending = true
	m.nextInspection = m.now.Add(dashboardSessionRefresh)
	inspect, parent := m.inspect, m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, 3*time.Second)
		defer cancel()
		values := dashboardSessionSummariesMsg{}
		for target, request := range targets {
			if ctx.Err() != nil {
				break
			}
			requestCtx, stop := context.WithTimeout(ctx, 500*time.Millisecond)
			value, err := inspect(requestCtx, request)
			stop()
			if err != nil || value.Recovery != nil {
				continue
			}
			values[target] = newSessionLiveSummary(value, time.Now())
		}
		return values
	}
}

func (m dashboardModel) visibleSessionTargets() map[dashboardSessionTarget]cli.PickerInspectRequest {
	targets := map[dashboardSessionTarget]cli.PickerInspectRequest{}
	selected := m.liveSelection(min(6, m.visibleSessionLimit()))
	for index, host := range m.hosts {
		for _, session := range host.Sessions.Rows[:selected[index]] {
			target := dashboardSessionTarget{host.Host.ID, session.ID}
			targets[target] = cli.PickerInspectRequest{HostID: host.Host.ID, SessionID: session.ID, PreviewCols: 1, PreviewRows: 1}
		}
	}
	return targets
}

func (m dashboardModel) visibleSessionLimit() int {
	if m.width < 80 || m.height < 24 {
		return 0
	}
	if m.width >= 140 && m.height >= 40 {
		m.layout = m.currentLayout()
		m.layoutPrepared = true
	}
	lines, _ := m.fleetBody()
	budget := m.height - len(m.header()) - len(lines) - 2
	if m.usageEnabled && (m.width < 140 || m.height < 40) {
		attention := m.attention((m.width-1)/2, min(7, max(0, budget-5)))
		return min(2, max(0, budget-len(attention)-3))
	}
	if m.usageEnabled {
		budget = min(budget, 17)
	} else if m.width < 140 && budget >= 6 {
		budget /= 2
	}
	return max(0, budget-4)
}

func (m *dashboardModel) acceptSessionSummaries(values dashboardSessionSummariesMsg) {
	for target, summary := range values {
		if m.hasSession(target) {
			m.sessionSummaries[target] = summary
		}
	}
}

func (m *dashboardModel) pruneSessionSummaries() {
	for target := range m.sessionSummaries {
		if !m.hasSession(target) {
			delete(m.sessionSummaries, target)
		}
	}
}

func (m dashboardModel) hasSession(target dashboardSessionTarget) bool {
	for _, host := range m.hosts {
		if host.Host.ID != target.hostID {
			continue
		}
		for _, session := range host.Sessions.Rows {
			if session.ID == target.sessionID {
				return true
			}
		}
	}
	return false
}

func (m dashboardModel) sessionDescription(host cli.DashboardHostView, session cli.DashboardSession) (name, activity, observedAge string) {
	name = session.Name
	if session.Label != "" {
		name = session.Label
	}
	if name == "" {
		name = "Terminal"
	}
	activity = dashboardProgram(session.Command) + " · launch only"
	observedAge = "--"
	summary, ok := m.sessionSummaries[dashboardSessionTarget{host.Host.ID, session.ID}]
	if !ok {
		return safeText(name), safeText(activity), observedAge
	}
	if session.Label == "" {
		name = dashboardSummaryName(summary, name)
	}
	activity = dashboardProgram(summary.foregroundCommand)
	if summary.foregroundCommand == "" {
		activity = "Unknown"
	}
	observedAge = dashboardAge(m.now, summary.receivedAt)
	if dashboardSessionsCached(host, m.now) || m.now.Sub(summary.receivedAt) >= 30*time.Second {
		activity += " · stale"
	}
	return safeText(name), safeText(activity), observedAge
}

func dashboardSummaryName(summary sessionLiveSummary, fallback string) string {
	if title := strings.TrimSpace(summary.terminalTitle); title != "" {
		return title
	}
	if directory := strings.TrimRight(summary.currentDirectory, "/"); directory != "" {
		return path.Base(directory)
	}
	return fallback
}

func dashboardProgram(command string) string {
	program := path.Base(firstCommandWord(command))
	switch program {
	case "sh", "bash", "zsh", "fish", "dash", "ksh", "-bash", "-zsh":
		return "Shell"
	case "claude":
		return "Claude"
	case "codex":
		return "Codex"
	case ".", "/", "":
		return "Unknown"
	default:
		return program
	}
}
