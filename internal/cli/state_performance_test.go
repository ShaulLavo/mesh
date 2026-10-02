package cli

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/hostmetrics"
)

func performanceSnapshot() hostmetrics.Snapshot {
	return hostmetrics.Snapshot{PerformanceVersion: 1, TemperaturesSample: "sensor/1", Temperatures: []hostmetrics.ComponentTemperature{{Kind: "cpu", Label: "CPU", Celsius: 61, AgeMillis: 1000}}, GPU: &hostmetrics.Reading[hostmetrics.GPU]{Availability: hostmetrics.Available, Value: hostmetrics.GPU{Utilization: 20, MemoryKind: "shared", MemoryUsedBytes: 1024}, Sample: "gpu/1", AgeMillis: 1000}, Cores: &hostmetrics.Reading[[]float64]{Availability: hostmetrics.Available, Value: []float64{10, 100}, Sample: "cores/1", AgeMillis: 1000}, Battery: &hostmetrics.Reading[hostmetrics.Battery]{Availability: hostmetrics.Available, Value: hostmetrics.Battery{Percent: 71, State: "discharging"}, Sample: "bat/1", AgeMillis: 1000}}
}
func TestPerformanceValidationBoundsAndCanonicalLabels(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*hostmetrics.Snapshot)
	}{
		{"raw label", func(v *hostmetrics.Snapshot) { v.Temperatures[0].Label = "coretemp: Package id 0" }},
		{"unknown kind", func(v *hostmetrics.Snapshot) { v.Temperatures[0].Kind = "raw" }},
		{"duplicate kind", func(v *hostmetrics.Snapshot) { v.Temperatures = append(v.Temperatures, v.Temperatures[0]) }},
		{"temperature bound", func(v *hostmetrics.Snapshot) { v.Temperatures = make([]hostmetrics.ComponentTemperature, 7) }},
		{"temperature finite", func(v *hostmetrics.Snapshot) { v.Temperatures[0].Celsius = math.NaN() }},
		{"negative age", func(v *hostmetrics.Snapshot) { v.Temperatures[0].AgeMillis = -1 }},
		{"core count", func(v *hostmetrics.Snapshot) { v.Cores.Value = make([]float64, 65) }},
		{"core percent", func(v *hostmetrics.Snapshot) { v.Cores.Value[0] = 101 }},
		{"GPU percent", func(v *hostmetrics.Snapshot) { v.GPU.Value.Utilization = math.Inf(1) }},
		{"dedicated capacity", func(v *hostmetrics.Snapshot) { v.GPU.Value.MemoryKind = "" }},
		{"battery state", func(v *hostmetrics.Snapshot) { v.Battery.Value.State = "unknown" }},
		{"battery time", func(v *hostmetrics.Snapshot) { v.Battery.Value.SecondsRemaining = 7*86400 + 1 }},
		{"disk rate", func(v *hostmetrics.Snapshot) {
			v.Disk = &hostmetrics.Reading[hostmetrics.Disk]{Availability: hostmetrics.Available, Value: hostmetrics.Disk{ReadBytesPerSecond: -1}}
		}},
		{"network finite", func(v *hostmetrics.Snapshot) {
			v.Network = &hostmetrics.Reading[hostmetrics.Network]{Availability: hostmetrics.Available, Value: hostmetrics.Network{SendBytesPerSecond: math.NaN()}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := performanceSnapshot()
			test.mutate(&value)
			if validatePerformance(&value) == nil {
				t.Fatal("invalid performance snapshot accepted")
			}
		})
	}
	value := performanceSnapshot()
	if err := validatePerformance(&value); err != nil {
		t.Fatal(err)
	}
}
func TestPerformanceTransportReplayAndProjectionOwnership(t *testing.T) {
	now := time.Unix(1700000000, 0)
	input := performanceSnapshot()
	state := StateView{Connection: StateReachable, LastReply: now}
	state.applyMetrics(&input, now, 100*time.Millisecond)
	if state.Metrics.GPU.AgeMillis != 1100 || input.GPU.AgeMillis != 1000 {
		t.Fatal("transit changed source age")
	}
	state.applyMetrics(&input, now.Add(2*time.Second), 0)
	if state.Metrics.GPU.AgeMillis != 3100 || state.Metrics.Temperatures[0].AgeMillis != 3100 || state.Metrics.Cores.AgeMillis != 3100 || state.Metrics.Battery.AgeMillis != 3100 {
		t.Fatal("replayed sample became fresh", state.Metrics)
	}
	projected := projectDashboardState(DashboardHost{}, state)
	if !projected.GPU.MeasuredAt.Equal(now.Add(-1100*time.Millisecond)) || projected.PerformanceVersion != 1 {
		t.Fatal("projection lost measurement identity", projected)
	}
	projected.Cores.Value[0] = 42
	if state.Metrics.Cores.Value[0] != 10 {
		t.Fatal("projection aliases state cores")
	}
	cloned := state.Clone()
	cloned.Metrics.GPU.AgeMillis = 0
	cloned.Metrics.Cores.Value[0] = 42
	cloned.Metrics.Temperatures[0].Celsius = 0
	if state.Metrics.GPU.AgeMillis != 3100 || state.Metrics.Cores.Value[0] != 10 || state.Metrics.Temperatures[0].Celsius != 61 {
		t.Fatal("state clone aliases optional values")
	}
	input.GPU.Sample = "gpu/2"
	input.TemperaturesSample = "sensor/2"
	input.GPU.AgeMillis = 0
	input.Temperatures[0].AgeMillis = 0
	state.applyMetrics(&input, now.Add(4*time.Second), 100*time.Millisecond)
	if state.Metrics.GPU.AgeMillis != 100 || state.Metrics.Temperatures[0].AgeMillis != 100 {
		t.Fatal("new sample retained old age")
	}
}
func TestPerformanceAdditiveLegacyWireAndAbsentHardware(t *testing.T) {
	var legacy hostmetrics.Snapshot
	if err := json.Unmarshal([]byte(`{"cpu":{"availability":"available","value":20,"sample":"cpu/1","ageMillis":100},"ram":{"availability":"available","value":{"totalBytes":4096,"availableBytes":1024,"estimate":"Linux MemAvailable estimate"},"sample":"ram/1","ageMillis":100},"temperature":{"availability":"unsupported","value":{"sensor":"","celsius":0},"ageMillis":0},"uptime":{"availability":"available","value":120,"sample":"up/1","ageMillis":0}}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if err := validateStateMetrics(&legacy); err != nil {
		t.Fatal("old snapshot rejected", err)
	}
	view := projectDashboardState(DashboardHost{}, StateView{Metrics: &legacy, MetricsReceivedAt: time.Now()})
	if view.PerformanceVersion != 0 || view.GPU != nil || view.Battery != nil || len(view.Temperatures) != 0 || view.CPU.Value != 20 {
		t.Fatal("old producer projection changed", view)
	}
	legacy.PerformanceVersion = 1
	wire, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{`"gpu"`, `"battery"`, `"temperatures"`, `"disk"`, `"network"`, `"cores"`} {
		if strings.Contains(string(wire), absent) {
			t.Fatal("absent hardware serialized", absent)
		}
	}
	view = projectDashboardState(DashboardHost{}, StateView{Metrics: &legacy, MetricsReceivedAt: time.Now()})
	if view.PerformanceVersion != 1 || view.GPU != nil || view.Battery != nil {
		t.Fatal("hardware omission confused with old producer")
	}
}
