package tui

import (
	"math"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/cli"
)

type dashboardPoint struct {
	at      time.Time
	sample  string
	segment string
	value   float64
}

type dashboardHostHistory struct {
	cpu    []dashboardPoint
	memory []dashboardPoint
}

func (m *dashboardModel) remember(host cli.DashboardHostView) {
	history := m.history[host.Host.ID]
	if host.Reachability == cli.DashboardReachable {
		history.cpu = dashboardRemember(history.cpu, host.CPU, m.now)
		memory := cli.DashboardMeasurement[float64]{State: host.Memory.State, Value: dashboardMemoryPercent(host.Memory.Value), Sample: host.Memory.Sample, MeasuredAt: host.Memory.MeasuredAt, Stale: host.Memory.Stale}
		history.memory = dashboardRemember(history.memory, memory, m.now)
	}
	m.history[host.Host.ID] = history
}

func (m *dashboardModel) pruneHistory() {
	for id, history := range m.history {
		history.cpu = dashboardPrune(history.cpu, m.now)
		history.memory = dashboardPrune(history.memory, m.now)
		m.history[id] = history
	}
}

func dashboardRemember(points []dashboardPoint, reading cli.DashboardMeasurement[float64], now time.Time) []dashboardPoint {
	points = dashboardPrune(points, now)
	if dashboardMetricStale(reading, now, 10*time.Second) || reading.State != "available" || reading.Sample == "" || math.IsNaN(reading.Value) || math.IsInf(reading.Value, 0) {
		return points
	}
	if reading.Value < 0 || reading.Value > 100 || reading.MeasuredAt.After(now) {
		return points
	}
	for _, point := range points {
		if point.sample == reading.Sample {
			return points
		}
	}
	if len(points) > 0 && !reading.MeasuredAt.After(points[len(points)-1].at) {
		return points
	}
	points = append(points, dashboardPoint{at: reading.MeasuredAt, sample: reading.Sample, segment: dashboardSegment(reading.Sample), value: reading.Value})
	return dashboardPrune(points, now)
}

func dashboardPrune(points []dashboardPoint, now time.Time) []dashboardPoint {
	start := 0
	for start < len(points) && now.Sub(points[start].at) > 120*time.Second {
		start++
	}
	start = max(start, len(points)-64)
	return append([]dashboardPoint(nil), points[start:]...)
}

func dashboardSegment(sample string) string {
	index := strings.LastIndexByte(sample, '/')
	if index < 0 {
		return sample
	}
	return sample[:index]
}

func dashboardMemoryPercent(memory cli.DashboardMemory) float64 {
	if memory.TotalBytes == 0 || memory.AvailableBytes > memory.TotalBytes {
		return math.NaN()
	}
	return 100 * float64(memory.TotalBytes-memory.AvailableBytes) / float64(memory.TotalBytes)
}

func dashboardMetricStale[T any](reading cli.DashboardMeasurement[T], now time.Time, limit time.Duration) bool {
	return reading.Stale || reading.MeasuredAt.IsZero() || now.Sub(reading.MeasuredAt) > limit
}

func dashboardGraph(points []dashboardPoint, now time.Time, width, height int, ascii bool) []string {
	lines := make([]string, height)
	if len(points) == 0 {
		return dashboardEmptyGraph(width, height, "awaiting samples")
	}
	segment := points[len(points)-1].segment
	for row := range height {
		lines[row] = dashboardGraphRow(points, segment, now, width, height, row, ascii)
	}
	return lines
}

func dashboardGraphRow(points []dashboardPoint, segment string, now time.Time, width, height, row int, ascii bool) string {
	var line strings.Builder
	for column := range width {
		at := now.Add(-120*time.Second + time.Duration(float64(column+1)/float64(width)*float64(120*time.Second)))
		value, found := dashboardGraphValue(points, segment, at)
		line.WriteString(dashboardGraphCell(value, found, height, row, ascii))
	}
	return line.String()
}

func dashboardGraphValue(points []dashboardPoint, segment string, at time.Time) (float64, bool) {
	for index := len(points) - 1; index >= 0; index-- {
		point := points[index]
		if point.segment != segment || point.at.After(at) {
			continue
		}
		return point.value, at.Sub(point.at) <= 3*time.Second
	}
	return 0, false
}

func dashboardGraphCell(value float64, found bool, height, row int, ascii bool) string {
	if !found {
		return " "
	}
	level := value/100*float64(height) - float64(height-row-1)
	if level <= 0 {
		return " "
	}
	if ascii {
		return "#"
	}
	blocks := []rune("▁▂▃▄▅▆▇█")
	return string(blocks[min(7, int(math.Ceil(min(1, level)*8))-1)])
}

func dashboardEmptyGraph(width, height int, label string) []string {
	lines := make([]string, height)
	for row := range height {
		lines[row] = strings.Repeat(" ", width)
	}
	lines[height/2] = dashboardFit(label, width)
	return lines
}
