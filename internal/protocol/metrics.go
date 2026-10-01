package protocol

import (
	"fmt"
	"math"
	"strings"
)

const (
	TypeHostMetrics       = "host.metrics"
	TypeHostMetricsResult = "host.metrics.result"
	MetricAvailable       = "available"
	MetricUnavailable     = "unavailable"
	MetricUnsupported     = "unsupported"
)

type MetricValue[T any] struct {
	State     string `json:"state"`
	Value     T      `json:"value"`
	Sample    string `json:"sample"`
	AgeMillis int64  `json:"ageMillis"`
	Problem   string `json:"problem,omitempty"`
}

type MemoryUsage struct {
	TotalBytes     uint64 `json:"totalBytes"`
	AvailableBytes uint64 `json:"availableBytes"`
}

type Temperature struct {
	Sensor  string  `json:"sensor"`
	Celsius float64 `json:"celsius"`
}

type HostMetrics struct {
	CPU         MetricValue[float64]     `json:"cpu"`
	Memory      MetricValue[MemoryUsage] `json:"memory"`
	Temperature MetricValue[Temperature] `json:"temperature"`
	Uptime      MetricValue[uint64]      `json:"uptime"`
}

// ValidateHostMetrics checks untrusted readings before they enter retained views.
func ValidateHostMetrics(m HostMetrics) error {
	checks := []error{
		validateMetric("CPU", m.CPU), validateMetric("memory", m.Memory),
		validateMetric("temperature", m.Temperature), validateMetric("uptime", m.Uptime),
	}
	for _, err := range checks {
		if err != nil {
			return err
		}
	}
	if !finite(m.CPU.Value) || m.CPU.Value < 0 || m.CPU.Value > 100 {
		return fmt.Errorf("protocol: invalid CPU percentage")
	}
	if m.Memory.Value.AvailableBytes > m.Memory.Value.TotalBytes {
		return fmt.Errorf("protocol: available memory exceeds total")
	}
	if m.Memory.State == MetricAvailable && m.Memory.Value.TotalBytes == 0 {
		return fmt.Errorf("protocol: available memory reading has zero total")
	}
	t := m.Temperature.Value
	if !finite(t.Celsius) || t.Celsius < -273.15 || t.Celsius > 1000 {
		return fmt.Errorf("protocol: invalid temperature")
	}
	if m.Temperature.State == MetricAvailable && strings.TrimSpace(t.Sensor) == "" {
		return fmt.Errorf("protocol: available temperature has no sensor")
	}
	return nil
}

func validateMetric[T any](name string, m MetricValue[T]) error {
	if m.State != MetricAvailable && m.State != MetricUnavailable && m.State != MetricUnsupported {
		return fmt.Errorf("protocol: invalid %s availability %q", name, m.State)
	}
	if m.AgeMillis < 0 || len(m.Sample) > 256 || len(m.Problem) > 1024 {
		return fmt.Errorf("protocol: invalid %s metadata", name)
	}
	if m.State == MetricAvailable && strings.TrimSpace(m.Sample) == "" {
		return fmt.Errorf("protocol: available %s has no sample identity", name)
	}
	if m.Sample == "" && m.AgeMillis != 0 {
		return fmt.Errorf("protocol: unmeasured %s has an age", name)
	}
	return nil
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }
