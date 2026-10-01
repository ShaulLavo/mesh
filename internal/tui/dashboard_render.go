package tui

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/shaul/mesh/internal/cli"
)

var dashboardColors = struct{ cpu, ram, live, error, stale, border, muted lipgloss.Style }{
	cpu:    lipgloss.NewStyle().Foreground(lipgloss.Color("#b59790")),
	ram:    lipgloss.NewStyle().Foreground(lipgloss.Color("#a5a0b6")),
	live:   lipgloss.NewStyle().Foreground(lipgloss.Color("#87a9b0")),
	error:  lipgloss.NewStyle().Foreground(lipgloss.Color("#c38b7b")),
	stale:  lipgloss.NewStyle().Foreground(lipgloss.Color("#c4d8e2")),
	border: lipgloss.NewStyle().Foreground(lipgloss.Color("#584e51")),
	muted:  lipgloss.NewStyle().Foreground(lipgloss.Color("#cfd3cd")),
}

func dashboardFit(value string, width int) string {
	width = max(0, width)
	value = ansi.Truncate(value, width, "…")
	return value + strings.Repeat(" ", max(0, width-ansi.StringWidth(value)))
}

func dashboardText(value string, width int) string { return dashboardFit(safeText(value), width) }

func (m dashboardModel) render() string {
	if m.width < 80 || m.height < 24 {
		return dashboardFit(fmt.Sprintf("Mesh fleet needs 80x24; current %dx%d", m.width, m.height), m.width)
	}
	lines := m.header()
	cardRows := (len(m.hosts) + 1) / 2
	if m.width >= 140 && m.height >= 40 && cardRows*13 <= m.height-11 {
		lines = append(lines, m.cards()...)
	} else {
		lines = append(lines, m.fleetTable(m.height-len(lines)-10)...)
	}
	lines = append(lines, m.summaries(m.height-len(lines)-1)...)
	lines = append(lines, dashboardColors.muted.Render("CPU / RAM 0–100%  |  history 2m  |  metrics 2s · catalogs 10s  |  "+m.now.Format("15:04:05")))
	for index := range lines {
		lines[index] = dashboardFit(lines[index], m.width)
	}
	return strings.Join(lines[:min(len(lines), m.height)], "\n")
}

func (m dashboardModel) header() []string {
	reachable, unreachable, refused := 0, 0, 0
	for _, host := range m.hosts {
		switch host.Reachability {
		case cli.DashboardReachable:
			reachable++
		case cli.DashboardUnreachable:
			unreachable++
		case cli.DashboardRefused:
			refused++
		}
	}
	liveSessions, cachedSessions, pendingSessions := m.catalogCounts(true)
	liveServices, cachedServices, pendingServices := m.catalogCounts(false)
	return []string{
		dashboardColors.cpu.Bold(true).Render("MESH fleet") + fmt.Sprintf("  %d hosts · %d reachable · %d unreachable · %d refused", len(m.hosts), reachable, unreachable, refused),
		fmt.Sprintf("sessions %d live / %d cached · %d hosts pending   services %d live / %d cached · %d hosts pending", liveSessions, cachedSessions, pendingSessions, liveServices, cachedServices, pendingServices),
	}
}

func (m dashboardModel) catalogCounts(sessions bool) (live, cached, pending int) {
	for _, host := range m.hosts {
		observed, stale, count := host.ServicesObservedAt, host.ServicesStale, len(host.Services)
		if sessions {
			observed, stale, count = host.SessionsObservedAt, host.SessionsStale, len(host.Sessions)
		}
		if observed.IsZero() {
			pending++
			continue
		}
		if m.catalogFresh(host, observed, stale) {
			live += count
			continue
		}
		cached += count
	}
	return
}

func (m dashboardModel) catalogFresh(host cli.DashboardHostView, observed time.Time, stale bool) bool {
	return host.Reachability == cli.DashboardReachable && !stale && !observed.IsZero() && m.now.Sub(observed) <= 30*time.Second
}

func (m dashboardModel) cards() []string {
	width := (m.width - 1) / 2
	var lines []string
	for index := 0; index < len(m.hosts); index += 2 {
		left := m.card(m.hosts[index], width)
		var right []string
		if index+1 < len(m.hosts) {
			right = m.card(m.hosts[index+1], width)
		}
		lines = append(lines, dashboardJoin(left, right, width)...)
	}
	return lines
}

