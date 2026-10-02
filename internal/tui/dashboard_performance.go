package tui

import (
	"fmt"
	"math"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/hostmetrics"
)

func dashboardPresent[T any](metric cli.DashboardMeasurement[T]) bool {
	return metric.State == statusAvailable && metric.Sample != "" && !metric.MeasuredAt.IsZero()
}
func dashboardOptionalPresent[T any](metric *cli.DashboardMeasurement[T]) bool {
	return metric != nil && dashboardPresent(*metric)
}
func dashboardHasGPU(host cli.DashboardHostView) bool {
	return dashboardOptionalPresent(host.GPU) || dashboardOptionalPresent(host.Battery)
}
func dashboardPercent[T any](metric cli.DashboardMeasurement[T], value float64) cli.DashboardMeasurement[float64] {
	return cli.DashboardMeasurement[float64]{State: metric.State, Value: value, Sample: metric.Sample, MeasuredAt: metric.MeasuredAt, Failing: metric.Failing}
}
func dashboardFresh[T any](metric cli.DashboardMeasurement[T], host cli.DashboardHostView, now time.Time, limit time.Duration) bool {
	return dashboardPresent(metric) && !metric.Failing && host.Connection == cli.StateReachable && !host.LastReply.IsZero() && now.Sub(host.LastReply) < 30*time.Second && now.Sub(metric.MeasuredAt) < limit
}
func dashboardPerformanceReading[T any](m dashboardModel, metric cli.DashboardMeasurement[T], host cli.DashboardHostView, limit time.Duration, value string) string {
	reading := dashboardReading(metric, host, m.now, limit, value)
	if reading != value {
		return m.paint(dashboardCachedStyle).Render(reading)
	}
	return value
}
func (m dashboardModel) performanceMeter(metric cli.DashboardMeasurement[float64], host cli.DashboardHostView, width int, style lipgloss.Style) string {
	if dashboardFresh(metric, host, m.now, 30*time.Second) {
		metric.MeasuredAt = m.now
	}
	return dashboardSegmentedMeter(metric, m.now, host.Connection == cli.StateReachable, max(0, width), m.ascii, m.paint(style))
}
func (m dashboardModel) coreStrip(host cli.DashboardHostView, width int) string {
	if !dashboardOptionalPresent(host.Cores) || !dashboardFresh(*host.Cores, host, m.now, 10*time.Second) {
		return ""
	}
	cores := host.Cores.Value
	width = min(28, width)
	if width <= 0 || len(cores) == 0 {
		return ""
	}
	group := (len(cores) + width - 1) / width
	var strip strings.Builder
	for start := 0; start < len(cores); start += group {
		value := float64(0)
		for _, v := range cores[start:min(start+group, len(cores))] {
			value = max(value, v)
		}
		cell := string([]rune("▁▂▃▄▅▆▇█")[min(7, max(0, int(math.Ceil(value/12.5))-1))])
		if m.ascii {
			cell = string([]rune("._-:=+*#")[min(7, max(0, int(math.Ceil(value/12.5))-1))])
		}
		style := dashboardCPUStyle
		if value < 6 {
			style = dashboardCPUFillStyle
		}
		strip.WriteString(m.paint(style).Render(cell))
	}
	return strip.String()
}
func (m dashboardModel) gpuLine(host cli.DashboardHostView, leftWidth, rightWidth int) string {
	if host.Connection != cli.StateReachable {
		return ""
	}
	left, right := "", ""
	if dashboardOptionalPresent(host.GPU) {
		gpu := *host.GPU
		percent := dashboardPercent(gpu, gpu.Value.Utilization)
		value := dashboardPerformanceReading(m, gpu, host, 30*time.Second, fmt.Sprintf("%.0f%%", gpu.Value.Utilization))
		if dashboardFresh(gpu, host, m.now, 30*time.Second) {
			value = m.paint(dashboardGPUStyle).Bold(true).Render(value)
		}
		left = m.paint(dashboardMutedStyle).Render("GPU ") + m.performanceMeter(percent, host, leftWidth-4-ansi.StringWidth(value), dashboardGPUStyle) + value
		if gpu.Value.MemoryKind != "shared" && gpu.Value.MemoryTotalBytes > 0 {
			text := fmt.Sprintf("VRAM %.1f / %.1f GiB ", float64(gpu.Value.MemoryUsedBytes)/(1<<30), float64(gpu.Value.MemoryTotalBytes)/(1<<30))
			memory := dashboardPercent(gpu, float64(gpu.Value.MemoryUsedBytes)/float64(gpu.Value.MemoryTotalBytes)*100)
			right = dashboardPerformanceReading(m, gpu, host, 30*time.Second, text) + m.performanceMeter(memory, host, rightWidth-ansi.StringWidth(text), dashboardGPUStyle)
		}
	}
	if dashboardOptionalPresent(host.Battery) {
		battery := *host.Battery
		color := dashboardGoodStyle
		if battery.Value.Percent <= 20 {
			color = dashboardCachedStyle
		}
		percent := dashboardPercent(battery, battery.Value.Percent)
		value := dashboardPerformanceReading(m, battery, host, 30*time.Second, fmt.Sprintf("%.0f%%", battery.Value.Percent))
		if dashboardFresh(battery, host, m.now, 30*time.Second) {
			value = m.paint(color).Bold(true).Render(value)
		}
		right = m.paint(dashboardMutedStyle).Render("BAT ") + m.performanceMeter(percent, host, 10, color) + " " + value + " " + dashboardBatteryState(battery.Value)
	}
	return dashboardFit(left, leftWidth) + "  " + dashboardFit(right, rightWidth)
}
func dashboardBatteryState(battery hostmetrics.Battery) string {
	state := map[string]string{hostmetrics.BatteryDischarging: "on battery", hostmetrics.BatteryCharging: hostmetrics.BatteryCharging, hostmetrics.BatteryFull: hostmetrics.BatteryFull, hostmetrics.BatteryAC: "plugged in"}[battery.State]
	if battery.State == hostmetrics.BatteryDischarging && battery.Percent <= 20 {
		state = "low battery"
	}
	if battery.SecondsRemaining > 0 && (battery.State == hostmetrics.BatteryDischarging || battery.State == hostmetrics.BatteryCharging) {
		state += " " + dashboardSeconds(battery.SecondsRemaining)
	}
	return state
}
func dashboardCompactBatteryState(battery hostmetrics.Battery) string {
	if battery.State == hostmetrics.BatteryDischarging && battery.SecondsRemaining > 0 {
		return dashboardSeconds(battery.SecondsRemaining) + " left"
	}
	return dashboardBatteryState(battery)
}
func (m dashboardModel) ioLine(host cli.DashboardHostView, leftWidth, rightWidth int) string {
	if host.Connection != cli.StateReachable {
		return ""
	}
	if host.PerformanceVersion == 0 {
		return m.paint(dashboardCachedStyle).Render("Performance metrics need a newer mesh producer")
	}
	left, right := "", ""
	if dashboardOptionalPresent(host.Disk) {
		disk := host.Disk.Value
		value := "DISK r " + dashboardRate(disk.ReadBytesPerSecond) + "  w " + dashboardRate(disk.WriteBytesPerSecond)
		if disk.BusyAvailable {
			value = dashboardAlign(value, fmt.Sprintf(" %.0f%% busy", disk.BusyPercent), leftWidth)
		}
		left = dashboardPerformanceReading(m, *host.Disk, host, 10*time.Second, value)
	}
	if dashboardOptionalPresent(host.Network) {
		net := host.Network.Value
		right = dashboardPerformanceReading(m, *host.Network, host, 10*time.Second, "NET ↓ "+dashboardRate(net.ReceiveBytesPerSecond)+"  ↑ "+dashboardRate(net.SendBytesPerSecond))
	}
	return dashboardFit(left, leftWidth) + "  " + dashboardFit(right, rightWidth)
}
func dashboardRate(value float64) string {
	for _, unit := range []string{"B/s", "kB/s", "MB/s", "GB/s"} {
		if value < 1000 || unit == "GB/s" {
			if value < 10 && value > 0 {
				return fmt.Sprintf("%.1f %s", value, unit)
			}
			return fmt.Sprintf("%.0f %s", value, unit)
		}
		value /= 1000
	}
	return ""
}
func dashboardCompactRate(value float64) string {
	text := dashboardRate(value)
	for _, unit := range []struct{ long, short string }{{" GB/s", "G"}, {" MB/s", "M"}, {" kB/s", "k"}, {" B/s", "B"}} {
		text = strings.ReplaceAll(text, unit.long, unit.short)
	}
	return text
}
func (m dashboardModel) componentTemperatures(host cli.DashboardHostView) string {
	return strings.Join(m.temperatureValues(host), " · ")
}
func (m dashboardModel) temperatureValues(host cli.DashboardHostView) []string {
	var values []string
	for _, kind := range []string{"cpu", "soc", "gpu", "nvme", "ram"} {
		for _, metric := range host.Temperatures {
			if metric.Value.Kind != kind || !dashboardPresent(metric) {
				continue
			}
			label := hostmetrics.TemperatureLabel(kind)
			value := fmt.Sprintf("%s %.0f°", label, metric.Value.Celsius)
			values = append(values, dashboardPerformanceReading(m, metric, host, 30*time.Second, value))
		}
	}
	return values[:min(3, len(values))]
}
func (m dashboardModel) cardFacts(host cli.DashboardHostView, width int) string {
	counts := m.catalogCounts(host)
	if host.Services.Failed > 0 {
		counts += " · " + m.paint(dashboardFailureStyle).Render(fmt.Sprintf("%d failed", host.Services.Failed))
	}
	values := m.temperatureValues(host)
	if host.PerformanceVersion == 0 && dashboardPresent(host.Temperature) {
		values = []string{m.temperature(host)}
	}
	for len(values) > 0 {
		text := strings.Join(values, " · ") + " · " + counts
		if ansi.StringWidth(text) <= width {
			return text
		}
		values = values[:len(values)-1]
	}
	return counts
}
func (m dashboardModel) compactHost(host cli.DashboardHostView) []string {
	if host.Connection != cli.StateReachable {
		return []string{dashboardAlign(m.hostTitle(host), "last reply "+dashboardAge(m.now, host.LastReply), m.width), m.paint(dashboardCachedStyle).Render("    " + m.catalogCounts(host))}
	}
	first := m.paint(dashboardGoodStyle).Render(dashboardCompactMark(m.ascii)+" ") + dashboardFit(m.paint(dashboardTitleStyle).Render(safeText(host.Host.Alias)), 10) + "  CPU " + m.coloredPercent(host.CPU, host, m.paint(dashboardCPUStyle)) + "  RAM " + strings.ReplaceAll(strings.TrimSuffix(m.ramValue(host), " GiB"), " / ", "/")
	if dashboardOptionalPresent(host.GPU) {
		gpu := *host.GPU
		first += "  GPU " + dashboardPerformanceReading(m, gpu, host, 30*time.Second, fmt.Sprintf("%.0f%%", gpu.Value.Utilization))
		if gpu.Value.MemoryKind != "shared" && gpu.Value.MemoryTotalBytes > 0 {
			first += fmt.Sprintf("  VRAM %.1f/%.1f", float64(gpu.Value.MemoryUsedBytes)/(1<<30), float64(gpu.Value.MemoryTotalBytes)/(1<<30))
		}
	}
	if dashboardOptionalPresent(host.Battery) {
		value := fmt.Sprintf("  BAT %.0f%% %s", host.Battery.Value.Percent, dashboardCompactBatteryState(host.Battery.Value))
		first += dashboardPerformanceReading(m, *host.Battery, host, 30*time.Second, value)
	}
	up := ""
	if dashboardPresent(host.Uptime) {
		up = "up " + m.uptime(host)
	}
	second := "    "
	if dashboardOptionalPresent(host.Disk) {
		disk := host.Disk.Value
		value := "DISK r " + dashboardCompactRate(disk.ReadBytesPerSecond) + " w " + dashboardCompactRate(disk.WriteBytesPerSecond)
		if disk.BusyAvailable {
			value += fmt.Sprintf(" %.0f%% busy", disk.BusyPercent)
		}
		second += dashboardPerformanceReading(m, *host.Disk, host, 10*time.Second, value) + "  "
	}
	if dashboardOptionalPresent(host.Network) {
		net := host.Network.Value
		second += dashboardPerformanceReading(m, *host.Network, host, 10*time.Second, "NET ↓ "+dashboardCompactRate(net.ReceiveBytesPerSecond)+" ↑ "+dashboardCompactRate(net.SendBytesPerSecond)) + "  "
	}
	second += strings.Join(m.temperatureValues(host), "  ")
	if host.PerformanceVersion == 0 {
		second += "Performance metrics need a newer mesh producer"
	}
	return []string{dashboardAlign(first, up, m.width), dashboardFit(second, m.width)}
}

func dashboardCompactMark(ascii bool) string {
	if ascii {
		return "*"
	}
	return "●"
}
