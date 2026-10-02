package tui

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/usagefeed"
)

const (
	usageStaleAfter = 15 * time.Minute
	usageUnknown    = "unknown"
	usageExhausted  = "exhausted"
)

func (m dashboardModel) usagePanel(width, budget int, compact bool) []string {
	if budget < 3 {
		return nil
	}
	rows := 5
	if compact {
		rows = 3
	}
	visible := min(len(m.usage.accounts), max(0, (budget-2)/rows))
	label := fmt.Sprintf("AI plans · %d accounts · passive", m.usage.total)
	if compact {
		label = fmt.Sprintf("AI plans · %d · used/left", m.usage.total)
	}
	if visible < m.usage.total {
		label = fmt.Sprintf("AI plans · %d/%d accounts · %d omitted", visible, m.usage.total, m.usage.total-visible)
	}
	if m.usageFailing {
		label = "AI plans · feed unavailable"
		if visible < m.usage.total {
			label += fmt.Sprintf(" · %d omitted", m.usage.total-visible)
		}
	}
	body := []string{}
	for _, account := range m.usage.accounts[:visible] {
		body = append(body, m.usageIdentity(account, width-4, compact))
		body = append(body, m.usageAccountWindows(account, width-4, compact)...)
	}
	if m.usage.total == 0 {
		body = append(body, "No data yet", "Waiting for feed observations")
	}
	body = body[:min(len(body), budget-2)]
	return m.panel(m.paint(dashboardTitleStyle).Render(label), body, width)
}

func (m dashboardModel) usageIdentity(account dashboardUsageAccount, width int, compact bool) string {
	prefix := "        "
	if account.first {
		prefix = m.paint(dashboardTitleStyle).Render(dashboardUsageTitle(account.Provider)) + " · "
	}
	identity := prefix + account.Label + " · " + usagePlan(account.Plan)
	badge := usageRouting(account.Account)
	if badge != "" && !compact {
		identity += " · " + badge
	}
	if account.extraWindows > 0 {
		unit := "windows"
		if account.extraWindows == 1 {
			unit = "window"
		}
		identity += fmt.Sprintf(" · +%d %s", account.extraWindows, unit)
	}
	age := usageAge(m.now, account.LastSeenAt)
	if compact {
		identity = strings.TrimLeft(identity, " ")
	}
	return dashboardAlign(identity, age, width)
}

func usageRouting(account usagefeed.Account) string {
	if account.State == "no-data" || account.LastSeenAt == nil {
		return ""
	}
	if account.State == "cooldown" {
		return "cooldown"
	}
	if account.Routing.LastServedAt != nil {
		return "last served"
	}
	if account.Routing.Mode == "rotating" {
		return "rotating"
	}
	return ""
}

func usageAge(now time.Time, seen *time.Time) string {
	if seen == nil {
		return "seen —"
	}
	prefix := "seen "
	if now.Sub(*seen) >= usageStaleAfter {
		prefix = "stale "
	}
	return prefix + dashboardAge(now, *seen)
}

func (m dashboardModel) usageAccountWindows(account dashboardUsageAccount, width int, compact bool) []string {
	var result []string
	for index := range 2 {
		window := usagefeed.Window{Label: []string{"5h", "Weekly"}[index], Status: usageUnknown}
		if index < len(account.Windows) {
			window = account.Windows[index]
		}
		showAge := window.LastSeenAt == nil || account.LastSeenAt == nil || !window.LastSeenAt.Equal(*account.LastSeenAt)
		if compact {
			result = append(result, m.usageCompactWindow(window, width, showAge))
			continue
		}
		result = append(result, m.usageWindowLines(window, width, showAge)...)
	}
	return result
}

