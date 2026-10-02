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

var dashboardCPUStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#269A49"))
var dashboardRAMStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#0099D4"))

func dashboardFit(value string, width int) string {
	width = max(0, width)
	value = ansi.Truncate(value, width, "…")
	return value + strings.Repeat(" ", max(0, width-ansi.StringWidth(value)))
}
func (m dashboardModel) render() string {
	if m.width < 80 || m.height < 24 {
		return ansi.Truncate(fmt.Sprintf("Mesh fleet needs 80×24; current %d×%d", m.width, m.height), m.width, "…")
	}
	lines := []string{"Mesh fleet · live machine measurements", "CPU / RAM 0–100% · history 120s · change-driven catalogs"}
	visible := len(m.hosts)
	switch {
	case m.width >= 140 && m.height >= 40 && ((len(m.hosts)+1)/2)*12 <= m.height-9:
		lines = append(lines, m.cards()...)
	case len(m.hosts) <= 3:
		for _, host := range m.hosts {
			lines = append(lines, m.hostLines(host)...)
		}
	default:
		visible = min(len(m.hosts), m.height-11)
		lines = append(lines, "HOST        DAEMON       CPU        RAM GiB       SESSIONS / SERVICES")
		for _, host := range m.hosts[:visible] {
			lines = append(lines, m.fleetRow(host))
		}
	}
	lines = append(lines, fmt.Sprintf("Hosts %d / %d visible · %d omitted", visible, len(m.hosts), len(m.hosts)-visible))
	lines = append(lines, m.summaries(m.height-len(lines))...)
	for index, line := range lines {
		lines[index] = ansi.Truncate(line, m.width, "…")
	}
	return strings.Join(lines, "\n")
}
func (m dashboardModel) cards() []string {
	width := (m.width - 1) / 2
	var lines []string
	for index := 0; index < len(m.hosts); index += 2 {
		left := m.card(m.hosts[index], width)
		right := make([]string, len(left))
		if index+1 < len(m.hosts) {
			right = m.card(m.hosts[index+1], width)
		}
		for row := range left {
			lines = append(lines, dashboardFit(left[row], width)+" "+dashboardFit(right[row], width))
		}
	}
	return lines
}
func (m dashboardModel) card(host cli.DashboardHostView, width int) []string {
	inner := width - 4
	details := m.hostLines(host)
	history := m.history[host.Host.ID]
	live := host.Connection == cli.StateReachable
	cpu := dashboardMeter(host.CPU, m.now, live, inner-5, m.ascii)
	ramMetric := cli.DashboardMeasurement[float64]{State: host.RAM.State, Value: dashboardMemoryPercent(host.RAM.Value), Sample: host.RAM.Sample, MeasuredAt: host.RAM.MeasuredAt, Failing: host.RAM.Failing}
	ram := dashboardMeter(ramMetric, m.now, live, inner-5, m.ascii)
	body := []string{details[1], dashboardCPUStyle.Render("CPU  " + cpu), dashboardRAMStyle.Render("RAM  " + ram), dashboardCPUStyle.Render("CPU  " + dashboardGraph(history.cpu, m.now, inner-5, m.ascii)), dashboardRAMStyle.Render("RAM  " + dashboardGraph(history.ram, m.now, inner-5, m.ascii)), "     -120s" + strings.Repeat(" ", max(0, inner-13)) + "now", m.temperature(host) + " · uptime " + m.uptime(host), details[2], "age CPU " + dashboardAge(m.now, host.CPU.MeasuredAt) + " · RAM " + dashboardAge(m.now, host.RAM.MeasuredAt) + " · temp " + dashboardAge(m.now, host.Temperature.MeasuredAt), details[3]}
	horizontal, vertical, corner := "─", "│", "┌"
	if m.ascii {
		horizontal, vertical, corner = "-", "|", "+"
	}
	lines := []string{corner + dashboardFit(details[0], width-2) + corner}
	for _, line := range body {
		lines = append(lines, vertical+" "+dashboardFit(line, inner)+" "+vertical)
	}
	return append(lines, corner+strings.Repeat(horizontal, width-2)+corner)
}
func (m dashboardModel) fleetRow(host cli.DashboardHostView) string {
	cpu := dashboardReading(host.CPU, host, m.now, 10*time.Second, fmt.Sprintf("%.0f%%", host.CPU.Value))
	ram := host.RAM.Value
	memory := dashboardReading(host.RAM, host, m.now, 10*time.Second, fmt.Sprintf("%.1f/%.1f", float64(ram.TotalBytes-ram.AvailableBytes)/(1<<30), float64(ram.TotalBytes)/(1<<30)))
	counts := dashboardCatalogCount("sessions", host.Sessions, host.LastReply, m.now) + " · " + dashboardCatalogCount("services", host.Services, host.LastReply, m.now)
	return dashboardFit(safeText(host.Host.Alias), 11) + " " + dashboardFit(string(host.Connection), 12) + " " + dashboardFit(cpu, 10) + " " + dashboardFit(memory, 14) + " " + counts
}
func (m dashboardModel) hostLines(host cli.DashboardHostView) []string {
	title := safeText(host.Host.Alias) + " · " + string(host.Connection)
	if host.Host.Local {
		title += " · this host"
	}
	counts := dashboardCatalogCount("sessions", host.Sessions, host.LastReply, m.now) + " · " + dashboardCatalogCount("services", host.Services, host.LastReply, m.now)
	measurements := "CPU " + dashboardReading(host.CPU, host, m.now, 10*time.Second, fmt.Sprintf("%.0f%%", host.CPU.Value))
	ram := host.RAM.Value
	value := ""
	if ram.TotalBytes > 0 && ram.AvailableBytes <= ram.TotalBytes {
		value = fmt.Sprintf("%.1f/%.1f GiB", float64(ram.TotalBytes-ram.AvailableBytes)/(1<<30), float64(ram.TotalBytes)/(1<<30))
	}
	measurements += " · RAM " + dashboardReading(host.RAM, host, m.now, 10*time.Second, value)
	detail := safeText(ram.Estimate)
	for _, problem := range []string{host.Problem, host.CPU.Problem, host.RAM.Problem, host.Temperature.Problem} {
		if problem != "" {
			detail = safeText(problem)
			break
		}
	}
	return []string{title, measurements, counts, detail}
}
func dashboardReading[T any](metric cli.DashboardMeasurement[T], host cli.DashboardHostView, now time.Time, limit time.Duration, value string) string {
	if metric.Sample == "" || metric.MeasuredAt.IsZero() {
		if metric.State == "unsupported" {
			return "unsupported"
		}
		return statusUnavailable
	}
	if metric.Failing || metric.State != statusAvailable || now.Sub(metric.MeasuredAt) >= limit || host.Connection != cli.StateReachable || host.LastReply.IsZero() || now.Sub(host.LastReply) >= 30*time.Second {
		return value + " stale " + dashboardAge(now, metric.MeasuredAt)
	}
	return value
}
func dashboardCatalogCount[T any](label string, catalog cli.DashboardCatalog[T], lastReply, now time.Time) string {
	if catalog.ObservedAt.IsZero() && catalog.Total == 0 {
		return label + " pending"
	}
	state := statusLive
	if catalog.Stale(now, lastReply) {
		state = "cached"
	}
	return fmt.Sprintf("%s %d %s", label, catalog.Total, state)
}
func dashboardMeter(metric cli.DashboardMeasurement[float64], now time.Time, live bool, width int, ascii bool) string {
	if !live || metric.State != statusAvailable || metric.Failing || metric.Sample == "" || metric.MeasuredAt.IsZero() || now.Sub(metric.MeasuredAt) >= 10*time.Second || math.IsNaN(metric.Value) || math.IsInf(metric.Value, 0) {
		return strings.Repeat(".", width)
	}
	filled := min(width, max(0, int(metric.Value/100*float64(width))))
	block, empty := "█", "·"
	if ascii {
		block, empty = "#", "."
	}
	return strings.Repeat(block, filled) + strings.Repeat(empty, width-filled)
}
func (m dashboardModel) temperature(host cli.DashboardHostView) string {
	metric := host.Temperature
	value := fmt.Sprintf("%s %.0f°C", safeText(metric.Value.Sensor), metric.Value.Celsius)
	return "temp " + dashboardReading(metric, host, m.now, 30*time.Second, value)
}
func (m dashboardModel) uptime(host cli.DashboardHostView) string {
	metric := host.Uptime
	value := fmt.Sprintf("%dh%dm", metric.Value/3600, (metric.Value%3600)/60)
	return dashboardReading(metric, host, m.now, 30*time.Second, value)
}
func dashboardAge(now, measured time.Time) string {
	if measured.IsZero() {
		return "never"
	}
	return max(time.Duration(0), now.Sub(measured)).Round(time.Second).String()
}

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
			state := session.State
			if host.Sessions.Stale(m.now, host.LastReply) {
				state += " cached"
			}
			text := dashboardFit(safeText(host.Host.Alias), 10) + " " + dashboardFit(session.ID, 6) + " " + dashboardFit(state, 15) + " " + safeText(session.Command)
			rows = append(rows, dashboardSummaryRow{text: dashboardFit(text, width)})
		}
	}
	if services {
		sort.SliceStable(rows, func(a, b int) bool { return rows[a].failed && !rows[b].failed })
	}
	return rows, total
}
func (m dashboardModel) serviceRows(host cli.DashboardHostView, width int) []dashboardSummaryRow {
	var rows []dashboardSummaryRow
	for _, service := range host.Services.Rows {
		state := service.State
		if host.Services.Stale(m.now, host.LastReply) {
			state += " cached"
		}
		text := dashboardFit(safeText(host.Host.Alias), 10) + " " + dashboardFit(safeText(service.Name), 14) + " " + dashboardFit(state, 15) + " " + safeText(service.Problem)
		rows = append(rows, dashboardSummaryRow{text: dashboardFit(text, width), failed: service.Failed})
	}
	return rows
}
func (m dashboardModel) summaries(budget int) []string {
	if budget < 2 {
		return nil
	}
	if m.width >= 140 {
		width := (m.width - 1) / 2
		left := m.summary(false, width, budget)
		right := m.summary(true, width, budget)
		lines := make([]string, max(len(left), len(right)))
		for i := range lines {
			a, b := "", ""
			if i < len(left) {
				a = left[i]
			}
			if i < len(right) {
				b = right[i]
			}
			lines[i] = dashboardFit(a, width) + " " + dashboardFit(b, width)
		}
		return lines
	}
	half := budget / 2
	return append(m.summary(false, m.width, half), m.summary(true, m.width, budget-half)...)
}
func (m dashboardModel) summary(services bool, width, budget int) []string {
	rows, total := m.summaryRows(services, width)
	visible := min(len(rows), max(0, budget-1))
	label := "Sessions"
	if services {
		label = "Services"
	}
	lines := []string{fmt.Sprintf("%s · %d visible / %d total · %d omitted", label, visible, total, max(0, total-visible))}
	for _, row := range rows[:visible] {
		lines = append(lines, row.text)
	}
	return lines
}
