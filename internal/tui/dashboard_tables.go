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
	text   string
	failed bool
}

func (m dashboardModel) summaryRows(services bool, width int) ([]dashboardSummaryRow, int) {
	var rows []dashboardSummaryRow
	total := 0
	for _, host := range m.hosts {
		if services {
			total += host.Services.Total
			rows = append(rows, m.serviceRows(host, width)...)
			continue
		}
		total += host.Sessions.Total
		for _, session := range host.Sessions.Rows {
			rows = append(rows, dashboardSummaryRow{text: m.sessionRow(host, session, width)})
		}
	}
	if services {
		sort.SliceStable(rows, func(a, b int) bool { return rows[a].failed && !rows[b].failed })
	}
	return rows, total
}
func (m dashboardModel) sessionRow(host cli.DashboardHostView, session cli.DashboardSession, width int) string {
	name := safeText(session.Name)
	if name == "" {
		name = "--"
	}
	state := m.paint(dashboardMutedStyle).Render(safeText(session.State))
	if session.State == dashboardRunning {
		state = m.paint(dashboardGoodStyle).Render(session.State)
	}
	cached := dashboardSessionsCached(host, m.now)
	if cached {
		state = safeText(session.State)
	}
	text := dashboardSessionColumns(m.paint(dashboardTitleStyle).Bold(false).Render(safeText(host.Host.Alias)), session.ID, name, state, safeText(session.Command), dashboardAge(m.now, host.Sessions.ObservedAt), width)
	if cached {
		text = m.paint(dashboardCachedStyle).Render(dashboardSessionColumns(safeText(host.Host.Alias), session.ID, name, state, safeText(session.Command), dashboardAge(m.now, host.Sessions.ObservedAt), width))
	}
	return text
}
func dashboardSessionColumns(host, id, name, state, command, age string, width int) string {
	hostWidth, nameWidth, stateWidth := 10, 13, 10
	if width < 90 {
		hostWidth, nameWidth = 8, 8
	}
	fixed := hostWidth + 5 + nameWidth + stateWidth + 6 + 5
	return dashboardFit(host, hostWidth) + " " + dashboardFit(id, 5) + " " + dashboardFit(name, nameWidth) + " " + dashboardFit(state, stateWidth) + " " + dashboardFit(command, max(0, width-fixed)) + " " + dashboardFit(age, 6)
}
func (m dashboardModel) serviceRows(host cli.DashboardHostView, width int) []dashboardSummaryRow {
	var rows []dashboardSummaryRow
	for _, service := range host.Services.Rows {
		mark := "●"
		if m.ascii {
			mark = "*"
		}
		state := mark + " " + safeText(service.State)
		switch {
		case service.Failed:
			state = m.paint(dashboardFailureStyle).Render(state)
		case service.HealthUnknown:
			state = m.paint(dashboardMutedStyle).Render(state)
		default:
			state = m.paint(dashboardGoodStyle).Render(state)
		}
		if dashboardServicesCached(host, m.now) {
			state = m.paint(dashboardCachedStyle).Render(mark + " " + service.State + " cached")
		}
		text := dashboardServiceColumns(m.paint(dashboardTitleStyle).Bold(false).Render(safeText(host.Host.Alias)), safeText(service.Name), state, dashboardAge(m.now, host.Services.ObservedAt), width)
		rows = append(rows, dashboardSummaryRow{text: text, failed: service.Failed})
	}
	return rows
}
func dashboardServiceColumns(host, name, state, age string, width int) string {
	return dashboardFit(host, 10) + " " + dashboardFit(name, max(0, width-10-18-6-3)) + " " + dashboardFit(state, 18) + " " + dashboardFit(age, 6)
}
func (m dashboardModel) summaries(budget int) []string {
	if budget < 6 {
		return m.summary(false, m.width, budget)
	}
	if m.width >= 140 {
		leftWidth := (m.width - 1) * 3 / 5
		rightWidth := m.width - leftWidth - 1
		left := m.summary(false, leftWidth, budget)
		attentionBudget := min(5, max(4, budget/3))
		right := m.summary(true, rightWidth, budget-attentionBudget)
		right = append(right, m.attention(rightWidth, attentionBudget)...)
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
	rows, total := m.summaryRows(true, width-4)
	visible := min(len(rows), budget-3)
	headings := dashboardServiceColumns("HOST", "SERVICE", "STATE", "AGE", width-4)
	body := []string{m.paint(dashboardMutedStyle).Render(headings)}
	for _, row := range rows[:visible] {
		body = append(body, row.text)
	}
	totals := m.totals()
	label := fmt.Sprintf("Services · %d total · ready %d · failed %d · %d/%d visible", total, totals.ready, totals.failed, visible, total)
	if totals.unknown > 0 {
		label = fmt.Sprintf("Services · %d · ready %d · failed %d · unknown %d · %d/%d visible", total, totals.ready, totals.failed, totals.unknown, visible, total)
	}
	return m.panel(m.paint(dashboardTitleStyle).Render(label), body, width)
}
func (m dashboardModel) sessionSummary(width, budget int) []string {
	totals := m.totals()
	body := []string{m.paint(dashboardMutedStyle).Render(dashboardSessionColumns("HOST", "ID", "NAME", "STATE", "COMMAND (launch)", "AGE", width-4))}
	liveLimit := max(0, budget-4)
	selected := m.liveSelection(liveLimit)
	shown := 0
	for index, host := range m.hosts {
		for _, session := range host.Sessions.Rows[:selected[index]] {
			body = append(body, m.sessionRow(host, session, width-4))
			shown++
		}
	}
	cachedRows, cachedShown := m.cachedSessionRows(width-4, budget-3-len(body))
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
func (m dashboardModel) attention(width, budget int) []string {
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
	var body []string
	visible := 0
	for _, group := range groups {
		if len(body)+len(group) > max(0, budget-2) {
			continue
		}
		body = append(body, group...)
		visible++
	}
	label := fmt.Sprintf("Attention · %d · %d/%d visible", total, visible, total)
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
	text := safeText(host.Host.Alias) + "/" + safeText(service.Name) + " · " + state + " · " + problem
	return strings.Split(ansi.Wrap(m.paint(dashboardFailureStyle).Render(text), max(1, width-4), ""), "\n")
}

func (m dashboardModel) cachedSessionRows(width, budget int) ([]string, int) {
	var rows []string
	shown := 0
	for _, host := range m.hosts {
		if !dashboardSessionsCached(host, m.now) || host.Sessions.Total == 0 || budget-len(rows) < 2 {
			continue
		}
		rows = append(rows, m.paint(dashboardCachedStyle).Render("cached · "+safeText(host.Host.Alias)+" · catalog "+dashboardAge(m.now, host.Sessions.ObservedAt)+" old · not counted as live"))
		available := min(len(host.Sessions.Rows), budget-len(rows))
		for _, session := range host.Sessions.Rows[:available] {
			rows = append(rows, m.sessionRow(host, session, width))
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
