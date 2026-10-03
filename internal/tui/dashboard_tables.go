package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/shaul/mesh/internal/cli"
)

const dashboardRunning = "running"

type dashboardSummaryRow struct {
	text     string
	failed   bool
	priority int
}

func (m dashboardModel) summaryRows(services bool, width int) ([]dashboardSummaryRow, int) {
	var rows []dashboardSummaryRow
	total := 0
	if services && m.usageEnabled {
		m.serviceHostWidth = m.usageServiceHostWidth()
	}
	hostWidth := 0
	if !services {
		hostWidth = m.sessionHostWidth(width)
	}
	for _, host := range m.hosts {
		if services {
			total += host.Services.Total
			rows = append(rows, m.serviceRows(host, width)...)
			continue
		}
		total += host.Sessions.Total
		for _, session := range host.Sessions.Rows {
			rows = append(rows, dashboardSummaryRow{text: m.sessionRow(host, session, width, hostWidth)})
		}
	}
	if services {
		sort.SliceStable(rows, func(a, b int) bool { return rows[a].priority < rows[b].priority })
	}
	return rows, total
}
func (m dashboardModel) sessionRow(host cli.DashboardHostView, session cli.DashboardSession, width, hostWidth int) string {
	name, activity, observedAge := m.sessionDescription(host, session)
	id := safeText(m.privacy.Text(session.ID))
	state := m.paint(dashboardMutedStyle).Render(catalogSessionState(session.State))
	if session.State == dashboardRunning {
		state = m.paint(dashboardGoodStyle).Render(catalogSessionState(session.State))
	}
	cached := dashboardSessionsCached(host, m.now)
	if cached {
		state = catalogSessionState(session.State)
	}
	if m.usageEnabled {
		if cached {
			state = m.paint(dashboardCachedStyle).Render("cached " + catalogSessionState(session.State))
		}
		return m.usageSessionColumns(m.paint(dashboardTitleStyle).Bold(false).Render(safeText(host.Host.Alias)), id, name, state, activity, width, hostWidth)
	}
	text := dashboardSessionColumns(m.paint(dashboardTitleStyle).Bold(false).Render(safeText(host.Host.Alias)), id, name, state, activity, observedAge, width, hostWidth)
	if cached {
		text = m.paint(dashboardCachedStyle).Render(dashboardSessionColumns(safeText(host.Host.Alias), id, name, state, activity, observedAge, width, hostWidth))
	}
	return text
}
func (m dashboardModel) sessionHostWidth(width int) int {
	hostWidth := 4
	for _, host := range m.hosts {
		if len(host.Sessions.Rows) == 0 {
			continue
		}
		hostWidth = max(hostWidth, ansi.StringWidth(safeText(host.Host.Alias)))
	}
	if m.usageEnabled {
		return min(hostWidth, max(4, min(20, width-18)))
	}
	// Bound long aliases while leaving room for session context on small terminals.
	return min(hostWidth, max(4, min(20, width/4)))
}
func dashboardSessionColumns(host, id, name, state, command, age string, width, hostWidth int) string {
	nameWidth, stateWidth := max(8, min(28, (width-hostWidth-26)/2)), 10
	fixed := hostWidth + 5 + nameWidth + stateWidth + 6 + 5
	return dashboardFit(host, hostWidth) + " " + dashboardFit(id, 5) + " " + dashboardFit(name, nameWidth) + " " + dashboardFit(state, stateWidth) + " " + dashboardFit(command, max(0, width-fixed)) + " " + dashboardFit(age, 6)
}
func (m dashboardModel) serviceRows(host cli.DashboardHostView, width int) []dashboardSummaryRow {
	if m.usageEnabled {
		m.serviceHostWidth = m.usageServiceHostWidth()
	}
	var rows []dashboardSummaryRow
	hostLabel := m.paint(dashboardTitleStyle).Bold(false).Render(safeText(host.Host.Alias))
	age := dashboardAge(m.now, host.Services.ObservedAt)
	for _, service := range host.Services.Rows {
		state := m.serviceState(host, service)
		name := safeText(service.Name)
		var text string
		if m.usageEnabled {
			text = m.usageServiceColumns(hostLabel, name, state, age, width)
		} else {
			text = dashboardServiceColumns(hostLabel, name, state, age, width)
		}
		rows = append(rows, dashboardSummaryRow{text: text, failed: service.Failed, priority: dashboardServicePriority(service)})
	}
	return rows
}
func (m dashboardModel) serviceState(host cli.DashboardHostView, service cli.DashboardService) string {
	mark := "●"
	if m.ascii {
		mark = "*"
	}
	state := mark + " " + safeText(service.State)
	switch {
	case service.Failed:
		state = m.paint(dashboardFailureStyle).Render(state)
	case service.HealthUnknown || service.State == "idle":
		state = m.paint(dashboardMutedStyle).Render(state)
	case service.State == "starting" || service.State == "stopping":
		state = m.paint(dashboardCachedStyle).Render(state)
	default:
		state = m.paint(dashboardGoodStyle).Render(state)
	}
	if dashboardServicesCached(host, m.now) {
		state = m.paint(dashboardCachedStyle).Render(mark + " " + service.State + " cached")
		if m.usageEnabled {
			state = m.paint(dashboardCachedStyle).Render("cached " + service.State)
		}
	}
	return state
}

