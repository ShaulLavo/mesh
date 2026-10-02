package cli

import (
	"fmt"
	"math"

	"github.com/shaul/mesh/internal/hostmetrics"
)

func finiteBetween(value, low, high float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= low && value <= high
}
func validateOptionalMetric[T any](reading *hostmetrics.Reading[T]) error {
	if reading == nil {
		return nil
	}
	return validateMetricReading(*reading)
}
func validatePerformance(metrics *hostmetrics.Snapshot) error {
	if metrics.PerformanceVersion < 0 || len(metrics.Temperatures) > hostmetrics.MaximumTemperatures {
		return fmt.Errorf("invalid performance metadata bounds")
	}
	for _, err := range []error{
		validateTemperatures(metrics.Temperatures),
		validateOptionalMetric(metrics.GPU), validateOptionalMetric(metrics.Disk), validateOptionalMetric(metrics.Network), validateOptionalMetric(metrics.Cores), validateOptionalMetric(metrics.Battery),
		validatePerformanceGPU(metrics.GPU), validatePerformanceDisk(metrics.Disk), validatePerformanceNetwork(metrics.Network), validatePerformanceCores(metrics.Cores), validatePerformanceBattery(metrics.Battery),
	} {
		if err != nil {
			return err
		}
	}
	return nil
}
func validateTemperatures(values []hostmetrics.ComponentTemperature) error {
	seen := map[string]bool{}
	for _, value := range values {
		label := hostmetrics.TemperatureLabel(value.Kind)
		if label == "" || label != value.Label || seen[value.Kind] || !finiteBetween(value.Celsius, -273.15, 1000) {
			return fmt.Errorf("invalid component temperature")
		}
		if err := validateStateAge(value.AgeMillis); err != nil {
			return err
		}
		seen[value.Kind] = true
	}
	return nil
}
func validatePerformanceGPU(metric *hostmetrics.Reading[hostmetrics.GPU]) error {
	if metric == nil {
		return nil
	}
	value := metric.Value
	if !finiteBetween(value.Utilization, 0, 100) || value.MemoryKind != "" && value.MemoryKind != "shared" || value.MemoryKind != "shared" && value.MemoryUsedBytes > value.MemoryTotalBytes {
		return fmt.Errorf("invalid GPU measurement")
	}
	return nil
}
func validatePerformanceDisk(metric *hostmetrics.Reading[hostmetrics.Disk]) error {
	if metric == nil {
		return nil
	}
	value := metric.Value
	if !finiteBetween(value.ReadBytesPerSecond, 0, 1e15) || !finiteBetween(value.WriteBytesPerSecond, 0, 1e15) || !finiteBetween(value.BusyPercent, 0, 100) {
		return fmt.Errorf("invalid disk measurement")
	}
	return nil
}
func validatePerformanceNetwork(metric *hostmetrics.Reading[hostmetrics.Network]) error {
	if metric == nil {
		return nil
	}
	value := metric.Value
	if !finiteBetween(value.ReceiveBytesPerSecond, 0, 1e15) || !finiteBetween(value.SendBytesPerSecond, 0, 1e15) {
		return fmt.Errorf("invalid network measurement")
	}
	return nil
}
func validatePerformanceCores(metric *hostmetrics.Reading[[]float64]) error {
	if metric == nil {
		return nil
	}
	if len(metric.Value) > hostmetrics.MaximumCores || metric.Availability == hostmetrics.Available && len(metric.Value) == 0 {
		return fmt.Errorf("invalid per-core measurement count")
	}
	for _, value := range metric.Value {
		if !finiteBetween(value, 0, 100) {
			return fmt.Errorf("invalid per-core measurement")
		}
	}
	return nil
}
func validatePerformanceBattery(metric *hostmetrics.Reading[hostmetrics.Battery]) error {
	if metric == nil || metric.Availability != hostmetrics.Available {
		return nil
	}
	value := metric.Value
	if !finiteBetween(value.Percent, 0, 100) || value.SecondsRemaining > 7*86400 {
		return fmt.Errorf("invalid battery measurement")
	}
	switch value.State {
	case hostmetrics.BatteryCharging, hostmetrics.BatteryDischarging, hostmetrics.BatteryFull, hostmetrics.BatteryAC:
		return nil
	default:
		return fmt.Errorf("invalid battery state")
	}
}
