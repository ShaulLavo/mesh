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
	segment uint64
	value   float64
}
type dashboardHostHistory struct{ cpu, ram []dashboardPoint }

func (m *dashboardModel) remember(host cli.DashboardHostView) {
	history := m.history[host.Host.ID]
	if host.Connection == cli.StateReachable {
		observed := maxTime(m.now, host.CPU.MeasuredAt, host.RAM.MeasuredAt)
		history.cpu = dashboardRemember(history.cpu, host.CPU, observed)
		memory := cli.DashboardMeasurement[float64]{State: host.RAM.State, Value: dashboardMemoryPercent(host.RAM.Value), Sample: host.RAM.Sample, Segment: host.RAM.Segment, MeasuredAt: host.RAM.MeasuredAt, Failing: host.RAM.Failing}
		history.ram = dashboardRemember(history.ram, memory, observed)
	}
	m.history[host.Host.ID] = history
}
func (m *dashboardModel) pruneHistory() {
	for id, history := range m.history {
		history.cpu = dashboardPrune(history.cpu, m.now)
		history.ram = dashboardPrune(history.ram, m.now)
		m.history[id] = history
	}
}
func dashboardRemember(points []dashboardPoint, reading cli.DashboardMeasurement[float64], now time.Time) []dashboardPoint {
	points = dashboardPrune(points, now)
	if reading.Failing || reading.State != statusAvailable || reading.Sample == "" || reading.MeasuredAt.IsZero() || reading.MeasuredAt.After(now) || now.Sub(reading.MeasuredAt) >= 10*time.Second || math.IsNaN(reading.Value) || math.IsInf(reading.Value, 0) || reading.Value < 0 || reading.Value > 100 {
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
	points = append(points, dashboardPoint{at: reading.MeasuredAt, sample: reading.Sample, segment: reading.Segment, value: reading.Value})
	return dashboardPrune(points, now)
}
func dashboardPrune(points []dashboardPoint, now time.Time) []dashboardPoint {
	start := 0
	for start < len(points) && now.Sub(points[start].at) > 120*time.Second {
		start++
	}
	start = max(start, len(points)-64)
	return points[start:]
}
func dashboardMemoryPercent(memory cli.DashboardMemory) float64 {
	if memory.TotalBytes == 0 || memory.AvailableBytes > memory.TotalBytes {
		return math.NaN()
	}
	return 100 * float64(memory.TotalBytes-memory.AvailableBytes) / float64(memory.TotalBytes)
}
func dashboardGraph(points []dashboardPoint, now time.Time, width int, ascii bool) string {
	var line strings.Builder
	for column := range width {
		at := now.Add(-120*time.Second + time.Duration(float64(column+1)/float64(width)*float64(120*time.Second)))
		value, found := dashboardGraphValue(points, at)
		line.WriteString(dashboardGraphCell(value, found, ascii))
	}
	return line.String()
}
func dashboardGraphValue(points []dashboardPoint, at time.Time) (float64, bool) {
	for index := len(points) - 1; index >= 0; index-- {
		point := points[index]
		if point.at.After(at) {
			continue
		}
		return point.value, at.Sub(point.at) <= 3*time.Second
	}
	return 0, false
}
func dashboardGraphCell(value float64, found, ascii bool) string {
	if !found {
		return " "
	}
	if ascii {
		if value == 0 {
			return "."
		}
		return string("123456789#"[min(9, int(value/10))])
	}
	if value == 0 {
		return "·"
	}
	return string([]rune("▁▂▃▄▅▆▇█")[min(7, max(0, int(math.Ceil(value/100*8))-1))])
}

func maxTime(values ...time.Time) time.Time {
	var result time.Time
	for _, value := range values {
		if value.After(result) {
			result = value
		}
	}
	return result
}
