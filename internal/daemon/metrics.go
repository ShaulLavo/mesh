package daemon

import (
	"context"

	"github.com/shaul/mesh/internal/hostmetrics"
	"github.com/shaul/mesh/internal/protocol"
)

func (l *lifecycle) hostMetrics(ctx context.Context, request protocol.Control) (protocol.Control, error) {
	snapshot, err := l.metrics.Read(ctx)
	if err != nil {
		return protocol.Control{}, err
	}
	memory := snapshot.Memory
	temperature := snapshot.Temperature
	metrics := &protocol.HostMetrics{
		CPU:         metricValue(snapshot.CPU, snapshot.CPU.Value),
		Memory:      metricValue(memory, protocol.MemoryUsage{TotalBytes: memory.Value.TotalBytes, AvailableBytes: memory.Value.AvailableBytes}),
		Temperature: metricValue(temperature, protocol.Temperature{Sensor: temperature.Value.Sensor, Celsius: temperature.Value.Celsius}),
		Uptime:      metricValue(snapshot.Uptime, snapshot.Uptime.Value),
	}
	return protocol.Control{Type: protocol.TypeHostMetricsResult, RequestID: request.RequestID, Metrics: metrics}, nil
}

func metricValue[T, U any](value hostmetrics.Reading[T], wire U) protocol.MetricValue[U] {
	return protocol.MetricValue[U]{State: value.State, Value: wire, Sample: value.Sample, AgeMillis: value.AgeMillis, Problem: value.Problem}
}
