package tui

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/shaul/mesh/internal/cli"
)

const (
	catalogRefreshInterval = 2 * time.Second
	catalogRefreshTimeout  = 1500 * time.Millisecond
)

type catalogRefreshTickMsg struct {
	epoch uint64
	at    time.Time
}

type catalogRefreshResultMsg struct {
	epoch     uint64
	hostAlias string
	snapshot  cli.PickerHostSnapshot
	err       error
}

func (m *model) restartSessionScreenLoops() tea.Cmd {
	return tea.Batch(m.restartInspectionLoop(), m.restartCatalogRefreshLoop())
}

func (m *model) invalidateSessionScreenLoops() {
	m.stopInspection()
	m.stopSessionSummaryInspection()
	m.refreshEpoch++
	m.invalidateCatalogRefreshLoop()
	m.invalidateSessionAction()
}

func (m *model) restartCatalogRefreshLoop() tea.Cmd {
	m.invalidateCatalogRefreshLoop()
	return m.refreshSelectedHost(m.catalogEpoch)
}

func (m *model) invalidateCatalogRefreshLoop() {
	m.stopCatalogRefresh()
	m.catalogEpoch++
}

func (m *model) refreshSelectedHost(epoch uint64) tea.Cmd {
	if m.screen != sessionScreen || m.refresh == nil {
		return nil
	}
	m.stopCatalogRefresh()
	hostAlias := m.currentHost().alias
	requestContext, cancel := context.WithTimeout(m.ctx, catalogRefreshTimeout)
	m.cancelRefresh = cancel
	refresh := m.refresh
	return func() tea.Msg {
		defer cancel()
		snapshot, err := refresh(requestContext, hostAlias)
		return catalogRefreshResultMsg{epoch: epoch, hostAlias: hostAlias, snapshot: snapshot, err: err}
	}
}

func (m model) applyCatalogRefresh(message catalogRefreshResultMsg) (model, tea.Cmd) {
	if m.screen != sessionScreen || message.epoch != m.catalogEpoch || message.hostAlias != m.currentHost().alias {
		return m, nil
	}
	m.cancelRefresh = nil
	nextRefresh := m.scheduleCatalogRefresh(message.epoch)
	if message.err != nil {
		return m, nextRefresh
	}

	selectedID := m.selectedSessionID()
	selectedIndex := m.list.Index()
	previousHost := m.currentHost()
	previousState, _ := sessionState(previousHost.sessions, selectedID)
	refreshedHost := hostCatalog(cli.PickerInput{Hosts: []cli.HostSessions{message.snapshot.Sessions}})[0]
	actionConfirmed := m.reconcileSessionAction(refreshedHost)
	if actionConfirmed || sessionCatalogChanged(previousHost, refreshedHost) && m.sessionAction.phase == sessionActionIdle {
		m.notice = ""
	}
	m.hosts[m.selectedHost].sessions = refreshedHost.sessions
	m.hosts[m.selectedHost].stale = refreshedHost.stale
	m.pruneSessionSummaries(message.hostAlias, refreshedHost.sessions)
	if services := message.snapshot.Services; services != nil {
		websites := servedWebsites(services.Rows, services.Stale)
		m.hosts[m.selectedHost].served = websites
		m.hosts[m.selectedHost].servedKnown = true
		m.hosts[m.selectedHost].servedStale = services.Stale || anyServedWebsiteStale(websites)
	} else {
		m.hosts[m.selectedHost].servedKnown = true
		m.hosts[m.selectedHost].servedStale = true
	}

	listCommand := m.list.SetItems(sessionItems(refreshedHost.sessions))
	if len(refreshedHost.sessions) > 0 {
		m.list.Select(preservedSessionIndex(refreshedHost.sessions, selectedID, selectedIndex))
	}
	m.resizeList()
	m.refreshSessionDelegate()

	newSelectedID := m.selectedSessionID()
	newState, _ := sessionState(refreshedHost.sessions, newSelectedID)
	selectionChanged := newSelectedID != selectedID
	inspectionFactsChanged := previousHost.stale != refreshedHost.stale || previousState != newState
	if m.fullPreview && newState != "running" && newState != "detached" {
		m.fullPreview = false
	}
	if !selectionChanged && !inspectionFactsChanged {
		return m, tea.Batch(listCommand, nextRefresh)
	}
	if selectionChanged {
		m.fullPreview = false
	}
	return m, tea.Batch(listCommand, m.restartInspectionLoop(), nextRefresh)
}

func sessionCatalogChanged(previous, refreshed host) bool {
	if previous.stale != refreshed.stale || len(previous.sessions) != len(refreshed.sessions) {
		return true
	}
	for index := range previous.sessions {
		if previous.sessions[index].id != refreshed.sessions[index].id || previous.sessions[index].state != refreshed.sessions[index].state {
			return true
		}
	}
	return false
}

func preservedSessionIndex(sessions []session, selectedID string, previousIndex int) int {
	for index := range sessions {
		if sessions[index].id == selectedID {
			return index
		}
	}
	return min(max(0, previousIndex), len(sessions)-1)
}

func sessionState(sessions []session, id string) (string, bool) {
	for index := range sessions {
		if sessions[index].id == id {
			return sessions[index].state, true
		}
	}
	return "", false
}

func (m model) scheduleCatalogRefresh(epoch uint64) tea.Cmd {
	return tea.Tick(catalogRefreshInterval, func(at time.Time) tea.Msg {
		return catalogRefreshTickMsg{epoch: epoch, at: at}
	})
}

func (m *model) stopCatalogRefresh() {
	if m.cancelRefresh == nil {
		return
	}
	m.cancelRefresh()
	m.cancelRefresh = nil
}
