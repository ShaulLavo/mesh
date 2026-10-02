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
	if m.width < 140 || m.height < 40 {
		return 4
	}
	cardRows := max(1, (len(m.hosts)+1)/2)
	// Catalog content claims its rows before graphs grow into the remaining height.
	tableHeight := len(m.summaries(m.height))
	available := m.height - 5 - tableHeight
	return max(1, (available-m.cardOverhead())/cardRows)
}

func (m dashboardModel) cardOverhead() int {
	total := 0
	for index := 0; index < len(m.hosts); index += 2 {
		total += 7
		if m.gridGPU(index) {
			total++
		}
	}
	return total
}
func (m dashboardModel) cardsHeight() int {
	height := m.cardOverhead()
	for index := 0; index < len(m.hosts); index += 2 {
		height += m.gridGraphHeight(index)
	}
	return height
}
func (m dashboardModel) gridGraphHeight(index int) int {
	height := m.graphHeight()
	if m.width < 140 || m.height < 40 {
		return height
	}
	rows := max(1, (len(m.hosts)+1)/2)
	available := max(0, m.height-5-len(m.summaries(m.height))-m.cardOverhead())
	if index/2 < available%rows {
		height++
	}
	return height
}
func (m dashboardModel) gridGPU(index int) bool {
	if dashboardHasGPU(m.hosts[index]) {
		return true
	}
	return index+1 < len(m.hosts) && dashboardHasGPU(m.hosts[index+1])
}
func (m dashboardModel) cards() []string {
	width := (m.width - 1) / 2
	var lines []string
	for index := 0; index < len(m.hosts); index += 2 {
		left := m.cardWithGPU(m.hosts[index], width, m.gridGPU(index), m.gridGraphHeight(index))
		right := make([]string, len(left))
		if index+1 < len(m.hosts) {
			right = m.cardWithGPU(m.hosts[index+1], width, m.gridGPU(index), m.gridGraphHeight(index))
		}
		for row := range left {
			lines = append(lines, dashboardFit(left[row], width)+" "+dashboardFit(right[row], width))
		}
	}
	return lines
}
func (m dashboardModel) cardWithGPU(host cli.DashboardHostView, width int, gpuRow bool, height int) []string {
	inner := width - 4
	plotWidth := (inner - 2) / 2
	live := host.Connection == cli.StateReachable
	cpuValue := m.coloredPercent(host.CPU, host, m.paint(dashboardCPUStyle))
	ramMetric := cli.DashboardMeasurement[float64]{State: host.RAM.State, Value: dashboardMemoryPercent(host.RAM.Value), Sample: host.RAM.Sample, MeasuredAt: host.RAM.MeasuredAt, Failing: host.RAM.Failing}
	ramPercent := m.coloredPercent(ramMetric, host, m.paint(dashboardRAMStyle))
	label := dashboardAlign(m.paint(dashboardMutedStyle).Render("CPU ")+m.coreStrip(host, max(0, plotWidth-8)), cpuValue, plotWidth) + "  " + dashboardAlign(m.paint(dashboardMutedStyle).Render("RAM ")+m.ramValue(host), ramPercent, inner-plotWidth-2)
	meter := dashboardSegmentedMeter(host.CPU, m.now, live, plotWidth, m.ascii, m.paint(dashboardCPUStyle)) + "  " + dashboardSegmentedMeter(ramMetric, m.now, live, inner-plotWidth-2, m.ascii, m.paint(dashboardRAMStyle))
	history := m.history[host.Host.ID]
	if host.MetricsUnsupported {
		history = dashboardHostHistory{}
	}
	left := dashboardArea(history.cpu, m.now, plotWidth, height, m.ascii, m.paint(dashboardCPUStyle))
	right := dashboardArea(history.ram, m.now, inner-plotWidth-2, height, m.ascii, m.paint(dashboardRAMStyle))
	body := []string{label, meter}
	for row := range left {
		body = append(body, left[row]+"  "+right[row])
	}
	body = append(body, m.paint(dashboardMutedStyle).Render(dashboardAlign("-2m", "now", plotWidth)+"  "+dashboardAlign("-2m", "now", inner-plotWidth-2)))
	if !live {
		body = m.offlineCard(host, inner, height)
	}
	if gpuRow {
		body = append(body, m.gpuLine(host, plotWidth, inner-plotWidth-2))
	}
	body = append(body, m.ioLine(host, plotWidth, inner-plotWidth-2))
	facts := ""
	if live {
		facts = m.cardFacts(host, inner)
	}
	body = append(body, facts)
	ages := "metrics " + dashboardAge(m.now, dashboardOldest(host.CPU.MeasuredAt, host.RAM.MeasuredAt)) + " catalogs " + dashboardAge(m.now, dashboardOldest(host.Sessions.ObservedAt, host.Services.ObservedAt))
	if host.MetricsUnsupported {
		ages = "catalogs " + dashboardAge(m.now, dashboardOldest(host.Sessions.ObservedAt, host.Services.ObservedAt))
	}
	frame := m.paint(dashboardBorderStyle)
	if !live {
		ages = "last reply " + dashboardAge(m.now, host.LastReply)
		frame = m.paint(dashboardCachedStyle)
	}
	hostTitle := m.hostTitle(host)
	if live && dashboardPresent(host.Uptime) {
		hostTitle += m.paint(dashboardMutedStyle).Render(" · up ") + m.uptime(host)
	}
	title := dashboardRuleTitle(hostTitle, m.paint(dashboardMutedStyle).Render(ages), width-2, m.ascii, frame)
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
func (m dashboardModel) offlineCard(host cli.DashboardHostView, width, height int) []string {
	body := make([]string, height+3)
	facts := []string{"last verified reply " + dashboardAge(m.now, host.LastReply) + " ago", "cached catalog " + dashboardAge(m.now, host.Sessions.ObservedAt) + " old", m.catalogCounts(host)}
	for row, text := range facts {
		body[row] = dashboardFit(m.paint(dashboardCachedStyle).Render(text), width)
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
	filled, empty := "▪", "▪"
	if ascii {
		filled, empty = "#", "-"
	}
	filled = style.Render(filled)
	empty = dashboardGraphStyle(style, dashboardGridStyle).Render(empty)
	var meter strings.Builder
	for _, cell := range raw {
		if cell == '█' || cell == '#' {
			meter.WriteString(filled)
			continue
		}
		meter.WriteString(empty)
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
	paints := [3]lipgloss.Style{style, fillStyle, dashboardGraphStyle(style, dashboardGridStyle)}
	marks := make(map[dashboardPlotMark]string)
	for column := range width {
		at := now.Add(-120*time.Second + time.Duration(float64(column+1)/float64(width)*float64(120*time.Second)))
		value, found := dashboardGraphValue(points, at)
		for row := range height {
			mark := dashboardPlotGlyph(value, found, row, height, column, ascii)
			painted, exists := marks[mark]
			if !exists {
				painted = paints[mark.paint].Render(mark.cell)
				marks[mark] = painted
			}
			_, _ = lines[row].WriteString(painted)
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

type dashboardPlotMark struct {
	cell  string
	paint int
}

func dashboardPlotGlyph(value float64, found bool, row, height, column int, ascii bool) dashboardPlotMark {
	cell := dashboardAreaCell(value, found, height-row, height, ascii)
	paint := 0
	units := int(value/100*float64(height*8) + 0.5)
	low, high := (height-row-1)*8, (height-row)*8
	edge := found && units > low && units <= high
	if edge && !ascii {
		cell = dashboardEdgeCell(units - low)
	}
	if !edge && (cell == "█" || cell == "#") {
		paint = 1
	}
	if cell == " " && (row == 0 || row == height/2) && column%2 == 0 {
		cell = "·"
		if ascii {
			cell = "."
		}
		paint = 2
	}
	return dashboardPlotMark{cell: cell, paint: paint}
}
