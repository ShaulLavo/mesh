package tui

import (
	"strings"

	"github.com/shaul/mesh/internal/cli"
)

const privacyPreviewPlaceholder = "Screen preview hidden for privacy."

// Clone only presentation data. IDs and observation keys still address the
// original host/session, and publications never inherit masked names.
func (m dashboardModel) privacyDisplay() dashboardModel {
	if m.privacy == nil {
		return m
	}
	mask := m.privacy
	m.notice = mask.Value("notice", m.notice)
	m.hosts = append([]cli.DashboardHostView(nil), m.hosts...)
	for i := range m.hosts {
		h := &m.hosts[i]
		h.Host.Alias = mask.Value("host", h.Host.Alias)
		h.Problem = mask.Value("error", h.Problem)
		h.Sessions.Rows = append([]cli.DashboardSession(nil), h.Sessions.Rows...)
		for j := range h.Sessions.Rows {
			s := &h.Sessions.Rows[j]
			s.Name = mask.Value("session", s.Name)
			s.Label = mask.Value("session", s.Label)
			s.Command = strings.Join(mask.Command(strings.Fields(s.Command)), " ")
		}
		h.Services.Rows = append([]cli.DashboardService(nil), h.Services.Rows...)
		for j := range h.Services.Rows {
			s := &h.Services.Rows[j]
			s.Name = mask.Value("service", s.Name)
			s.Problem = mask.Value("error", s.Problem)
		}
	}
	summaries := make(map[dashboardSessionTarget]sessionLiveSummary, len(m.sessionSummaries))
	for key, summary := range m.sessionSummaries {
		summary.currentDirectory = mask.Value("path", summary.currentDirectory)
		summary.terminalTitle = mask.Value("session", summary.terminalTitle)
		summary.foregroundCommand = strings.Join(mask.Command(strings.Fields(summary.foregroundCommand)), " ")
		summaries[key] = summary
	}
	m.sessionSummaries = summaries
	m.usage.accounts = append([]dashboardUsageAccount(nil), m.usage.accounts...)
	for i := range m.usage.accounts {
		m.usage.accounts[i].Label = mask.Value("account", m.usage.accounts[i].Label)
	}
	// Attention contains preformatted names. Recompute it from this frame's
	// display values rather than reusing any raw cached strings.
	m.attentionData = nil
	return m
}

func (m model) privacyForeground(current session, fallback string) string {
	if endedSession(current) && current.recovery != nil {
		saved := current.recovery
		switch {
		case saved.Remote != nil:
			return "Target: " + m.privacy.Value("host", saved.Remote.HostID) + "/" + m.privacy.Value("session", saved.Remote.SessionID)
		case saved.Agent != nil:
			return string(saved.Agent.Provider) + " conversation: " + m.privacy.Value("conversation", saved.Agent.ConversationID) + " · " + string(saved.Agent.Lifecycle)
		case saved.Restart != nil:
			return "Restart command: " + strings.Join(m.privacy.Command(saved.Restart.Argv), " ")
		}
	}
	if !endedSession(current) && m.inspection.kind == inspectionReady && m.inspection.value.ForegroundCommand != "" {
		return strings.Join(m.privacy.Command(strings.Fields(m.inspection.value.ForegroundCommand)), " ")
	}
	// These are renderer-owned states, not process-provided foreground text.
	return fallback
}

func (delegate sessionDelegate) privacyRow(row sessionRow) sessionRow {
	if delegate.privacy == nil {
		return row
	}
	row.primary = delegate.privacy.Value("session", row.primary)
	row.secondary = delegate.privacy.Value("session", row.secondary)
	if row.context != "previous attempt" {
		row.context = delegate.privacy.Value("context", row.context)
	}
	return row
}
