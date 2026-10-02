package hostmetrics

import (
	"context"
	"strings"
)

const (
	MaximumTemperatures = 6
	MaximumCores        = 64
	PerformanceVersion  = 1
	KindCPU             = "cpu"
	KindGPU             = "gpu"
	KindSoC             = "soc"
	KindNVMe            = "nvme"
	KindRAM             = "ram"
	BatteryCharging     = "charging"
	BatteryDischarging  = "discharging"
	BatteryFull         = "full"
	BatteryAC           = "ac"
)

type ComponentTemperature struct {
	Kind      string  `json:"kind"`
	Label     string  `json:"label"`
	Celsius   float64 `json:"celsius"`
	AgeMillis int64   `json:"age"`
}

type Battery struct {
	Percent          float64 `json:"percent"`
	State            string  `json:"state"`
	SecondsRemaining uint64  `json:"secondsRemaining,omitempty"`
}

type GPU struct {
	MemoryKind       string  `json:"memoryKind,omitempty"`
	Utilization      float64 `json:"utilization"`
	MemoryUsedBytes  uint64  `json:"memoryUsedBytes"`
	MemoryTotalBytes uint64  `json:"memoryTotalBytes"`
}

type Disk struct {
	ReadBytesPerSecond  float64 `json:"readBytesPerSecond"`
	WriteBytesPerSecond float64 `json:"writeBytesPerSecond"`
	BusyPercent         float64 `json:"busyPercent"`
	BusyAvailable       bool    `json:"busyAvailable"`
}

type Network struct {
	ReceiveBytesPerSecond float64 `json:"receiveBytesPerSecond"`
	SendBytesPerSecond    float64 `json:"sendBytesPerSecond"`
}

type DiskCounters struct {
	ReadBytes, WriteBytes, BusyMillis uint64
	Devices                           string
	BusyAvailable                     bool
}

type NetworkCounters struct {
	ReceiveBytes, SendBytes uint64
	Devices                 string
}

// Optional collectors preserve the existing collector contract for older integrations.
type performanceCollector interface {
	Components(context.Context) ([]ComponentTemperature, error)
	GPU(context.Context) (GPU, error)
	Disk(context.Context) (DiskCounters, error)
	Network(context.Context) (NetworkCounters, error)
	Cores(context.Context) ([]Counters, error)
	Battery(context.Context) (Battery, error)
}

func TemperatureLabel(kind string) string {
	switch kind {
	case KindCPU:
		return "CPU"
	case KindGPU:
		return "GPU"
	case KindSoC:
		return "SoC"
	case KindNVMe:
		return "NVMe"
	case KindRAM:
		return "RAM"
	}
	return ""
}

// HottestTemperatures bounds wire data using a fixed order and hottest-per-kind reduction.
func HottestTemperatures(values []ComponentTemperature) []ComponentTemperature {
	result := make([]ComponentTemperature, 0, MaximumTemperatures)
	for _, kind := range []string{KindCPU, KindGPU, KindSoC, KindNVMe, KindRAM} {
		var hottest ComponentTemperature
		found := false
		for _, value := range values {
			if value.Kind != kind || !finite(value.Celsius) || value.Celsius < -273.15 || value.Celsius > 1000 {
				continue
			}
			if !found || value.Celsius > hottest.Celsius {
				hottest = value
				found = true
			}
		}
		if found {
			hottest.Label = TemperatureLabel(kind)
			result = append(result, hottest)
		}
	}
	return result
}

// AgeSnapshot owns all optional pointers and slices before adding transport age.
func AgeSnapshot(value Snapshot, age int64) Snapshot {
	value.CPU.AgeMillis += age
	value.RAM.AgeMillis += age
	value.Temperature.AgeMillis += age
	value.Uptime.AgeMillis += age
	value.Battery = ageOptional(value.Battery, age)
	value.GPU = ageOptional(value.GPU, age)
	value.Disk = ageOptional(value.Disk, age)
	value.Network = ageOptional(value.Network, age)
	value.Cores = ageOptional(value.Cores, age)
	if value.Cores != nil {
		value.Cores.Value = append([]float64(nil), value.Cores.Value...)
	}
	value.Temperatures = append([]ComponentTemperature(nil), value.Temperatures...)
	for i := range value.Temperatures {
		value.Temperatures[i].AgeMillis += age
	}
	return value
}
func ageOptional[T any](value *Reading[T], age int64) *Reading[T] {
	if value == nil {
		return nil
	}
	cloned := *value
	cloned.AgeMillis += age
	return &cloned
}

func realInterface(name string) bool {
	for _, prefix := range []string{"lo", "docker", "veth", "br-", "virbr", "bridge", "tailscale", "tun", "tap", "utun", "awdl", "llw", "vmnet", "wg"} {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}
	return name != ""
}

func addCounter(total *uint64, value uint64) bool {
	if value > ^uint64(0)-*total {
		return false
	}
	*total += value
	return true
}