func dashboardJoin(left, right []string, width int) []string {
	lines := make([]string, max(len(left), len(right)))
	for index := range lines {
		var a, b string
		if index < len(left) {
			a = left[index]
		}
		if index < len(right) {
			b = right[index]
		}
		lines[index] = dashboardFit(a, width) + " " + dashboardFit(b, width)
	}
	return lines
}

func (m dashboardModel) card(host cli.DashboardHostView, width int) []string {
	inner := width - 4
	half := (inner - 3) / 2
	live := host.Reachability == cli.DashboardReachable
	cpu := m.percent(host.CPU, live)
	memory := m.memory(host.Memory, live)
	lines := []string{dashboardFit("CPU all cores "+cpu, half) + "   " + "RAM " + memory}
	lines = append(lines, dashboardColors.cpu.Render(m.meter(host.CPU, live, half))+"   "+dashboardColors.ram.Render(m.memoryMeter(host.Memory, live, half)))
	history := m.history[host.Host.ID]
	cpuGraph := dashboardGraph(history.cpu, m.now, half, 4, m.ascii)
	ramGraph := dashboardGraph(history.memory, m.now, half, 4, m.ascii)
	if !live {
		cpuGraph = dashboardEmptyGraph(half, 4, "last good data is stale")
		ramGraph = dashboardEmptyGraph(half, 4, "no current observation")
	}
	for row := range 4 {
		lines = append(lines, dashboardColors.cpu.Render(cpuGraph[row])+"   "+dashboardColors.ram.Render(ramGraph[row]))
	}
	axis := dashboardFit("-2m", max(0, half-3)) + "now"
	lines = append(lines, dashboardColors.muted.Render(axis+"   "+axis))
	lines = append(lines, m.temperature(host.Temperature, live)+"   uptime "+m.uptime(host.Uptime, live))
	lines = append(lines, m.catalogLabel(host, true)+"   "+m.catalogLabel(host, false))
	lines = append(lines, dashboardColors.muted.Render("age CPU "+dashboardAge(m.now, host.CPU.MeasuredAt)+" · RAM "+dashboardAge(m.now, host.Memory.MeasuredAt)+" · temp "+dashboardAge(m.now, host.Temperature.MeasuredAt)))
	lines = append(lines, m.hostProblem(host))
	title := safeText(host.Host.Alias) + " · " + string(host.Reachability)
	if host.Host.Local {
		title += " · this host"
	}
	return m.panel(title, lines, width)
}

func (m dashboardModel) panel(title string, lines []string, width int) []string {
	cornerLeft, cornerRight, vertical, horizontal, bottomLeft, bottomRight := "┌", "┐", "│", "─", "└", "┘"
	if m.ascii {
		cornerLeft, cornerRight, vertical, horizontal, bottomLeft, bottomRight = "+", "+", "|", "-", "+", "+"
	}
	title = ansi.Truncate(" "+title+" ", width-2, "…")
	top := cornerLeft + title + strings.Repeat(horizontal, max(0, width-2-ansi.StringWidth(title))) + cornerRight
	result := []string{dashboardColors.border.Render(top)}
	for _, line := range lines {
		result = append(result, dashboardColors.border.Render(vertical)+" "+dashboardFit(line, width-4)+" "+dashboardColors.border.Render(vertical))
	}
	return append(result, dashboardColors.border.Render(bottomLeft+strings.Repeat(horizontal, width-2)+bottomRight))
}

func (m dashboardModel) percent(reading cli.DashboardMeasurement[float64], live bool) string {
	if reading.Sample == "" || math.IsNaN(reading.Value) || math.IsInf(reading.Value, 0) {
		return dashboardUnavailable(reading.State)
	}
	value := fmt.Sprintf("%.0f%%", reading.Value)
	if !live || dashboardMetricStale(reading, m.now, 10*time.Second) || reading.State != "available" {
		return dashboardColors.stale.Render(value + " stale " + dashboardAge(m.now, reading.MeasuredAt))
	}
	return dashboardColors.cpu.Render(value)
}

func dashboardUnavailable(state string) string {
	if state == "unsupported" {
		return "unsupported"
	}
	return "unavailable"
}