func dashboardServiceColumns(host, name, state, age string, width int) string {
	return dashboardFit(host, 10) + " " + dashboardFit(name, max(0, width-10-18-6-3)) + " " + dashboardFit(state, 18) + " " + dashboardFit(age, 6)
}
func (m dashboardModel) summaries(budget int) []string {
	if m.renderWork != nil {
		m.renderWork.summaries++
	}
	if m.usageEnabled {
		return m.usageSummaries(budget)
	}
	if m.width >= 140 {
		leftWidth := (m.width - 1) * 3 / 5
		rightWidth := m.width - leftWidth - 1
		left := m.summary(false, leftWidth, budget)
		attention := m.attention(rightWidth, min(5, max(0, budget-4)))
		right := m.summary(true, rightWidth, budget-len(attention))
		right = append(right, attention...)
		var lines []string
		for i := range max(len(left), len(right)) {
			a, b := "", ""
			if i < len(left) {
				a = left[i]
			}
			if i < len(right) {
				b = right[i]
			}
			lines = append(lines, dashboardFit(a, leftWidth)+" "+dashboardFit(b, rightWidth))
		}
		return lines
	}
	if budget < 6 {
		return m.summary(false, m.width, budget)
	}
	half := budget / 2
	return append(m.summary(false, m.width, half), m.summary(true, m.width, budget-half)...)
}
func (m dashboardModel) summary(services bool, width, budget int) []string {
	if budget < 4 {
		return nil
	}
	if !services {
		return m.sessionSummary(width, budget)
	}
	if m.usageEnabled {
		m.serviceHostWidth = m.usageServiceHostWidth()
	}
	rows, total := m.summaryRows(true, width-4)
	if total == 0 {
		return nil
	}
	visible := min(len(rows), budget-3)
	headings := dashboardServiceColumns("HOST", "SERVICE", "STATE", "AGE", width-4)
	if m.usageEnabled {
		headings = m.usageServiceColumns("HOST", "SERVICE", "STATE", "AGE", width-4)
	}
	body := []string{m.paint(dashboardMutedStyle).Render(headings)}
	for _, row := range rows[:visible] {
		body = append(body, row.text)
	}
	totals := m.totals()
	label := fmt.Sprintf("Services · ready %d · failed %d", totals.ready, totals.failed)
	if totals.idle > 0 {
		label += fmt.Sprintf(" · %d idle", totals.idle)
	}
	if totals.unknown > 0 {
		label += fmt.Sprintf(" · unknown %d", totals.unknown)
	}
	count := fmt.Sprintf("%d/%d visible", visible, total)
	if m.usageEnabled {
		count = fmt.Sprintf("%d/%d", visible, total)
	}
	title := dashboardRuleTitle(m.paint(dashboardTitleStyle).Render(label), count, width-2, m.ascii, m.paint(dashboardBorderStyle))
	return m.framedPanel(title, body, width, m.paint(dashboardBorderStyle))
}
func (m dashboardModel) sessionSummary(width, budget int) []string {
	totals := m.totals()
	if totals.liveSessions+totals.cachedSessions == 0 {
		return nil
	}
	hostWidth := m.sessionHostWidth(width - 4)
	headings := dashboardSessionColumns("HOST", "ID", "SESSION", "STATE", "ACTIVITY", "AGE", width-4, hostWidth)
	if m.usageEnabled {
		headings = m.usageSessionColumns("HOST", "ID", "SESSION", "STATE", "ACTIVITY", width-4, hostWidth)
	}
	body := []string{m.paint(dashboardMutedStyle).Render(headings)}
	liveLimit := max(0, budget-4)
	selected := m.liveSelection(liveLimit)
	shown := 0
	for index, host := range m.hosts {
		for _, session := range host.Sessions.Rows[:selected[index]] {
			body = append(body, m.sessionRow(host, session, width-4, hostWidth))
			shown++
		}
	}
	cachedRows, cachedShown := m.cachedSessionRows(width-4, budget-3-len(body), hostWidth)
	body = append(body, cachedRows...)
	more := max(0, totals.liveSessions-shown)
	detail := m.moreSessionDetail(selected)
	footer := fmt.Sprintf("showing %d of %d live · %d total · %d/%d cached", shown, totals.liveSessions, totals.liveSessions+totals.cachedSessions, cachedShown, totals.cachedSessions)
	if more > 0 {
		footer += " · " + detail
	}
	body = append(body, m.paint(dashboardMutedStyle).Render(footer))
	title := fmt.Sprintf("Sessions · live %d/%d · cached %d", shown, totals.liveSessions, totals.cachedSessions)
	return m.panel(m.paint(dashboardTitleStyle).Render(title), body, width)
}
func (m dashboardModel) liveSelection(limit int) []int {
	selected := make([]int, len(m.hosts))
	for round := 0; limit > 0; round++ {
		progress := false
		for index, host := range m.hosts {
			if limit == 0 {
				return selected
			}
			if dashboardSessionsCached(host, m.now) || round >= len(host.Sessions.Rows) {
				continue
			}
			selected[index]++
			limit--
			progress = true
		}
		if !progress {
			return selected
		}
	}
	return selected
}

