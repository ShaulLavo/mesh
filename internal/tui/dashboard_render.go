package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/shaul/mesh/internal/cli"
)

var (
	dashboardGPUStyle     = dashboardColorStyle(dashboardTheme.gpu)
	dashboardCPUStyle     = dashboardColorStyle(dashboardTheme.cpu)
	dashboardCPUFillStyle = dashboardColorStyle(dashboardTheme.cpuFill)
	dashboardRAMFillStyle = dashboardColorStyle(dashboardTheme.ramFill)
	dashboardGridStyle    = dashboardColorStyle(dashboardTheme.grid)
	dashboardRAMStyle     = dashboardColorStyle(dashboardTheme.ram)
	dashboardMutedStyle   = dashboardColorStyle(dashboardTheme.muted)
	dashboardBorderStyle  = dashboardColorStyle(dashboardTheme.border)
	dashboardGoodStyle    = dashboardColorStyle(dashboardTheme.good)
	dashboardCachedStyle  = dashboardColorStyle(dashboardTheme.cached)
	dashboardFailureStyle = dashboardColorStyle(dashboardTheme.failure)
	dashboardTitleStyle   = dashboardColorStyle(dashboardTheme.title).Bold(true)
)

func dashboardFit(value string, width int) string {
	width = max(0, width)
	value = ansi.Truncate(value, width, "…")
	return value + strings.Repeat(" ", max(0, width-ansi.StringWidth(value)))
}
func dashboardAlign(left, right string, width int) string {
	right = ansi.Truncate(right, width, "…")
	return dashboardFit(left, max(0, width-ansi.StringWidth(right))) + right
}
func (m dashboardModel) render() string {
	if m.width < 80 || m.height < 24 {
		return ansi.Truncate(fmt.Sprintf("Mesh fleet needs 80×24; current %d×%d", m.width, m.height), m.width, "…")
	}
	lines := m.header()
	visible := len(m.hosts)
	switch {
	case m.width >= 140 && m.height >= 40 && m.cardsHeight()+len(m.summaries(m.height)) <= m.height-5:
		lines = append(lines, m.cards()...)
	case len(m.hosts) > 6:
		visible = min(len(m.hosts), m.height-11)
		lines = append(lines, m.paint(dashboardMutedStyle).Render("HOST        DAEMON       CPU        RAM GiB       SESSIONS / SERVICES"))
		for _, host := range m.hosts[:visible] {
			lines = append(lines, m.fleetRow(host))
		}
	default:
		visible = min(len(m.hosts), max(0, (m.height-11)/2))
		for _, host := range m.hosts[:visible] {
			lines = append(lines, m.compactHost(host)...)
		}
	}
	lines = append(lines, m.paint(dashboardMutedStyle).Render(fmt.Sprintf("Hosts %d / %d visible · %d omitted", visible, len(m.hosts), len(m.hosts)-visible)))
	lines = append(lines, m.summaries(m.height-len(lines)-1)...)
	for len(lines) < m.height-1 {
		lines = append(lines, "")
	}
	lines = append(lines, m.paint(dashboardMutedStyle).Render(m.footer()))
	for index, line := range lines {
		lines[index] = dashboardFit(line, m.width)
	}
	return m.paint(dashboardColorStyle(dashboardTheme.text)).Render(strings.Join(lines, "\n"))
}
func (m dashboardModel) header() []string {
	totals := m.totals()
	title := m.paint(dashboardTitleStyle).Render("MESH") + " fleet · reachable " + m.paint(dashboardGoodStyle).Render(fmt.Sprint(totals.reachable)) + " · unreachable " + m.paint(dashboardFailureStyle).Render(fmt.Sprint(totals.unreachable))
	counts := "sessions " + fmt.Sprint(totals.liveSessions) + " live " + m.paint(dashboardCachedStyle).Render(fmt.Sprint(totals.cachedSessions)) + " cached · services " + fmt.Sprint(totals.services) + " · ready " + m.paint(dashboardGoodStyle).Render(fmt.Sprint(totals.ready)) + " · failed " + m.paint(dashboardFailureStyle).Render(fmt.Sprint(totals.failed))
	if totals.idle > 0 {
		counts += " · " + m.paint(dashboardMutedStyle).Render(fmt.Sprintf("%d idle", totals.idle))
	}
	if totals.unknown > 0 {
		counts += " · unknown " + m.paint(dashboardMutedStyle).Render(fmt.Sprint(totals.unknown))
	}
	if totals.cachedServices > 0 {
		counts += fmt.Sprintf(" · cached %d", totals.cachedServices)
	}
	if totals.connecting > 0 {
		title += fmt.Sprintf(" · connecting %d", totals.connecting)
	}
	if totals.refused > 0 {
		title += " · " + m.paint(dashboardFailureStyle).Render(fmt.Sprintf("refused %d", totals.refused))
	}
	clock := m.now.Format("2006-01-02 15:04:05")
	if m.width >= 140 {
		h := "─"
		if m.ascii {
			h = "-"
		}
		return m.framedPanel(m.paint(dashboardBorderStyle).Render(strings.Repeat(h, m.width-2)), []string{dashboardAlign(title+" · "+counts, clock, m.width-4)}, m.width, m.paint(dashboardBorderStyle))
	}
	return []string{dashboardAlign(title, m.now.Format("15:04:05"), m.width), counts}
}

