package tui

import (
	"fmt"
	"io"
	"strings"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/shaul/mesh/internal/cli"
)

const (
	pickerMoveKeys    = "↑/↓"
	pickerEnterKey    = "enter"
	pickerEscapeKey   = "esc"
	pickerCancelLabel = "cancel"
)

type pickerServicesMsg cli.PickerServicesUpdate
type pickerServicesFailedMsg struct{ err error }
type serviceTarget struct{ hostID, route string }

type serviceItem struct {
	target    serviceTarget
	hostAlias string
	website   servedWebsite
}

func (item serviceItem) FilterValue() string {
	return item.hostAlias + " " + item.website.name + " " + item.website.route
}

func mainItemKey(item list.Item) serviceTarget {
	switch item := item.(type) {
	case hostItem:
		return serviceTarget{item.host.id + "|" + item.host.alias, ""}
	case serviceItem:
		return item.target
	default:
		return serviceTarget{}
	}
}

func (m *model) refreshMainDelegate() {
	m.list.SetDelegate(hostDelegate{styles: m.styles, privacy: m.privacy, services: len(m.list.Items()) > len(m.hosts), feedback: m.serviceFeedback, pending: m.pendingService})
}

func (m *model) resetMainItems(selected serviceTarget, previous int) tea.Cmd {
	items := hostItems(m.hosts)
	command := m.list.SetItems(items)
	index := min(previous, max(0, len(items)-1))
	for candidate, item := range items {
		if mainItemKey(item) == selected {
			index = candidate
			break
		}
	}
	m.list.Select(index)
	m.refreshMainDelegate()
	m.resizeList()
	return command
}

func (m model) applyServices(update cli.PickerServicesUpdate) (model, tea.Cmd) {
	selected, previous := mainItemKey(m.list.SelectedItem()), m.list.Index()
	for index := range m.hosts {
		if m.hosts[index].id != update.Host.ID {
			continue
		}

		previous := m.hosts[index].served
		websites := servedWebsites(update.Catalog.Rows, update.Catalog.Stale)
		states := make(map[string]string, len(websites))
		for _, website := range websites {
			states[website.route] = website.state
		}
		for _, website := range previous {
			if state, exists := states[website.route]; !exists || state != website.state {
				delete(m.serviceFeedback, serviceTarget{update.Host.ID, website.route})
			}
		}
		m.hosts[index].served, m.hosts[index].servedKnown, m.hosts[index].servedStale = websites, true, update.Catalog.Stale

	}
	if update.Problem != "" {
		m.notice = update.Host.Alias + " services: " + update.Problem
	} else if strings.HasPrefix(m.notice, update.Host.Alias+" services: ") {
		m.notice = ""
	}
	if m.screen != hostScreen {
		return m, nil
	}
	return m, m.resetMainItems(selected, previous)
}

func (delegate hostDelegate) renderService(output io.Writer, browser list.Model, index int, item serviceItem) {
	selected := index == browser.Index()
	cursor := "  "
	if selected {
		cursor = delegate.styles.cursor.Render("› ")
	}
	state := item.website.state
	if state == "" {
		state = dashboardRunning
		if item.website.health != "healthy" {
			state = "unhealthy"
		}
	}
	feedback := delegate.feedback[item.target]
	status := cli.DashboardService{State: state, Failed: state == "unhealthy", HealthUnknown: state == usageUnknown}
	dashboard := dashboardModel{palette: dashboardTheme("current"), profile: colorprofile.TrueColor}
	stateText := dashboard.paint(dashboardServiceRole(status)).Render("● " + safeText(state))
	if item.website.stale {
		stateText = dashboard.paint(dashboardCachedStyle).Render("● " + state + " cached")
	}
	name := item.website.name
	if name == "" {
		name = "/" + item.website.route
	}
	name = safeText(delegate.privacy.Value("service", name))
	host := safeText(delegate.privacy.Value("host", item.hostAlias))
	row := cursor + cell(delegate.styles.item(selected).Render(name), max(8, browser.Width()-32)) + " " + cell(delegate.styles.muted.Render(host), 10) + " " + stateText
	address := item.website.url
	if item.website.row.Service.LocalOnly && item.hostAlias != "this host" {
		address = ":" + item.website.route + " on host"
	}
	detail := safeText(delegate.privacy.Value("url", address))
	if feedback != "" {
		detail = safeText(delegate.privacy.Value("notice", feedback)) + " · " + detail
	}
	if pending := delegate.pending; pending != nil && pending.HostID == item.target.hostID && pending.ServiceName == item.target.route {
		detail = serviceActionLabel(pending.Action) + "…  " + detail
	}
	_, _ = fmt.Fprint(output, truncate(row, browser.Width())+"\n"+truncate("    "+detail, browser.Width()))
}

func (m model) serviceFooter() string {
	hints := []hint{{"s", "stop"}, {"r", "restart"}, {"p", "ping"}, {"o", "open"}, {pickerEscapeKey, pickerCancelLabel}}
	lines := []string{}
	line := ""
	for _, hint := range hints {
		rendered := m.styles.hints(hint)
		if line != "" && ansi.StringWidth(line+"  "+rendered) > m.width {
			lines = append(lines, line)
			line = ""
		}
		if line != "" {
			line += "  "
		}
		line += rendered
	}
	return strings.Join(append(lines, line), "\n")
}

func (m model) mainChrome() (string, string, string) {
	header := justify(m.styles.title.Render("mesh"), m.styles.muted.Render(count(len(m.hosts), "host")), m.width)
	subtitle := "Choose a host."
	if len(m.hosts) == 0 {
		subtitle = "No hosts yet. Add one with mesh add [user@]host."
	}
	footer := m.styles.hints(hint{pickerMoveKeys, "move"}, hint{pickerEnterKey, "sessions"}, hint{pickerEscapeKey, pickerCancelLabel})
	if services := len(m.list.Items()) - len(m.hosts); services > 0 {
		header = justify(m.styles.title.Render("mesh"), m.styles.muted.Render(count(len(m.hosts), "host")+" · "+count(services, "service")), m.width)
		subtitle = "Choose a host or service."
	}
	if m.notice != "" {
		subtitle = m.notice
	}
	if _, ok := m.list.SelectedItem().(serviceItem); ok {
		footer = m.serviceFooter()
	}
	subtitle = safeText(m.privacy.Value("notice", subtitle))
	return truncate(header, m.width), truncate(m.styles.muted.Render(subtitle), m.width), footer
}
