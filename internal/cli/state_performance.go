package cli

import "github.com/shaul/mesh/internal/hostmetrics"

func retainOptionalAge[T any](next, old *hostmetrics.Reading[T], elapsed int64) {
	if next == nil || old == nil {
		return
	}
	retainMetricAge(next, *old, elapsed)
}
func retainPerformanceAge(next *hostmetrics.Snapshot, old hostmetrics.Snapshot, elapsed int64) {
	retainOptionalAge(next.GPU, old.GPU, elapsed)
	retainOptionalAge(next.Disk, old.Disk, elapsed)
	retainOptionalAge(next.Network, old.Network, elapsed)
	retainOptionalAge(next.Cores, old.Cores, elapsed)
	retainOptionalAge(next.Battery, old.Battery, elapsed)
	if next.TemperaturesSample == "" || next.TemperaturesSample != old.TemperaturesSample {
		return
	}
	for i, value := range next.Temperatures {
		for _, previous := range old.Temperatures {
			if value.Kind == previous.Kind {
				next.Temperatures[i].AgeMillis = max(value.AgeMillis, previous.AgeMillis+elapsed)
			}
		}
	}
}