func (m dashboardModel) memory(reading cli.DashboardMeasurement[cli.DashboardMemory], live bool) string {
	if reading.Sample == "" || math.IsNaN(dashboardMemoryPercent(reading.Value)) {
		return dashboardUnavailable(reading.State)
	}
	value := fmt.Sprintf("%.1f/%.1f GiB %.0f%%", float64(reading.Value.TotalBytes-reading.Value.AvailableBytes)/(1<<30), float64(reading.Value.TotalBytes)/(1<<30), dashboardMemoryPercent(reading.Value))
	if !live || dashboardMetricStale(reading, m.now, 10*time.Second) || reading.State != "available" {
		return dashboardColors.stale.Render(value + " stale " + dashboardAge(m.now, reading.MeasuredAt))
	}
	return dashboardColors.ram.Render(value)
}

func (m dashboardModel) meter(reading cli.DashboardMeasurement[float64], live bool, width int) string {
	if !live || reading.State != "available" || dashboardMetricStale(reading, m.now, 10*time.Second) || math.IsNaN(reading.Value) {
		return strings.Repeat("·", width)
	}
	filled := min(width, max(0, int(reading.Value/100*float64(width))))
	block, empty := "▰", "▱"
	if m.ascii {
		block, empty = "#", "."
	}
	return strings.Repeat(block, filled) + strings.Repeat(empty, width-filled)
}

func (m dashboardModel) memoryMeter(reading cli.DashboardMeasurement[cli.DashboardMemory], live bool, width int) string {
	metric := cli.DashboardMeasurement[float64]{State: reading.State, Sample: reading.Sample, MeasuredAt: reading.MeasuredAt, Value: dashboardMemoryPercent(reading.Value), Stale: reading.Stale}
	return m.meter(metric, live, width)
}

func (m dashboardModel) temperature(reading cli.DashboardMeasurement[cli.DashboardTemperature], live bool) string {
	if reading.Sample == "" {
		return "temp " + dashboardUnavailable(reading.State)
	}
	value := fmt.Sprintf("%s %.0f°C", dashboardText(reading.Value.Sensor, 18), reading.Value.Celsius)
	if !live || dashboardMetricStale(reading, m.now, 30*time.Second) || reading.State != "available" {
		return dashboardColors.stale.Render(value + " stale")
	}
	return value
}

func (m dashboardModel) uptime(reading cli.DashboardMeasurement[uint64], live bool) string {
	if reading.Sample == "" {
		return dashboardUnavailable(reading.State)
	}
	value := (time.Duration(reading.Value) * time.Second).Round(time.Minute).String()
	if !live || dashboardMetricStale(reading, m.now, 10*time.Second) || reading.State != "available" {
		return value + " stale"
	}
	return value
}

func dashboardAge(now, measured time.Time) string {
	if measured.IsZero() {
		return "never"
	}
	return age(now, measured)
}

func (m dashboardModel) catalogLabel(host cli.DashboardHostView, sessions bool) string {
	label, count, observed, stale := "services", len(host.Services), host.ServicesObservedAt, host.ServicesStale
	if sessions {
		label, count, observed, stale = "sessions", len(host.Sessions), host.SessionsObservedAt, host.SessionsStale
	}
	if observed.IsZero() {
		return label + " pending"
	}
	state := "live"
	if !m.catalogFresh(host, observed, stale) {
		state = "cached"
	}
	return fmt.Sprintf("%s %d %s %s", label, count, state, dashboardAge(m.now, observed))
}

func (m dashboardModel) hostProblem(host cli.DashboardHostView) string {
	if host.Problem != "" {
		return dashboardColors.error.Render(safeText(host.Problem))
	}
	problems := []string{host.CPU.Problem, host.Memory.Problem, host.Temperature.Problem, host.SessionsProblem, host.ServicesProblem}
	for _, problem := range problems {
		if problem != "" {
			return dashboardColors.stale.Render(safeText(problem))
		}
	}
	return ""
}

func (m dashboardModel) fleetTable(budget int) []string {
	lines := []string{dashboardColors.muted.Render("HOST          DAEMON          CPU         RAM             TEMP    SESS / SVC")}
	visible := min(len(m.hosts), max(0, budget-2))
	for _, host := range m.hosts[:visible] {
		live := host.Reachability == cli.DashboardReachable
		row := dashboardText(host.Host.Alias, 13) + " " + dashboardFit(string(host.Reachability), 15) + " " + dashboardFit(m.percent(host.CPU, live), 11) + " " + dashboardFit(m.memory(host.Memory, live), 16)
		row += " " + dashboardFit(m.temperature(host.Temperature, live), 7) + " " + m.compactCounts(host)
		lines = append(lines, row)
	}
	lines = append(lines, dashboardColors.muted.Render(fmt.Sprintf("hosts %d / %d visible · %d omitted", visible, len(m.hosts), len(m.hosts)-visible)))
	return lines
}

