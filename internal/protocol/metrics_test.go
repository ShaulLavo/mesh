package protocol

import (
	"math"
	"testing"
)

func validHostMetrics() HostMetrics {
	return HostMetrics{
		CPU:         MetricValue[float64]{State: MetricAvailable, Value: 0, Sample: "instance/1/1"},
		Memory:      MetricValue[MemoryUsage]{State: MetricAvailable, Value: MemoryUsage{TotalBytes: 100, AvailableBytes: 100}, Sample: "instance/0/2"},
		Temperature: MetricValue[Temperature]{State: MetricUnsupported},
		Uptime:      MetricValue[uint64]{State: MetricAvailable, Sample: "instance/0/3"},
	}
}

func TestValidateHostMetrics(t *testing.T) {
	tests := []struct {
		name   string
		modify func(*HostMetrics)
	}{
		{"missing state", func(m *HostMetrics) { m.CPU.State = "" }},
		{"missing identity", func(m *HostMetrics) { m.CPU.Sample = " " }},
		{"negative age", func(m *HostMetrics) { m.CPU.AgeMillis = -1 }},
		{"unmeasured age", func(m *HostMetrics) { m.Temperature.AgeMillis = 1 }},
		{"nonfinite CPU", func(m *HostMetrics) { m.CPU.Value = math.NaN() }},
		{"negative CPU", func(m *HostMetrics) { m.CPU.Value = -1 }},
		{"CPU over total", func(m *HostMetrics) { m.CPU.Value = 101 }},
		{"memory over total", func(m *HostMetrics) { m.Memory.Value.AvailableBytes = 101 }},
		{"empty memory", func(m *HostMetrics) { m.Memory.Value = MemoryUsage{} }},
		{"nonfinite temperature", func(m *HostMetrics) { m.Temperature.Value.Celsius = math.Inf(1) }},
		{"unnamed temperature", func(m *HostMetrics) { m.Temperature.State = MetricAvailable; m.Temperature.Sample = "sample" }},
	}
	if err := ValidateHostMetrics(validHostMetrics()); err != nil {
		t.Fatalf("valid zero utilization and optional sensor rejected: %v", err)
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			metrics := validHostMetrics()
			test.modify(&metrics)
			if err := ValidateHostMetrics(metrics); err == nil {
				t.Fatal("invalid wire reading accepted")
			}
		})
	}
}

func TestHostMetricsControlRoundTrip(t *testing.T) {
	metrics := validHostMetrics()
	encoded, err := (Control{Type: TypeHostMetricsResult, RequestID: "m-1", Metrics: &metrics}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeControl(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Metrics == nil || *decoded.Metrics != metrics || decoded.RequestID != "m-1" {
		t.Fatalf("metrics round trip = %+v", decoded)
	}
}