type dashboardTotals struct{ reachable, unreachable, connecting, refused, liveSessions, cachedSessions, services, ready, failed, idle, unknown, cachedServices int }

func (m dashboardModel) totals() dashboardTotals {
	var totals dashboardTotals
	for _, host := range m.hosts {
		switch host.Connection {
		case cli.StateReachable:
			totals.reachable++
		case cli.StateUnreachable:
			totals.unreachable++
		case cli.StateRefused:
			totals.refused++
		case cli.StateConnecting:
			totals.connecting++
		default:
			totals.connecting++
		}
		if dashboardSessionsCached(host, m.now) {
			totals.cachedSessions += host.Sessions.Total
		} else {
			totals.liveSessions += host.Sessions.Total
		}
		totals.services += host.Services.Total
		if dashboardServicesCached(host, m.now) {
			totals.cachedServices += host.Services.Total
			continue
		}
		totals.ready += host.Services.Ready
		totals.failed += host.Services.Failed
		totals.idle += host.Services.Idle
		totals.unknown += host.Services.Unknown
	}
	return totals
}
func dashboardSessionsCached(host cli.DashboardHostView, now time.Time) bool {
	return host.Connection != cli.StateReachable || host.Sessions.Stale(now, host.LastReply)
}
func dashboardServicesCached(host cli.DashboardHostView, now time.Time) bool {
	return host.Connection != cli.StateReachable || host.Services.Stale(now, host.LastReply)
}
func (m dashboardModel) fleetRow(host cli.DashboardHostView) string {
	cpu := m.metricValue(host.CPU, host, fmt.Sprintf("%.0f%%", host.CPU.Value))
	memory := m.ramValue(host)
	if strings.Contains(ansi.Strip(memory), "stale") {
		memory = m.paint(dashboardCachedStyle).Render("stale " + dashboardAge(m.now, host.RAM.MeasuredAt))
	} else {
		memory = strings.ReplaceAll(strings.TrimSuffix(memory, " GiB"), " / ", "/")
	}
	counts := m.catalogCounts(host)
	state := string(host.Connection)
	if host.MetricsUnsupported {
		return dashboardFit(safeText(host.Host.Alias), 11) + " " + dashboardMetricsUpgrade + " · " + counts
	}
	if host.Connection == cli.StateUnreachable || host.Connection == cli.StateRefused {
		state = m.paint(dashboardFailureStyle).Render(state)
	}
	return dashboardFit(safeText(host.Host.Alias), 11) + " " + dashboardFit(state, 12) + " " + dashboardFit(cpu, 10) + " " + dashboardFit(memory, 14) + " " + counts
}
func (m dashboardModel) hostTitle(host cli.DashboardHostView) string {
	mark := "●"
	if m.ascii {
		mark = "*"
	}
	state := m.paint(dashboardGoodStyle).Render(mark + " " + string(host.Connection))
	if host.Connection != cli.StateReachable {
		state = m.paint(dashboardCachedStyle).Render(mark + " " + string(host.Connection))
	}
	title := m.paint(dashboardTitleStyle).Render(safeText(host.Host.Alias)) + " · " + state
	if host.MetricsUnsupported {
		title += " · " + dashboardMetricsUpgrade
	}
	return title
}
func (m dashboardModel) metricValue(metric cli.DashboardMeasurement[float64], host cli.DashboardHostView, value string) string {
	if host.MetricsUnsupported {
		return "--"
	}
	if metric.State == dashboardUnsupported {
		return dashboardUnsupported
	}
	if host.Connection != cli.StateReachable {
		return "--"
	}
	reading := dashboardReading(metric, host, m.now, 10*time.Second, value)
	if strings.Contains(reading, " stale ") {
		return m.paint(dashboardCachedStyle).Render(reading)
	}
	return reading
}
func (m dashboardModel) ramValue(host cli.DashboardHostView) string {
	if host.MetricsUnsupported {
		return "--/-- GiB"
	}
	ram := host.RAM.Value
	value := ""
	if ram.TotalBytes > 0 && ram.AvailableBytes <= ram.TotalBytes {
		value = fmt.Sprintf("%.1f / %.1f GiB", float64(ram.TotalBytes-ram.AvailableBytes)/(1<<30), float64(ram.TotalBytes)/(1<<30))
	}
	if host.RAM.State == dashboardUnsupported {
		return dashboardUnsupported
	}
	if host.Connection != cli.StateReachable {
		return "--/-- GiB"
	}
	reading := dashboardReading(host.RAM, host, m.now, 10*time.Second, value)
	if strings.Contains(reading, " stale ") {
		return m.paint(dashboardCachedStyle).Render(reading)
	}
	return reading
}

func (m dashboardModel) catalogCounts(host cli.DashboardHostView) string {
	sessions, services := host.Sessions, host.Services
	sessions.Failing = dashboardSessionsCached(host, m.now)
	services.Failing = dashboardServicesCached(host, m.now)
	return dashboardCatalogCount("sessions", sessions, host.LastReply, m.now) + " · " + strings.TrimSuffix(dashboardCatalogCount("services", services, host.LastReply, m.now), " live")
}

func (m dashboardModel) footer() string {
	cadence := "metrics 2s · catalogs change-driven · history 2m (120s)"
	if m.width >= 140 {
		cadence += " · 0–100% · AGE catalog age"
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