func (m dashboardModel) usageWindowLines(window usagefeed.Window, width int, showAge bool) []string {
	if window.LastSeenAt == nil && window.UsedPercent == nil && window.Status == usageUnknown {
		return []string{dashboardFit(window.Label, 7) + "No data yet", "Waiting for normal traffic"}
	}
	used, left := "—", "—"
	if window.UsedPercent != nil {
		used, left = fmt.Sprintf("%.0f%%", *window.UsedPercent), fmt.Sprintf("%.0f%%", 100-*window.UsedPercent)
	}
	reset := "—"
	if window.ResetsAt != nil {
		reset = dashboardDuration(window.ResetsAt.Sub(m.now))
	}
	facts := fmt.Sprintf("%s%s used · %s left · resets %s", dashboardFit(window.Label, 7), used, left, reset)
	word, role := usageStatus(window)
	status := m.usageMeter(window, role) + " " + m.paint(role).Render(m.usageDot()) + " " + word
	if showAge && (window.LastSeenAt == nil || m.now.Sub(*window.LastSeenAt) >= usageStaleAfter) {
		status = dashboardAlign(status, usageAge(m.now, window.LastSeenAt), width)
	}
	if window.ResetsAt != nil && !window.ResetsAt.After(m.now) {
		// Historical values remain on the first line; the expired bar yields to its explanation.
		status = "reset passed · awaiting traffic"
	}
	return []string{facts, status}
}

func usageStatus(window usagefeed.Window) (string, dashboardStyle) {
	if window.Status == usageExhausted || window.UsedPercent != nil && *window.UsedPercent >= 100 {
		return usageExhausted, dashboardFailureStyle
	}
	if window.Status == "warning" || window.UsedPercent != nil && *window.UsedPercent >= 75 {
		return "high", dashboardCachedStyle
	}
	if window.Status == usageUnknown || window.UsedPercent == nil {
		return usageUnknown, dashboardMutedStyle
	}
	return "OK", dashboardGoodStyle
}

func (m dashboardModel) usageDot() string {
	if m.ascii {
		return "*"
	}
	return "●"
}

func (m dashboardModel) usageMeter(window usagefeed.Window, role dashboardStyle) string {
	filled := 0
	if window.UsedPercent != nil {
		filled = int(math.Round(*window.UsedPercent / 100 * 28))
	}
	marker := m.usagePace(window)
	glyph, track, pace := "▪", "▪", "│"
	if m.ascii {
		glyph, track, pace = "#", "-", "|"
	}
	marks := [3]string{m.paint(role).Render(glyph), m.paint(dashboardGridStyle).Render(track), m.paint(dashboardTextStyle).Render(pace)}
	var result strings.Builder
	for index := range 28 {
		switch {
		case index == marker:
			result.WriteString(marks[2])
		case index < filled:
			result.WriteString(marks[0])
		default:
			result.WriteString(marks[1])
		}
	}
	return result.String()
}

func (m dashboardModel) usagePace(window usagefeed.Window) int {
	word, _ := usageStatus(window)
	if window.Status == usageUnknown || word == usageExhausted || word == usageUnknown || window.LastSeenAt == nil || m.now.Sub(*window.LastSeenAt) >= usageStaleAfter || window.WindowMinutes == nil || window.ResetsAt == nil || !window.ResetsAt.After(m.now) {
		return -1
	}
	elapsed := 1 - window.ResetsAt.Sub(m.now).Minutes() / *window.WindowMinutes
	return min(27, max(0, int(min(1.0, max(0.0, elapsed))*28)))
}

func (m dashboardModel) usageCompactWindow(window usagefeed.Window, width int, showAge bool) string {
	if window.LastSeenAt == nil && window.UsedPercent == nil && window.Status == usageUnknown {
		return dashboardFit(window.Label, 7) + "No data yet"
	}
	if window.ResetsAt != nil && !window.ResetsAt.After(m.now) {
		return dashboardFit(window.Label, max(0, width-31)) + "reset passed · awaiting traffic"
	}
	used, left := "—", "—"
	if window.UsedPercent != nil {
		used, left = fmt.Sprintf("%.0f%%", *window.UsedPercent), fmt.Sprintf("%.0f%%", 100-*window.UsedPercent)
	}
	reset := "—"
	if window.ResetsAt != nil {
		reset = dashboardDuration(window.ResetsAt.Sub(m.now))
	}
	word, _ := usageStatus(window)
	if showAge {
		word += " " + usageAge(m.now, window.LastSeenAt)
	}
	return dashboardFit(fmt.Sprintf("%s %s/%s %s %s", window.Label, used, left, strings.ReplaceAll(reset, " ", ""), word), width)
}
