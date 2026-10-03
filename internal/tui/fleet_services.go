package tui

import (
	"fmt"
	"io"
	"reflect"
	"strings"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
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
	pending := m.pendingService
	if m.serviceInvalid {
		pending = nil
	}
	m.list.SetDelegate(hostDelegate{styles: m.styles, privacy: m.privacy, feedback: m.serviceFeedback, pending: pending})
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
		m.updateServiceTargets(update.Host.ID, previous, websites)
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

func (m *model) updateServiceTargets(hostID string, previous, websites []servedWebsite) {
	observed := make(map[string]servedWebsite, len(websites))
	for _, website := range websites {
		observed[website.route] = website
	}
	for _, website := range previous {
		target := serviceTarget{hostID, website.route}
		next, exists := observed[website.route]
		if !exists || next.state != website.state {
			delete(m.serviceFeedback, target)
		}
		m.observeServiceAction(target, website, next, exists)
	}
}

func (m *model) observeServiceAction(target serviceTarget, previous, next servedWebsite, exists bool) {
	pending := m.pendingService
	if pending == nil || pending.HostID != target.hostID || pending.ServiceName != target.route {
		return
	}
	if exists && reflect.DeepEqual(previous.row, next.row) {
		return
	}
	// Count each change, including transitions that return to the starting value.
	m.serviceObservation++
	if !exists {
		m.serviceInvalid = true
	}
}

func (m model) mainExtraRows() int {
	if m.screen != hostScreen || len(m.list.Items()) <= len(m.hosts) {
		return 0
	}
	if _, selected := m.list.SelectedItem().(serviceItem); selected {
		return 3
	}
	return 2
}

func (m model) mainListView() string {
	return lipgloss.NewStyle().Height(m.list.Height() + m.mainExtraRows()).Render(m.list.View())
}

func (delegate hostDelegate) serviceName(item serviceItem) string {
	name := item.website.name
	if name == "" {
		name = "/" + item.website.route
	}
	return safeText(delegate.privacy.Value("service", name))
}

func (delegate hostDelegate) serviceColumns(browser list.Model) (int, int) {
	nameWidth, hostWidth, stateWidth := 1, 1, 1
	for _, entry := range browser.Items() {
		item, ok := entry.(serviceItem)
		if !ok {
			continue
		}
		nameWidth = max(nameWidth, ansi.StringWidth(delegate.serviceName(item)))
		hostWidth = max(hostWidth, ansi.StringWidth(safeText(delegate.privacy.Value("host", item.hostAlias))))
		stateWidth = max(stateWidth, ansi.StringWidth(item.website.state))
		if item.website.stale {
			stateWidth = max(stateWidth, ansi.StringWidth(item.website.state)+len(" cached"))
		}
	}
	hostWidth = min(12, hostWidth)
	nameWidth = min(24, nameWidth, max(1, browser.Width()-hostWidth-stateWidth-8))
	return nameWidth, hostWidth
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
	status := cli.DashboardService{State: state, Failed: state == "unhealthy", HealthUnknown: state == usageUnknown}
	dashboard := dashboardModel{palette: dashboardTheme("current"), profile: colorprofile.TrueColor}
	role := dashboardServiceRole(status)
	if item.website.stale {
		role = dashboardCachedStyle
		state += " cached"
	}
	paint := dashboard.paint(role)
	nameWidth, hostWidth := delegate.serviceColumns(browser)
	host := safeText(delegate.privacy.Value("host", item.hostAlias))
	row := cursor + paint.Render("●") + " " + cell(delegate.styles.item(selected).Render(delegate.serviceName(item)), nameWidth) + "  " + cell(delegate.styles.muted.Render(host), hostWidth) + "  " + paint.Render(safeText(state))
	start, _ := browser.Paginator.GetSliceBounds(len(browser.Items()))
	previousService := false
	if index > 0 {
		_, previousService = browser.Items()[index-1].(serviceItem)
	}
	if index == start || !previousService {
		_, _ = fmt.Fprintln(output, "\n"+delegate.styles.muted.Render("  Services"))
	}
	_, _ = fmt.Fprint(output, truncate(row, browser.Width()))
	if !selected {
		return
	}
	_, _ = fmt.Fprint(output, "\n"+delegate.styles.muted.Render(truncate("    "+delegate.serviceDetail(item), browser.Width())))
}

func (delegate hostDelegate) serviceDetail(item serviceItem) string {
	address := item.website.url
	if item.website.row.Service.LocalOnly && item.hostAlias != "this host" {
		address = ":" + item.website.route + " on host"
	}
	detail := safeText(delegate.privacy.Value("url", address))
	if feedback := delegate.feedback[item.target]; feedback != "" {
		detail = safeText(delegate.privacy.Value("notice", feedback)) + " · " + detail
	}
	if pending := delegate.pending; pending != nil && pending.HostID == item.target.hostID && pending.ServiceName == item.target.route {
		detail = serviceActionLabel(pending.Action) + "…  " + detail
	}
	return detail
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
