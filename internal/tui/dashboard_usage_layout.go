package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

func (m dashboardModel) usageSummaries(budget int) []string {
	if m.width < 140 || m.height < 40 {
		leftWidth := (m.width - 1) / 2
		left := m.usageCompactFleet(leftWidth, budget)
		right := m.usagePanel(m.width-leftWidth-1, budget, true)
		return dashboardJoinPanels(left, right, leftWidth, m.width-leftWidth-1)
	}
	// Hidden accounts cannot claim graph rows when the renderer measures summary height.
	budget = min(budget, 17)
	remaining := m.width - 56
	leftWidth := remaining * 57 / 104
	centerWidth := remaining - leftWidth
	left := m.summary(false, leftWidth, budget)
	attention := m.attention(centerWidth, min(7, max(0, budget-10)))
	center := m.summary(true, centerWidth, budget-len(attention))
	center = append(center, attention...)
	right := m.usagePanel(54, budget, false)
	fleet := dashboardJoinPanels(left, center, leftWidth, centerWidth)
	return dashboardJoinPanels(fleet, right, m.width-55, 54)
}

func (m dashboardModel) usageAttentionGroup(group []string, width, budget int) []string {
	height := m.attentionGroupHeight(len(group), budget)
	if height == len(group) {
		return group
	}
	if height == 0 {
		return nil
	}
	group = group[:height]
	group[height-1] = ansi.Truncate(group[height-1], max(0, width-5), "") + "…"
	return group
}

func dashboardJoinPanels(left, right []string, leftWidth, rightWidth int) []string {
	lines := make([]string, max(len(left), len(right)))
	for index := range lines {
		a, b := "", ""
		if index < len(left) {
			a = left[index]
		}
		if index < len(right) {
			b = right[index]
		}
		lines[index] = dashboardFit(a, leftWidth) + " " + dashboardFit(b, rightWidth)
	}
	return lines
}

func (m dashboardModel) usageSessionColumns(host, id, name, state, command string, width, hostWidth int) string {
	nameWidth := max(8, min(24, (width-hostWidth-19)/2))
	if width < 45 {
		if strings.HasPrefix(ansi.Strip(state), "cached ") {
			name = "cached " + name
		}
		return dashboardFit(host, hostWidth) + " " + dashboardFit(id, 5) + " " + dashboardFit(name+" · "+command, max(0, width-hostWidth-7))
	}
	commandWidth := max(0, width-hostWidth-5-9-nameWidth-4)
	return dashboardFit(host, hostWidth) + " " + dashboardFit(id, 5) + " " + dashboardFit(name, nameWidth) + " " + dashboardFit(state, 9) + " " + dashboardFit(command, commandWidth)
}

func (m dashboardModel) usageServiceHostWidth() int {
	if m.serviceHostWidth != 0 {
		return m.serviceHostWidth
	}
	hostWidth := 4
	for _, entry := range m.hosts {
		if m.renderWork != nil {
			m.renderWork.serviceWidthVisits++
		}
		if len(entry.Services.Rows) > 0 {
			hostWidth = max(hostWidth, ansi.StringWidth(entry.Host.Alias))
		}
	}
	return min(hostWidth, 11)
}

func (m dashboardModel) usageServiceColumns(host, name, state, age string, width int) string {
	hostWidth := m.usageServiceHostWidth()
	return dashboardFit(host, hostWidth) + " " + dashboardFit(name, max(0, width-hostWidth-20)) + " " + dashboardFit(state, 14) + " " + dashboardFit(age, 3)
}

func (m dashboardModel) usageCompactFleet(width, budget int) []string {
	if budget < 2 {
		return nil
	}
	totals := m.totals()
	attention := m.attention(width, min(7, max(0, budget-5)))
	rows := []string{m.paint(dashboardTitleStyle).Render(fmt.Sprintf("Sessions · %d live · %d cached", totals.liveSessions, totals.cachedSessions))}
	rows = append(rows, m.usageCompactSessions(width, min(2, max(0, budget-len(attention)-3)))...)
	services, total := m.summaryRows(true, width)
	visible := min(len(services), max(0, budget-len(rows)-len(attention)-1))
	label := fmt.Sprintf("Services · %d/%d · failed %d", visible, total, totals.failed)
	rows = append(rows, m.paint(dashboardTitleStyle).Render(label))
	for _, row := range services[:visible] {
		rows = append(rows, row.text)
	}
	return append(rows, attention...)
}

func (m dashboardModel) usageCompactSessions(width, limit int) []string {
	var rows []string
	hostWidth := m.sessionHostWidth(width)
	selected := m.liveSelection(limit)
	for index, host := range m.hosts {
		for _, session := range host.Sessions.Rows[:selected[index]] {
			rows = append(rows, m.sessionRow(host, session, width, hostWidth))
		}
	}
	for _, host := range m.hosts {
		if !dashboardSessionsCached(host, m.now) {
			continue
		}
		for _, session := range host.Sessions.Rows[:min(len(host.Sessions.Rows), limit-len(rows))] {
			rows = append(rows, m.sessionRow(host, session, width, hostWidth))
		}
	}
	return rows
}

func (m dashboardModel) usageAwaitingTraffic() bool {
	for _, account := range m.usage.accounts {
		for _, window := range account.Windows {
			if window.ResetsAt != nil && !window.ResetsAt.After(m.now) {
				return true
			}
		}
	}
	return false
}

func (m dashboardModel) usageFooter() string {
	cadence := "metrics 2s · catalogs change-driven · history 2m · AI bars used 0–100% · │ elapsed pace"
	if m.width < 140 || m.height < 40 {
		cadence = "metrics 2s · AI used/left · history 2m"
		if m.usageAwaitingTraffic() {
			cadence = "AI used/left · reset passed · awaiting traffic"
		}
	}
	if m.ascii {
		cadence = strings.ReplaceAll(cadence, "│", "|")
	}
	frame := "frame " + m.now.Format("15:04:05")
	for _, host := range m.hosts {
		if host.Host.Local {
			frame += " on " + safeText(host.Host.Alias)
			break
		}
	}
	return dashboardAlign(cadence, frame, m.width)
}
