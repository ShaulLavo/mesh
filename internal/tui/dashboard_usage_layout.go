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
	attention := m.attention(centerWidth, min(4, max(0, budget-11)))
	center := m.summary(true, centerWidth, budget-len(attention))
	center = append(center, attention...)
	right := m.usagePanel(54, budget, false)
	fleet := dashboardJoinPanels(left, center, leftWidth, centerWidth)
	return dashboardJoinPanels(fleet, right, m.width-55, 54)
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
	nameWidth := 8
	if width < 45 {
		return dashboardFit(host, hostWidth) + " " + dashboardFit(id, 5) + " " + dashboardFit(state, max(0, width-hostWidth-7))
	}
	commandWidth := max(0, width-hostWidth-5-9-nameWidth-4)
	return dashboardFit(host, hostWidth) + " " + dashboardFit(id, 5) + " " + dashboardFit(name, nameWidth) + " " + dashboardFit(state, 9) + " " + dashboardFit(command, commandWidth)
}

func (m dashboardModel) usageServiceColumns(host, name, state, age string, width int) string {
	hostWidth := 4
	for _, entry := range m.hosts {
		if len(entry.Services.Rows) > 0 {
			hostWidth = max(hostWidth, ansi.StringWidth(entry.Host.Alias))
		}
	}
	hostWidth = min(hostWidth, 11)
	return dashboardFit(host, hostWidth) + " " + dashboardFit(name, max(0, width-hostWidth-17)) + " " + dashboardFit(state, 11) + " " + dashboardFit(age, 3)
}

func (m dashboardModel) usageCompactFleet(width, budget int) []string {
	if budget < 2 {
		return nil
	}
	totals := m.totals()
	attention := m.attention(width, min(4, max(0, budget-4)))
	rows := []string{m.paint(dashboardTitleStyle).Render(fmt.Sprintf("Sessions · %d live · %d cached", totals.liveSessions, totals.cachedSessions))}
	selected := m.liveSelection(min(2, max(0, budget-len(attention)-3)))
	for index, host := range m.hosts {
		for _, session := range host.Sessions.Rows[:selected[index]] {
			rows = append(rows, m.sessionRow(host, session, width, m.sessionHostWidth(width)))
		}
	}
	services, total := m.summaryRows(true, width)
	visible := min(len(services), max(0, budget-len(rows)-len(attention)-1))
	label := fmt.Sprintf("Services · %d/%d · failed %d", visible, total, totals.failed)
	rows = append(rows, m.paint(dashboardTitleStyle).Render(label))
	for _, row := range services[:visible] {
		rows = append(rows, row.text)
	}
	return append(rows, attention...)
}

func (m dashboardModel) usageFooter() string {
	cadence := "metrics 2s · catalogs change-driven · history 2m"
	if m.width >= 140 {
		cadence += " · AI bars used 0–100% · │ elapsed pace"
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
