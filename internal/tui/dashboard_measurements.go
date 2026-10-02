package tui

import (
	"fmt"
	"github.com/shaul/mesh/internal/cli"
	"math"
	"strings"
	"time"
)

const dashboardUnsupported = "unsupported"

func dashboardReading[T any](metric cli.DashboardMeasurement[T], host cli.DashboardHostView, now time.Time, limit time.Duration, value string) string {
	if metric.State == dashboardUnsupported {
		return dashboardUnsupported
	}
	if metric.Sample == "" || metric.MeasuredAt.IsZero() {
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
	if host.PerformanceVersion > 0 {
		return m.componentTemperatures(host)
	}
	metric := host.Temperature
	if !dashboardPresent(metric) {
		return ""
	}
	value := fmt.Sprintf("CPU %.0f°", metric.Value.Celsius)
	return dashboardPerformanceReading(m, metric, host, 30*time.Second, value)
}
func (m dashboardModel) uptime(host cli.DashboardHostView) string {
	metric := host.Uptime
	value := dashboardSeconds(metric.Value)
	return dashboardReading(metric, host, m.now, 30*time.Second, value)
}
func dashboardAge(now, measured time.Time) string {
	if measured.IsZero() {
		return "never"
	}
	return dashboardDuration(max(time.Duration(0), now.Sub(measured)).Round(time.Second))
}

func dashboardDuration(duration time.Duration) string {
	return dashboardSeconds(uint64(max(time.Duration(0), duration) / time.Second))
}
func dashboardSeconds(seconds uint64) string {
	days, hours, minutes := seconds/86400, (seconds%86400)/3600, (seconds%3600)/60
	switch {
	case days > 0:
		if hours > 0 {
			return fmt.Sprintf("%dd %dh", days, hours)
		}
		return fmt.Sprintf("%dd", days)
	case hours > 0:
		if minutes > 0 {
			return fmt.Sprintf("%dh %dm", hours, minutes)
		}
		return fmt.Sprintf("%dh", hours)
	case minutes > 0:
		return fmt.Sprintf("%dm", minutes)
	default:
		return fmt.Sprintf("%ds", seconds)
	}
}
