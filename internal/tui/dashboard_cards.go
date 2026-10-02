package tui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/shaul/mesh/internal/cli"
)

func (m dashboardModel) graphHeight() int {
	minimum := 4
	if len(m.hosts) > 4 {
		minimum = 3
	}
	if m.width < 140 || m.height < 40 {
		return minimum
	}
	cardRows := max(1, (len(m.hosts)+1)/2)
	tableHeight := 4
	for _, host := range m.hosts {
		tableHeight += len(host.Sessions.Rows)
		if dashboardSessionsCached(host, m.now) && len(host.Sessions.Rows) > 0 {
			tableHeight++
		}
	}
	available := m.height - 5 - tableHeight
	return min(12, max(minimum, available/cardRows-6))
}

func (m dashboardModel) cardHeight() int { return m.graphHeight() + 6 }
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
	plotWidth := (inner - 2) / 2
	live := host.Connection == cli.StateReachable
	cpuValue := m.coloredPercent(host.CPU, host, m.paint(dashboardCPUStyle))
	ramMetric := cli.DashboardMeasurement[float64]{State: host.RAM.State, Value: dashboardMemoryPercent(host.RAM.Value), Sample: host.RAM.Sample, MeasuredAt: host.RAM.MeasuredAt, Failing: host.RAM.Failing}
	ramPercent := m.coloredPercent(ramMetric, host, m.paint(dashboardRAMStyle))
	label := dashboardAlign(m.paint(dashboardMutedStyle).Render("CPU all cores"), cpuValue, plotWidth) + "  " + dashboardAlign(m.paint(dashboardMutedStyle).Render("RAM ")+m.ramValue(host), ramPercent, inner-plotWidth-2)
	meter := dashboardSegmentedMeter(host.CPU, m.now, live, plotWidth, m.ascii, m.paint(dashboardCPUStyle)) + "  " + dashboardSegmentedMeter(ramMetric, m.now, live, inner-plotWidth-2, m.ascii, m.paint(dashboardRAMStyle))
	history := m.history[host.Host.ID]
	left := dashboardArea(history.cpu, m.now, plotWidth, m.graphHeight(), m.ascii, m.paint(dashboardCPUStyle))
	right := dashboardArea(history.ram, m.now, inner-plotWidth-2, m.graphHeight(), m.ascii, m.paint(dashboardRAMStyle))
	body := []string{label, meter}
	for row := range left {
		body = append(body, left[row]+"  "+right[row])
	}
	body = append(body, m.paint(dashboardMutedStyle).Render(dashboardAlign("-2m", "now", plotWidth)+"  "+dashboardAlign("-2m", "now", inner-plotWidth-2)))
	if !live {
		body = m.offlineCard(host, label, meter, inner)
	}
	facts := m.temperature(host) + m.paint(dashboardMutedStyle).Render(" · uptime ") + m.uptime(host)
	if !live {
		facts = m.paint(dashboardMutedStyle).Render("temp -- · uptime --")
	}
	facts += " · " + m.catalogCounts(host)
	if host.Services.Failed > 0 {
		facts += " · " + m.paint(dashboardFailureStyle).Render(fmt.Sprintf("%d failed", host.Services.Failed))
	}
	body = append(body, facts)
	ages := "metrics " + dashboardAge(m.now, dashboardOldest(host.CPU.MeasuredAt, host.RAM.MeasuredAt)) + " catalogs " + dashboardAge(m.now, dashboardOldest(host.Sessions.ObservedAt, host.Services.ObservedAt))
	frame := m.paint(dashboardBorderStyle)
	if !live {
		ages = "last reply " + dashboardAge(m.now, host.LastReply)
		frame = m.paint(dashboardFailureStyle)
	}
	title := dashboardRuleTitle(m.hostTitle(host), m.paint(dashboardMutedStyle).Render(ages), width-2, m.ascii, frame)
	return m.framedPanel(title, body, width, frame)
}
func (m dashboardModel) coloredPercent(metric cli.DashboardMeasurement[float64], host cli.DashboardHostView, style lipgloss.Style) string {
	value := fmt.Sprintf("%.0f%%", metric.Value)
	reading := m.metricValue(metric, host, value)
	if reading == value {
		return style.Bold(true).Render(value)
	}
	return reading
}
func dashboardOldest(a, b time.Time) time.Time {
	if a.IsZero() || b.IsZero() {
		return time.Time{}
	}
	if a.Before(b) {
		return a
	}
	return b
}
func (m dashboardModel) offlineCard(host cli.DashboardHostView, label, meter string, width int) []string {
	body := []string{label, meter}
	facts := []string{"last verified reply " + dashboardAge(m.now, host.LastReply) + " ago", "cached catalog " + dashboardAge(m.now, host.Sessions.ObservedAt) + " old", "fresh metrics --"}
	for row := range m.graphHeight() + 1 {
		text := ""
		if row < len(facts) {
			text = m.paint(dashboardCachedStyle).Render(facts[row])
		}
		body = append(body, dashboardFit(text, width))
	}
	return body
}
func dashboardRuleTitle(left, right string, width int, ascii bool, frame lipgloss.Style) string {
	h := "─"
	if ascii {
		h = "-"
	}
	available := max(0, width-ansi.StringWidth(right)-2)
	left = ansi.Truncate(left, available, "…")
	gap := max(0, width-ansi.StringWidth(left)-ansi.StringWidth(right)-2)
	return left + frame.Render(" "+strings.Repeat(h, gap)+" ") + right
}
func (m dashboardModel) panel(title string, body []string, width int) []string {
	return m.framedPanel(dashboardRuleTitle(title, "", width-2, m.ascii, m.paint(dashboardBorderStyle)), body, width, m.paint(dashboardBorderStyle))
}
func (m dashboardModel) framedPanel(title string, body []string, width int, frame lipgloss.Style) []string {
	h, v, tl, tr, bl, br := "─", "│", "┌", "┐", "└", "┘"
	if m.ascii {
		h, v, tl, tr, bl, br = "-", "|", "+", "+", "+", "+"
	}
	lines := []string{frame.Render(tl) + dashboardFit(title, width-2) + frame.Render(tr)}
	for _, line := range body {
		lines = append(lines, frame.Render(v)+" "+dashboardFit(line, width-4)+" "+frame.Render(v))
	}
	return append(lines, frame.Render(bl+strings.Repeat(h, width-2)+br))
}
func dashboardSegmentedMeter(metric cli.DashboardMeasurement[float64], now time.Time, live bool, width int, ascii bool, style lipgloss.Style) string {
	raw := dashboardMeter(metric, now, live, width, ascii)
	var meter strings.Builder
	for _, cell := range raw {
		mark := "▪"
		if ascii {
			mark = "-"
		}
		paint := dashboardGraphStyle(style, dashboardGridStyle)
		if cell == '█' || cell == '#' {
			paint = style
			if ascii {
				mark = "#"
			}
		}
		meter.WriteString(paint.Render(mark))
	}
	return meter.String()
}
func dashboardArea(points []dashboardPoint, now time.Time, width, height int, ascii bool, style lipgloss.Style) []string {
	lines := make([]strings.Builder, height)
	fillStyle := dashboardCPUFillStyle
	if style.GetForeground() == dashboardRAMStyle.GetForeground() || style.GetForeground() == lipgloss.Cyan {
		fillStyle = dashboardRAMFillStyle
	}
	fillStyle = dashboardGraphStyle(style, fillStyle)
	for column := range width {
		at := now.Add(-120*time.Second + time.Duration(float64(column+1)/float64(width)*float64(120*time.Second)))
		value, found := dashboardGraphValue(points, at)
		for row := range height {
			_, _ = lines[row].WriteString(dashboardPlotCell(value, found, row, height, column, ascii, style, fillStyle))
		}
	}
	result := make([]string, height)
	for row := range lines {
		result[row] = lines[row].String()
	}
	return result
}
func dashboardAreaCell(value float64, found bool, level, height int, ascii bool) string {
	if !found {
		return " "
	}
	if value == 0 {
		if level == 1 {
			return "."
		}
		return " "
	}
	units := int(value/100*float64(height*8) + 0.5)
	fill := min(8, max(0, units-(level-1)*8))
	if fill == 0 {
		return " "
	}
	if ascii {
		return "#"
	}
	return string([]rune(" ▁▂▃▄▅▆▇█")[fill])
}

func dashboardEdgeCell(fill int) string {
	switch {
	case fill <= 2:
		return "⣀"
	case fill <= 4:
		return "⠤"
	case fill <= 6:
		return "⠒"
	default:
		return "⠉"
	}
}

func dashboardPlotCell(value float64, found bool, row, height, column int, ascii bool, style, fillStyle lipgloss.Style) string {
	cell := dashboardAreaCell(value, found, height-row, height, ascii)
	paint := style
	units := int(value/100*float64(height*8) + 0.5)
	low, high := (height-row-1)*8, (height-row)*8
	edge := found && units > low && units <= high
	if edge && !ascii {
		cell = dashboardEdgeCell(units - low)
	}
	if !edge && (cell == "█" || cell == "#") {
		paint = fillStyle
	}
	if cell == " " && (row == 0 || row == height/2) && column%2 == 0 {
		cell = "·"
		if ascii {
			cell = "."
		}
		paint = dashboardGraphStyle(style, dashboardGridStyle)
	}
	return paint.Render(cell)
}