func (m dashboardModel) compactCounts(host cli.DashboardHostView) string {
	sessions, services := "?", "?"
	if !host.SessionsObservedAt.IsZero() {
		sessions = fmt.Sprint(len(host.Sessions))
	}
	if !host.ServicesObservedAt.IsZero() {
		services = fmt.Sprint(len(host.Services))
	}
	state := "live"
	if !m.catalogFresh(host, host.SessionsObservedAt, host.SessionsStale) || !m.catalogFresh(host, host.ServicesObservedAt, host.ServicesStale) {
		state = "cached"
	}
	return sessions + "/" + services + " " + state
}

type dashboardSummaryRow struct {
	text   string
	failed bool
}

func (m dashboardModel) summaryRows(services bool, width int) []dashboardSummaryRow {
	var rows []dashboardSummaryRow
	for _, host := range m.hosts {
		rows = append(rows, m.hostSummaryRows(host, services, width)...)
	}
	if services {
		sort.SliceStable(rows, func(a, b int) bool { return rows[a].failed && !rows[b].failed })
	}
	return rows
}

func (m dashboardModel) hostSummaryRows(host cli.DashboardHostView, services bool, width int) []dashboardSummaryRow {
	var rows []dashboardSummaryRow
	if services {
		for _, service := range host.Services {
			rows = append(rows, m.serviceRow(host, service, width))
		}
		return rows
	}
	for _, session := range host.Sessions {
		rows = append(rows, m.sessionRow(host, session, width))
	}
	return rows
}

func (m dashboardModel) serviceRow(host cli.DashboardHostView, service cli.DashboardService, width int) dashboardSummaryRow {
	status := service.State
	if !m.catalogFresh(host, host.ServicesObservedAt, host.ServicesStale) {
		status += " cached"
	}
	failed := service.Problem != "" || service.State == "failed" || service.State == "unhealthy"
	text := dashboardText(host.Host.Alias, 10) + " " + dashboardText(service.Name, 18) + " " + dashboardText(status, 17) + " " + dashboardText(service.Problem, width-48)
	if failed {
		text = dashboardColors.error.Render(text)
	}
	return dashboardSummaryRow{text: text, failed: failed}
}

func (m dashboardModel) sessionRow(host cli.DashboardHostView, session cli.DashboardSession, width int) dashboardSummaryRow {
	state := session.State
	if !m.catalogFresh(host, host.SessionsObservedAt, host.SessionsStale) {
		state += " cached"
	}
	command := session.Command
	if session.Name != "" {
		command = session.Name + " · " + command
	}
	if session.InspectionProblem != "" {
		command += " · inspect unavailable"
	}
	text := dashboardText(host.Host.Alias, 10) + " " + dashboardText(session.ID, 8) + " " + dashboardText(state, 18) + " " + dashboardText(command, width-40)
	return dashboardSummaryRow{text: text}
}

func (m dashboardModel) summaries(budget int) []string {
	if budget < 5 {
		return nil
	}
	if m.width >= 140 {
		width := (m.width - 1) / 2
		left := m.summaryPanel(false, width, budget)
		right := m.summaryPanel(true, width, budget)
		return dashboardJoin(left, right, width)
	}
	height := max(4, budget/2)
	return append(m.summaryPanel(false, m.width, height), m.summaryPanel(true, m.width, budget-height)...)
}

func (m dashboardModel) summaryPanel(services bool, width, height int) []string {
	rows := m.summaryRows(services, width-4)
	visible := min(len(rows), max(0, height-3))
	title := fmt.Sprintf("SESSIONS %d / %d rows", visible, len(rows))
	if services {
		title = fmt.Sprintf("SERVICES %d / %d rows", visible, len(rows))
	}
	lines := make([]string, 0, visible+1)
	for _, row := range rows[:visible] {
		lines = append(lines, row.text)
	}
	if len(rows) == 0 {
		lines = append(lines, "No observed rows yet")
	}
	lines = append(lines, dashboardColors.muted.Render(fmt.Sprintf("%d omitted · catalog ages shown with each host", len(rows)-visible)))
	return m.panel(title, lines, width)
}
