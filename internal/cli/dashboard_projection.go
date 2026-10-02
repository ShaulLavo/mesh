package cli

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shaul/mesh/internal/hostmetrics"
	"github.com/shaul/mesh/internal/protocol"
)

const (
	dashboardSessionLimit = 6
	dashboardServiceLimit = 12
	dashboardTextLimit    = 256
)

func projectDashboardState(host DashboardHost, state StateView) DashboardHostView {
	view := DashboardHostView{Host: host, Connection: state.Connection, Problem: dashboardText(state.Problem), LastReply: state.LastReply}
	view.Sessions = projectDashboardSessions(state.Sessions, state.Sections[protocol.TopicSessions])
	view.Services = projectDashboardServices(state.Services, state.Sections[protocol.TopicServices])
	if state.Metrics == nil {
		return view
	}
	metrics := state.Metrics
	view.PerformanceVersion = metrics.PerformanceVersion
	view.GPU = projectOptionalMetric(metrics.GPU, state.MetricsReceivedAt)
	view.Disk = projectOptionalMetric(metrics.Disk, state.MetricsReceivedAt)
	view.Network = projectOptionalMetric(metrics.Network, state.MetricsReceivedAt)
	view.Cores = projectOptionalMetric(metrics.Cores, state.MetricsReceivedAt)
	if view.Cores != nil {
		view.Cores.Value = append([]float64(nil), view.Cores.Value...)
	}
	view.Battery = projectOptionalMetric(metrics.Battery, state.MetricsReceivedAt)
	for _, entry := range metrics.Temperatures {
		reading := hostmetrics.Reading[hostmetrics.ComponentTemperature]{Availability: hostmetrics.Available, Value: entry, Sample: metrics.TemperaturesSample, AgeMillis: entry.AgeMillis, Failing: metrics.TemperaturesFailing}
		view.Temperatures = append(view.Temperatures, projectDashboardMetric(reading, state.MetricsReceivedAt))
	}
	view.CPU = projectDashboardMetric(metrics.CPU, state.MetricsReceivedAt)
	view.Uptime = projectDashboardMetric(metrics.Uptime, state.MetricsReceivedAt)
	ram := projectDashboardMetric(metrics.RAM, state.MetricsReceivedAt)
	view.RAM = DashboardMeasurement[DashboardMemory]{State: ram.State, Value: DashboardMemory{TotalBytes: ram.Value.TotalBytes, AvailableBytes: ram.Value.AvailableBytes, Estimate: dashboardText(ram.Value.Estimate)}, Sample: ram.Sample, Segment: ram.Segment, MeasuredAt: ram.MeasuredAt, Failing: ram.Failing, Problem: ram.Problem}
	temperature := projectDashboardMetric(metrics.Temperature, state.MetricsReceivedAt)
	view.Temperature = DashboardMeasurement[DashboardTemperature]{State: temperature.State, Value: DashboardTemperature{Sensor: dashboardText(temperature.Value.Sensor), Celsius: temperature.Value.Celsius}, Sample: temperature.Sample, Segment: temperature.Segment, MeasuredAt: temperature.MeasuredAt, Failing: temperature.Failing, Problem: temperature.Problem}
	return view
}

func projectDashboardMetric[T any](reading hostmetrics.Reading[T], received time.Time) DashboardMeasurement[T] {
	measured := time.Time{}
	if !received.IsZero() {
		measured = received.Add(-time.Duration(reading.AgeMillis) * time.Millisecond)
	}
	return DashboardMeasurement[T]{State: reading.Availability, Value: reading.Value, Sample: dashboardText(reading.Sample), Segment: reading.Segment, MeasuredAt: measured, Failing: reading.Failing, Problem: dashboardText(reading.Problem)}
}

func projectDashboardSessions(rows []protocol.SessionInfo, section ObservedSection) DashboardCatalog[DashboardSession] {
	result := DashboardCatalog[DashboardSession]{Total: len(rows), ObservedAt: dashboardObservedAt(section), Failing: section.Observation.Failing}
	for _, row := range rows[:min(len(rows), dashboardSessionLimit)] {
		result.Rows = append(result.Rows, DashboardSession{ID: dashboardText(row.ID), Name: dashboardText(row.Label), State: dashboardText(row.State), Command: dashboardCommandText(row.Command)})
	}
	return result
}

func projectDashboardServices(rows []protocol.ServiceInfo, section ObservedSection) DashboardCatalog[DashboardService] {
	result := DashboardCatalog[DashboardService]{Total: len(rows), ObservedAt: dashboardObservedAt(section), Failing: section.Observation.Failing}
	for _, row := range rows {
		if dashboardServiceFailed(row) {
			result.Failed++
			continue
		}
		if row.Demand == nil || row.Demand.State == protocol.DemandRunning {
			result.Ready++
		}
	}
	// Select failures before healthy rows without copying an unbounded catalog.
	for _, failed := range []bool{true, false} {
		result.Rows = appendDashboardServices(result.Rows, rows, failed)
	}
	return result
}
func appendDashboardServices(selected []DashboardService, rows []protocol.ServiceInfo, failed bool) []DashboardService {
	for _, row := range rows {
		if len(selected) == dashboardServiceLimit {
			break
		}
		service := projectDashboardService(row)
		if service.Failed == failed {
			selected = append(selected, service)
		}
	}
	return selected
}
func projectDashboardService(row protocol.ServiceInfo) DashboardService {
	service := DashboardService{Name: dashboardText(row.Name), State: "ready", Problem: dashboardText(row.Problem), Failed: dashboardServiceFailed(row)}
	if !row.Healthy {
		service.State = "unhealthy"
	}
	if row.Demand == nil {
		return service
	}
	service.State = dashboardText(row.Demand.State)
	if row.Demand.Failure != "" {
		service.Problem = dashboardText(row.Demand.Failure)
		service.Failed = true
	}
	service.Failed = service.Failed || row.Demand.State == protocol.DemandFailed
	return service
}
func dashboardObservedAt(section ObservedSection) time.Time {
	if section.ReceivedAt.IsZero() {
		return time.Time{}
	}
	return section.ReceivedAt.Add(-time.Duration(section.Observation.AgeMillis) * time.Millisecond)
}
func dashboardText(value string) string {
	return dashboardClip(SafeTerminalText(dashboardClip(value, dashboardTextLimit)), dashboardTextLimit)
}
func dashboardClip(value string, limit int) string {
	end := min(len(value), limit)
	for end < len(value) && end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return strings.Clone(value[:end])
}
func dashboardCommandText(parts []string) string {
	var result strings.Builder
	for _, part := range parts {
		if result.Len() >= dashboardTextLimit {
			break
		}
		if result.Len() > 0 {
			result.WriteByte(' ')
		}
		result.WriteString(dashboardClip(part, dashboardTextLimit-result.Len()))
	}
	return dashboardText(result.String())
}

func dashboardServiceFailed(row protocol.ServiceInfo) bool {
	if !row.Healthy || row.Problem != "" {
		return true
	}
	return row.Demand != nil && (row.Demand.Failure != "" || row.Demand.State == protocol.DemandFailed)
}

func projectOptionalMetric[T any](metric *hostmetrics.Reading[T], received time.Time) *DashboardMeasurement[T] {
	if metric == nil {
		return nil
	}
	value := projectDashboardMetric(*metric, received)
	return &value
}
