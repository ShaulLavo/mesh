package tui

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/shaul/mesh/internal/usagefeed"
)

const (
	usageStaleAfter = 15 * time.Minute
	usageUnknown    = "unknown"
	usageExhausted  = "exhausted"
	usageCooldown   = "cooldown"
	usageDisabled   = "disabled"
	usageWaiting    = "Waiting"
	usageRotating   = "rotating"
	usageReading    = "reading"
)

func (m dashboardModel) usagePanel(width, budget int, compact bool) []string {
	if budget < 3 {
		return nil
	}
	windowCompact := m.usagePanelCompact(budget, compact)
	visible, _ := m.usageVisible(budget, windowCompact)
	body := []string{}
	for _, account := range m.usage.accounts[:visible] {
		body = append(body, m.usageIdentity(account, width-4, compact))
		body = append(body, m.usageAccountWindows(account, width-4, windowCompact)...)
	}

	label := fmt.Sprintf("AI plans · %d accounts · passive", m.usage.total)
	if compact {
		label = fmt.Sprintf("AI plans · %d · used/left", m.usage.total)
	}
	if windowCompact && !compact {
		label = fmt.Sprintf("AI plans · %d accounts · used/left", m.usage.total)
	}
	if visible < m.usage.total {
		label = fmt.Sprintf("AI plans · %d/%d accounts · %d omitted", visible, m.usage.total, m.usage.total-visible)
		if compact {
			label = fmt.Sprintf("AI plans · %d/%d · %d omitted", visible, m.usage.total, m.usage.total-visible)
		}
	}
	if m.usageFailing {
		label = "AI plans · feed unavailable"
		if visible < m.usage.total {
			label += fmt.Sprintf(" · %d omitted", m.usage.total-visible)
			if compact {
				label = fmt.Sprintf("AI plans · unavailable · %d omitted", m.usage.total-visible)
			}
		}
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
	if badge != "" && badge != usageWaiting && !compact {
		identity += " · " + badge
	}
	if account.extraWindows > 0 {
		unit := "windows"
		if account.extraWindows == 1 {
			unit = "window"
		}
		identity += fmt.Sprintf(" · +%d %s", account.extraWindows, unit)
	}
	age := "checked —"
	if account.CheckedAt != nil {
		age = "checked " + dashboardAge(m.now, *account.CheckedAt)
	}
	if compact {
		identity = strings.TrimLeft(identity, " ")
	}
	if badge == usageWaiting {
		// Reserve parking and age together so compact identities retain both facts.
		age = badge + " · " + age
	}
	if compact && ansi.StringWidth(identity+" "+age) > width {
		age = ""
		if badge == usageWaiting {
			age = badge
		}
	}
	return dashboardAlign(identity, " "+age, width)
}

func usageRouting(account usagefeed.Account) string {
	if account.State == usageDisabled && account.Routing.Mode == usageRotating && account.Routing.Active != nil && !*account.Routing.Active && account.Cooldown == nil {
		return usageWaiting
	}
	if account.State == usageDisabled || account.State == usageCooldown {
		return account.State
	}
	if account.State == "no-data" || account.LastSeenAt == nil {
		return ""
	}
	if account.Routing.LastServedAt != nil {
		return "last served"
	}
	if account.Routing.Mode == usageRotating {
		return usageRotating
	}
	return ""
}

func usageAge(now time.Time, seen *time.Time) string {
	if seen == nil {
		return "read —"
	}
	prefix := "read "
	if now.Sub(*seen) >= usageStaleAfter {
		prefix = "stale "
	}
	return prefix + dashboardAge(now, *seen)
}

func (m dashboardModel) usageAccountWindows(account dashboardUsageAccount, width int, compact bool) []string {
	var result []string
	for _, window := range account.Windows {
		if !usageWindowHasReading(window) {
			continue
		}
		showAge := true
		if compact {
			result = append(result, m.usageCompactWindow(window, width, showAge))
			continue
		}
		result = append(result, m.usageWindowLines(window, width, showAge)...)
	}
	credits := usageCredits(account.Credits)
	if len(result) == 0 {
		return []string{usageEmptySummary(account.Account, credits, width)}
	}
	if credits != "" {
		result = append([]string{credits}, result...)
	}

	return result
}

func usageEmptySummary(account usagefeed.Account, credits string, width int) string {
	badge := usageRouting(account)
	summary := "No quota reading · waits for traffic"
	if badge == usageDisabled {
		summary = "Out of rotation · no quota reading"
	}
	if badge == usageWaiting {
		summary = usageWaiting + " · no quota reading"
	}
	if credits == "" {
		return summary
	}
	if ansi.StringWidth(summary+" · "+credits) <= width {
		return summary + " · " + credits
	}
	summary = "Awaiting traffic"
	if badge == usageDisabled {
		summary = "Out of rotation"
	}
	if badge == usageWaiting {
		summary = "no quota reading"
	}
	return summary + " · " + credits
}

func (m dashboardModel) usageWindowLines(window usagefeed.Window, width int, showAge bool) []string {
	if !usageWindowHasReading(window) {
		waiting := "Waiting for normal traffic"
		if showAge {
			waiting = dashboardAlign(waiting, " "+m.usageWindowAge(window), width)
		}
		return []string{dashboardFit(window.Label, 7) + " No data yet", waiting}
	}
	used, left := "—", "—"
	if window.UsedPercent != nil {
		used, left = fmt.Sprintf("%.0f%%", *window.UsedPercent), fmt.Sprintf("%.0f%%", 100-*window.UsedPercent)
	}
	reset := "—"
	if window.ResetsAt != nil {
		reset = dashboardDuration(window.ResetsAt.Sub(m.now))
	}
	facts := fmt.Sprintf("%s %s used · %s left · resets %s", dashboardFit(window.Label, 7), used, left, reset)
	facts = ansi.Truncate(facts, width, "…")
	word, role := m.usageWindowStatus(window, false)
	meter := ansi.Truncate(m.usageMeter(window, role), max(0, width-ansi.StringWidth(word)-3), "")
	if showAge {
		// Keep the status word and observation age when an unknown age needs extra cells.
		meter = ansi.Truncate(meter, max(0, width-ansi.StringWidth(m.usageWindowAge(window))-ansi.StringWidth(word)-4), "")
	}
	status := meter + " " + m.paint(role).Render(m.usageDot()) + " " + word
	if window.ResetsAt != nil && !window.ResetsAt.After(m.now) {
		// Historical values remain on the first line; the expired bar yields to its explanation.
		status = "reset passed · awaiting traffic"
		if word == usageExhausted {
			status = "exhausted · " + status
		}
	}
	if showAge {
		status = dashboardAlign(status, " "+m.usageWindowAge(window), width)
	}
	return []string{facts, status}
}

func (m dashboardModel) usageWindowStatus(window usagefeed.Window, compact bool) (string, dashboardStyle) {
	word, role := usageStatus(window)
	if word != usageUnknown && (word != "OK" || !m.usageReadingHistoric(window)) {
		return word, role
	}
	if window.UsedPercent == nil {
		if compact {
			return "no quota", role
		}
		return "quota unavailable", role
	}
	if compact {
		return usageReading, role
	}
	return "last reading", role
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
	if usageHistoricSource(window.Source) || window.Status == usageUnknown || word == usageExhausted || word == usageUnknown || window.LastSeenAt == nil || m.now.Sub(*window.LastSeenAt) >= usageStaleAfter || window.WindowMinutes == nil || window.ResetsAt == nil || !window.ResetsAt.After(m.now) {
		return -1
	}
	elapsed := 1 - window.ResetsAt.Sub(m.now).Minutes() / *window.WindowMinutes
	return min(27, max(0, int(min(1.0, max(0.0, elapsed))*28)))
}

func (m dashboardModel) usageCompactWindow(window usagefeed.Window, width int, showAge bool) string {
	if !usageWindowHasReading(window) {
		text := dashboardFit(window.Label, 7) + " No data yet"
		if showAge {
			text = dashboardAlign(text, " "+m.usageWindowAge(window), width)
		}
		return text
	}
	used, left := "—", "—"
	if window.UsedPercent != nil {
		used, left = fmt.Sprintf("%.0f%%", *window.UsedPercent), fmt.Sprintf("%.0f%%", 100-*window.UsedPercent)
	}
	reset := "—"
	if window.ResetsAt != nil {
		reset = dashboardDuration(window.ResetsAt.Sub(m.now))
	}
	word, _ := m.usageWindowStatus(window, true)
	compactReset := strings.ReplaceAll(reset, " ", "")
	ratio := used + "/" + left
	facts := ratio + " resets " + compactReset + " " + word
	if window.ResetsAt != nil && !window.ResetsAt.After(m.now) {
		facts = used + "/" + left + " resets " + reset + " passed"
		if word == usageExhausted {
			facts = used + "/" + left + " resets " + reset + " used up"
		}
	}
	if showAge {
		facts = usageCompactHistory(window.Label, facts, ratio, compactReset, word, m.usageWindowAge(window), width)
	}
	if word == usageExhausted && ansi.StringWidth(window.Label+" "+facts) > width {
		facts = strings.ReplaceAll(facts, usageExhausted, "used up")
	}
	// Reserve the standard window label even when unusually long facts need truncation.
	label := ansi.Truncate(window.Label, max(7, width-ansi.StringWidth(facts)-1), "…")
	return dashboardFit(strings.TrimSpace(label+" "+facts), width)
}

func usageCompactHistory(label, facts, ratio, reset, word, age string, width int) string {
	if age == "stale · age unknown" {
		age = "stale ?"
	}
	facts += " " + age
	if ansi.StringWidth(label+" "+facts) <= width {
		return facts
	}
	if reading := usageCompactReadingHistory(label, facts, ratio, reset, word, age, width); reading != "" {
		return reading
	}
	if strings.HasSuffix(facts, " passed "+age) || word == "OK" || word == usageReading {
		facts = ratio + " resets " + reset + " " + age
	} else {
		facts = strings.ReplaceAll(word, usageExhausted, "used up") + " resets " + reset + " " + age
	}
	if ansi.StringWidth(label+" "+facts) <= width {
		return facts
	}
	compactAge := strings.ReplaceAll(strings.ReplaceAll(age, "d ", "d"), "h ", "h")
	facts = strings.TrimSuffix(facts, age) + compactAge
	if ansi.StringWidth(label+" "+facts) <= width {
		return facts
	}
	if word == usageReading {
		facts = ratio + " reset " + reset + " " + compactAge
		if ansi.StringWidth(label+" "+facts) <= width {
			return facts
		}
		return ratio + " reset " + reset + " " + strings.Replace(compactAge, "stale ", "old ", 1)
	}
	if word == "no quota" {
		return word + " reset " + reset + " " + strings.Replace(compactAge, "stale ", "old ", 1)
	}
	status := strings.NewReplacer(usageExhausted, "spent", usageUnknown, "?").Replace(word)
	return status + " resets " + reset + " " + compactAge
}

func usageCompactReadingHistory(label, facts, ratio, reset, word, age string, width int) string {
	if !(word == usageExhausted && strings.HasPrefix(ratio, "100%/") || word == "high" && !strings.HasPrefix(ratio, "—")) {
		return ""
	}
	resetLabel := "reset " + reset
	if strings.Contains(facts, " used up ") {
		resetLabel = "reset passed"
	}
	used, _, _ := strings.Cut(ratio, "/")
	if word == usageExhausted {
		word = "used"
	}
	return usageCompactQuotaHistory(label, used+" "+word, resetLabel, age, width)
}

func usageCompactQuotaHistory(label, reading, resetLabel, age string, width int) string {
	prefix := reading + " " + resetLabel + " "
	facts := prefix + age
	if ansi.StringWidth(label+" "+facts) <= width {
		return facts
	}
	age = strings.ReplaceAll(strings.ReplaceAll(age, "d ", "d"), "h ", "h")
	age = strings.Replace(age, "stale ", "old ", 1)
	facts = prefix + age
	if ansi.StringWidth(label+" "+facts) <= width {
		return facts
	}
	return prefix + strings.Replace(age, "old ", "old", 1)
}

func usageWindowHasReading(window usagefeed.Window) bool {
	return window.LastSeenAt != nil || window.UsedPercent != nil || window.ResetsAt != nil || window.Status != usageUnknown && window.Status != ""
}

func usageHistoricSource(source string) bool {
	return source != "proxy-state" && source != "passive-header"
}

func (m dashboardModel) usageReadingHistoric(window usagefeed.Window) bool {
	return usageHistoricSource(window.Source) || window.LastSeenAt == nil || m.now.Sub(*window.LastSeenAt) >= usageStaleAfter
}

func (m dashboardModel) usageWindowAge(window usagefeed.Window) string {
	if usageHistoricSource(window.Source) {
		if window.LastSeenAt == nil {
			return "stale · age unknown"
		}
		return "stale " + dashboardAge(m.now, *window.LastSeenAt)
	}
	return usageAge(m.now, window.LastSeenAt)
}

func usageCredits(credits *usagefeed.Credits) string {
	if credits == nil {
		return ""
	}
	if credits.Unlimited {
		return "Credits unlimited"
	}
	if credits.Balance > 0 && credits.Balance < 0.01 {
		return "Credits <0.01"
	}
	value := strconv.FormatFloat(credits.Balance, 'f', 2, 64)
	whole, fraction, _ := strings.Cut(value, ".")
	for position := len(whole) - 3; position > 0; position -= 3 {
		whole = whole[:position] + "," + whole[position:]
	}
	if fraction != "00" {
		whole += "." + fraction
	}
	return "Credits " + whole
}

func (m dashboardModel) usagePanelCompact(budget int, compact bool) bool {
	if compact {
		return true
	}
	expanded, _ := m.usageVisible(budget, false)
	compressed, _ := m.usageVisible(budget, true)
	return expanded < m.usage.total && compressed == m.usage.total
}

func (m dashboardModel) usageVisible(budget int, compact bool) (int, int) {
	height, visible := 2, 0
	for _, account := range m.usage.accounts {
		rows := usageAccountRows(account, compact)
		if height+rows > budget {
			break
		}
		height += rows
		visible++
	}
	if m.usage.total == 0 {
		height = min(budget, 4)
	}
	return visible, height
}

func usageAccountRows(account dashboardUsageAccount, compact bool) int {
	readings := 0
	for _, window := range account.Windows {
		if !usageWindowHasReading(window) {
			continue
		}
		readings++
	}
	if readings == 0 {
		return 2
	}
	windowRows := 2
	if compact {
		windowRows = 1
	}
	rows := 1 + windowRows*readings
	if usageCredits(account.Credits) != "" {
		rows++
	}
	return rows
}