type dashboardAttention struct {
	width, total int
	groups       [][]string
}

func (m dashboardModel) attentionForWidth(width int) dashboardAttention {
	if m.attentionData != nil && m.attentionData.width == width {
		return *m.attentionData
	}
	var groups [][]string
	total := 0
	for _, host := range m.hosts {
		total += host.Services.Failed
		for _, service := range host.Services.Rows {
			if !service.Failed {
				continue
			}
			groups = append(groups, m.serviceAttention(host, service, width))
		}
	}
	for _, host := range m.hosts {
		if host.Connection == cli.StateReachable || host.Connection == cli.StateConnecting {
			continue
		}
		total++
		text := m.paint(dashboardFailureStyle).Render(safeText(host.Host.Alias)+" · "+string(host.Connection)) + " · last reply " + m.paint(dashboardCachedStyle).Render(dashboardAge(m.now, host.LastReply))
		groups = append(groups, strings.Split(ansi.Wrap(text, max(1, width-4), ""), "\n"))
	}
	return dashboardAttention{width: width, total: total, groups: groups}
}

func (m dashboardModel) attention(width, budget int) []string {
	data := m.attentionForWidth(width)
	var body []string
	visible := 0
	for _, group := range data.groups {
		group = m.usageAttentionGroup(group, width, max(0, budget-2-len(body)))
		if len(group) == 0 || len(body)+len(group) > max(0, budget-2) {
			continue
		}
		body = append(body, group...)
		visible++
	}
	if len(body) == 0 {
		return nil
	}
	label := fmt.Sprintf("Attention · %d · %d/%d visible", data.total, visible, data.total)
	return m.framedPanel(dashboardRuleTitle(m.paint(dashboardCachedStyle).Render(label), "", width-2, m.ascii, m.paint(dashboardFailureStyle)), body, width, m.paint(dashboardFailureStyle))
}

func (m dashboardModel) serviceAttention(host cli.DashboardHostView, service cli.DashboardService, width int) []string {
	state := service.State
	if dashboardServicesCached(host, m.now) {
		state += " cached"
	}
	problem := safeText(service.Problem)
	if problem == "" {
		problem = "reason unavailable"
	}
	if m.usageEnabled {
		identity := safeText(host.Host.Alias) + "/" + safeText(service.Name) + " · " + state
		lines := []string{m.paint(dashboardFailureStyle).Render(identity)}
		return append(lines, strings.Split(ansi.Wrap(m.paint(dashboardFailureStyle).Render(problem), max(1, width-4), ""), "\n")...)
	}
	text := safeText(host.Host.Alias) + "/" + safeText(service.Name) + " · " + state + " · " + problem
	return strings.Split(ansi.Wrap(m.paint(dashboardFailureStyle).Render(text), max(1, width-4), ""), "\n")
}

func (m dashboardModel) cachedSessionRows(width, budget, hostWidth int) ([]string, int) {
	var rows []string
	shown := 0
	for _, host := range m.hosts {
		if !dashboardSessionsCached(host, m.now) || host.Sessions.Total == 0 || budget-len(rows) < 2 {
			continue
		}
		rows = append(rows, m.paint(dashboardCachedStyle).Render("cached · "+safeText(host.Host.Alias)+" · catalog "+dashboardAge(m.now, host.Sessions.ObservedAt)+" old · not counted as live"))
		available := min(len(host.Sessions.Rows), budget-len(rows))
		for _, session := range host.Sessions.Rows[:available] {
			rows = append(rows, m.sessionRow(host, session, width, hostWidth))
			shown++
		}
	}
	return rows, shown
}
func (m dashboardModel) moreSessionDetail(selected []int) string {
	detail := ""
	for index, host := range m.hosts {
		if dashboardSessionsCached(host, m.now) || host.Sessions.Total <= selected[index] {
			continue
		}
		if detail != "" {
			detail += " · "
		}
		detail += fmt.Sprintf("%d more on %s", host.Sessions.Total-selected[index], safeText(host.Host.Alias))
	}
	return detail
}

func dashboardServicePriority(service cli.DashboardService) int {
	switch {
	case service.Failed:
		return 0
	case service.State == "idle":
		return 1
	case service.State == "starting" || service.State == "stopping":
		return 2
	case service.State == "ready" || service.State == dashboardRunning:
		return 3
	default:
		return 4
	}
}
