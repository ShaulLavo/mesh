package tui

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/protocol"
)

const (
	sessionSummaryRefreshInterval = 4 * time.Second
	sessionSummaryMaxConcurrency  = 3
)

// sessionLiveSummary is the mutable worker state needed to name a row. Launch
// command and directory remain on session because they are durable fallback
// facts, while these values belong to one live host/session observation.
type sessionLiveSummary struct {
	currentDirectory  string
	foregroundCommand string
	terminalTitle     string
	lastOutputAt      *time.Time
	observedAt        time.Time
	receivedAt        time.Time
	nested            []protocol.SessionIdentity
}

// newSessionLiveSummary keeps only the facts a list row renders. The preview
// itself stays with the selected inspection so summaries remain cheap to hold
// for every visible session.
func newSessionLiveSummary(value cli.SessionInspection, receivedAt time.Time) sessionLiveSummary {
	summary := sessionLiveSummary{
		currentDirectory:  value.CurrentDirectory,
		foregroundCommand: value.ForegroundCommand,
		terminalTitle:     value.TerminalTitle,
		observedAt:        value.ObservedAt,
		receivedAt:        receivedAt,
		nested:            protocol.CloneSessionIdentities(value.Nested),
	}
	if value.LastOutputAt != nil {
		lastOutputAt := *value.LastOutputAt
		summary.lastOutputAt = &lastOutputAt
	}
	return summary
}

type sessionSummaryResult struct {
	target inspectionTarget
	value  cli.SessionInspection
	err    error
}

type sessionSummariesResultMsg struct {
	hostAlias  string
	generation uint64
	values     map[inspectionTarget]cli.SessionInspection
}

// inspectVisibleSessionSummaries refreshes only the current page. The selected
// row already has a full inspection in flight, so the other rows request a 1x1
// preview and use only the process and directory facts from the response.
func (m *model) inspectVisibleSessionSummaries() tea.Cmd {
	if m.cancelSummary != nil {
		return nil
	}
	targets := m.visibleSessionSummaryTargets()
	if len(targets) == 0 {
		return nil
	}

	m.summarySeq++
	generation := m.summarySeq
	hostAlias := m.currentHost().alias
	requestContext, cancel := context.WithTimeout(m.ctx, inspectionRequestTimeout)
	m.cancelSummary = cancel
	inspect := m.inspect
	return func() tea.Msg {
		defer cancel()
		results := make(chan sessionSummaryResult, len(targets))
		jobs := make(chan inspectionTarget, len(targets))
		for _, target := range targets {
			jobs <- target
		}
		close(jobs)

		for range min(sessionSummaryMaxConcurrency, len(targets)) {
			go func() {
				for target := range jobs {
					value, err := inspect(requestContext, cli.PickerInspectRequest{
						HostAlias:   target.hostAlias,
						SessionID:   target.sessionID,
						PreviewCols: 1,
						PreviewRows: 1,
					})
					results <- sessionSummaryResult{target: target, value: value, err: err}
				}
			}()
		}

		values := make(map[inspectionTarget]cli.SessionInspection, len(targets))
		for range targets {
			result := <-results
			if result.err == nil {
				values[result.target] = result.value
			}
		}
		return sessionSummariesResultMsg{hostAlias: hostAlias, generation: generation, values: values}
	}
}

func (m model) visibleSessionSummaryTargets() []inspectionTarget {
	if m.screen != sessionScreen || m.inspect == nil || m.currentHost().stale {
		return nil
	}
	items := m.list.VisibleItems()
	perPage := max(1, m.list.Paginator.PerPage)
	start := min(len(items), m.list.Paginator.Page*perPage)
	end := min(len(items), start+perPage)
	selectedID := m.selectedSessionID()
	targets := make([]inspectionTarget, 0, end-start)
	for _, item := range items[start:end] {
		row, ok := item.(sessionItem)
		if !ok || row.session.id == selectedID || row.session.state != "running" && row.session.state != "detached" {
			continue
		}
		target := inspectionTarget{hostAlias: m.currentHost().alias, sessionID: row.session.id}
		if m.inspectionTargetContainsPicker(target) {
			continue
		}
		if summary, exists := m.summaries[target]; exists && m.now.Before(summary.receivedAt.Add(sessionSummaryRefreshInterval)) {
			continue
		}
		targets = append(targets, target)
	}
	return targets
}

func (m model) applySessionSummaries(message sessionSummariesResultMsg) model {
	if m.screen != sessionScreen || message.generation != m.summarySeq || message.hostAlias != m.currentHost().alias {
		return m
	}
	m.cancelSummary = nil
	next := make(map[inspectionTarget]sessionLiveSummary, len(m.summaries)+len(message.values))
	for target, summary := range m.summaries {
		next[target] = summary
	}
	for target, value := range message.values {
		if target.hostAlias != message.hostAlias || !liveSessionExists(m.currentHost().sessions, target.sessionID) {
			continue
		}
		next[target] = newSessionLiveSummary(value, m.now)
	}
	m.summaries = next
	m.refreshSessionDelegate()
	return m
}

func (m *model) rememberSessionSummary(target inspectionTarget, value cli.SessionInspection) {
	next := make(map[inspectionTarget]sessionLiveSummary, len(m.summaries)+1)
	for existingTarget, summary := range m.summaries {
		next[existingTarget] = summary
	}
	next[target] = newSessionLiveSummary(value, m.now)
	m.summaries = next
}

func (m *model) pruneSessionSummaries(hostAlias string, sessions []session) {
	ids := make(map[string]struct{}, len(sessions))
	for _, current := range sessions {
		ids[current.id] = struct{}{}
	}
	next := make(map[inspectionTarget]sessionLiveSummary, len(m.summaries))
	for target, summary := range m.summaries {
		if target.hostAlias == hostAlias {
			if _, exists := ids[target.sessionID]; !exists {
				continue
			}
		}
		next[target] = summary
	}
	m.summaries = next
}

func liveSessionExists(sessions []session, id string) bool {
	for _, current := range sessions {
		if current.id == id {
			return current.state == "running" || current.state == "detached"
		}
	}
	return false
}

func (m *model) stopSessionSummaryInspection() {
	if m.cancelSummary == nil {
		return
	}
	m.cancelSummary()
	m.cancelSummary = nil
	m.summarySeq++
}
