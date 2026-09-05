package tui

import (
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/protocol"
)

type hostCatalogLoadedMsg struct {
	hosts []cli.HostSessions
	err   error
}

func (m model) loadHostCatalog() tea.Cmd {
	if m.loadHosts == nil {
		return nil
	}
	return func() tea.Msg {
		hosts, err := m.loadHosts(m.ctx)
		return hostCatalogLoadedMsg{hosts: hosts, err: err}
	}
}

func (m model) applyLoadedHosts(message hostCatalogLoadedMsg) (model, tea.Cmd) {
	m.loadHosts = nil
	if message.err != nil || m.selection != nil {
		return m, nil
	}
	selectedAlias := ""
	if m.screen == sessionScreen {
		selectedAlias = m.currentHost().alias
	} else if selected, ok := m.list.SelectedItem().(hostItem); ok {
		selectedAlias = selected.host.alias
	}
	merged := cloneHosts(m.hosts)
	for _, loaded := range hostCatalog(cli.PickerInput{Hosts: message.hosts}) {
		if loaded.local {
			continue
		}
		found := false
		for index := range merged {
			if merged[index].alias == loaded.alias {
				// The local list and an open session screen already have their own
				// current reader. A delayed network catalog cannot replace them.
				if !merged[index].local && (m.screen != sessionScreen || merged[index].alias != selectedAlias) {
					merged[index] = loaded
				}
				found = true
				break
			}
		}
		if !found {
			merged = append(merged, loaded)
		}
	}
	m.hosts = merged
	m.rememberContainingSessionSnapshots()
	if m.screen == sessionScreen {
		m.refreshSessionDelegate()
		return m, nil
	}
	command := m.list.SetItems(hostItems(m.hosts))
	for index, current := range m.hosts {
		if current.alias == selectedAlias {
			m.list.Select(index)
			break
		}
	}
	return m, command
}

func (m model) selectedSessionAttached() bool {
	_, current, ok := m.currentSession()
	if !ok || current.state == "interrupted" || current.state == "exited" {
		return false
	}
	if m.inspection.kind == inspectionReady && m.inspection.target.sessionID == current.id && !m.inspection.beforePicker {
		return m.inspection.value.Attached
	}
	return current.state == "running"
}

func (m model) hostNames() map[string]string {
	names := make(map[string]string, len(m.hosts))
	for _, current := range m.hosts {
		names[current.id] = current.alias
	}
	return names
}

func nestedSessionLabel(nested []protocol.SessionIdentity, names map[string]string) string {
	labels := make([]string, 0, len(nested))
	for _, current := range nested {
		name := names[current.HostID]
		if name == "" {
			name = current.HostID
		}
		labels = append(labels, safeText(name)+"/"+safeText(current.SessionID))
	}
	if len(labels) == 0 {
		return ""
	}
	sort.Strings(labels)
	return "on " + strings.Join(labels, ", ")
}

func appendRowLabel(current, label string) string {
	if current == "" {
		return label
	}
	if label == "" {
		return current
	}
	return current + "  " + label
}

func (m model) sessionViaLabels() map[string]string {
	labels := make(map[string]string)
	current := m.currentHost()
	for index, containing := range m.containingPath {
		if containing.HostID == current.id && index+1 < len(m.containingPath) {
			labels[containing.SessionID] = "via " + safeText(m.containingPath[index+1].SessionID)
		}
	}
	for target, summary := range m.summaries {
		for _, outer := range m.hosts {
			if !outer.local || outer.alias != target.hostAlias {
				continue
			}
			for _, nested := range summary.nested {
				if nested.HostID == current.id {
					labels[nested.SessionID] = "via " + safeText(target.sessionID)
				}
			}
		}
	}
	return labels
}
