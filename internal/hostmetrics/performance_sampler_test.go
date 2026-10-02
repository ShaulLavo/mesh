package hostmetrics

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

type performanceFixture struct {
	step    uint64
	sensors int
	fail    bool
	block   chan struct{}
}

func (c *performanceFixture) CPU(context.Context) (Counters, error) {
	c.step++
	return Counters{User: float64(c.step) * 10, Idle: float64(c.step) * 10}, nil
}
func (*performanceFixture) Memory(context.Context) (Memory, error) {
	return Memory{TotalBytes: 100, AvailableBytes: 50, Estimate: "fixture"}, nil
}
func (*performanceFixture) Uptime(context.Context) (uint64, error) { return 100, nil }
func (*performanceFixture) Temperature(context.Context) (Temperature, error) {
	return Temperature{Sensor: "package", Celsius: 69}, nil
}
func (c *performanceFixture) Components(context.Context) ([]ComponentTemperature, error) {
	if c.fail {
		return nil, errors.New("sensor failure")
	}
	return []ComponentTemperature{{Kind: "cpu", Celsius: 69}, {Kind: "cpu", Celsius: 50}, {Kind: "soc", Celsius: 48}}, nil
}
func (c *performanceFixture) GPU(ctx context.Context) (GPU, error) {
	c.sensors++
	if c.block != nil {
		close(c.block)
		<-ctx.Done()
		return GPU{}, fmt.Errorf("fixture GPU cancellation: %w", ctx.Err())
	}
	if c.fail {
		return GPU{}, errors.New("GPU failure")
	}
	return GPU{Utilization: 37, MemoryUsedBytes: 3, MemoryTotalBytes: 8}, nil
}
func (c *performanceFixture) Disk(context.Context) (DiskCounters, error) {
	if c.fail {
		return DiskCounters{}, errors.New("disk failure")
	}
	return DiskCounters{ReadBytes: c.step * 1000, WriteBytes: c.step * 2000, BusyMillis: c.step * 200, Devices: "disk0", BusyAvailable: true}, nil
}
func (c *performanceFixture) Network(context.Context) (NetworkCounters, error) {
	if c.fail {
		return NetworkCounters{}, errors.New("network failure")
	}
	return NetworkCounters{ReceiveBytes: c.step * 3000, SendBytes: c.step * 4000, Devices: "eth0"}, nil
}
func (c *performanceFixture) Cores(context.Context) ([]Counters, error) {
	return []Counters{{User: float64(c.step) * 10, Idle: float64(c.step) * 10}, {User: float64(c.step) * 20}}, nil
}
func (*performanceFixture) Battery(context.Context) (Battery, error) {
	return Battery{}, ErrUnsupported
}
func TestPerformanceIndependentRatesAgeAndLongGap(t *testing.T) {
	now := time.Unix(100, 0)
	c := &performanceFixture{}
	s := NewWithCollector(c, func() time.Time { return now })
	first, err := s.Read(t.Context())
	if err != nil || first.Disk.Availability != Unavailable || first.Network.Availability != Unavailable || first.Battery != nil {
		t.Fatal(first, err)
	}
	now = now.Add(2 * time.Second)
	value, err := s.Read(t.Context())
	if err != nil || value.Disk.Value.ReadBytesPerSecond != 500 || value.Network.Value.ReceiveBytesPerSecond != 1500 || value.Disk.Value.BusyPercent != 10 || value.Cores.Value[1] != 100 || value.GPU.AgeMillis != 2000 || c.sensors != 1 {
		t.Fatal(value, err)
	}
	value.Temperatures[0].Celsius = 999
	value.Cores.Value[0] = 999
	owned, _ := s.Read(t.Context())
	if owned.Temperatures[0].Celsius != 69 || owned.Cores.Value[0] != 50 {
		t.Fatal("snapshot aliases cached lists")
	}
	now = now.Add(8 * time.Second)
	_, _ = s.Read(t.Context())
	if c.sensors != 2 {
		t.Fatal("sensor cadence", c.sensors)
	}
	c.fail = true
	now = now.Add(10 * time.Second)
	failed, _ := s.Read(t.Context())
	if !failed.GPU.Failing || failed.GPU.AgeMillis != 10000 || failed.Temperatures[0].AgeMillis != 10000 || !failed.Disk.Failing || failed.CPU.Availability != Available {
		t.Fatal(failed)
	}
	c.fail = false
	now = now.Add(BaselineGap + time.Second)
	warming, _ := s.Read(t.Context())
	if !warming.Disk.Failing || !warming.Network.Failing || !warming.Cores.Failing {
		t.Fatal("long gap minted rate", warming)
	}
	now = now.Add(2 * time.Second)
	fresh, _ := s.Read(t.Context())
	if fresh.Disk.Failing || fresh.Disk.Segment == value.Disk.Segment || fresh.Cores.Value[1] != 100 {
		t.Fatal(fresh)
	}
}
func TestPerformanceDemandReleaseCancelsInFlightCollector(t *testing.T) {
	c := &performanceFixture{block: make(chan struct{})}
	s := NewWithCollector(c, time.Now)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx, func(Snapshot) { t.Error("released demand published") }) }()
	release := s.Demand(MinimumInterval)
	select {
	case <-c.block:
	case <-time.After(3 * time.Second):
		t.Fatal("sample did not start")
	}
	start := time.Now()
	release()
	cancel()
	select {
	case <-done:
		if time.Since(start) > 200*time.Millisecond {
			t.Fatal("slow demand cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("collector retained run")
	}
}
func TestTemperatureBoundsAndCanonicalLabels(t *testing.T) {
	values := make([]ComponentTemperature, 1000)
	for i := range values {
		values[i] = ComponentTemperature{Kind: "cpu", Label: "raw sensor", Celsius: float64(i)}
	}
	values = append(values, ComponentTemperature{Kind: "unknown", Celsius: 999})
	hottest := HottestTemperatures(values)
	if len(hottest) != 1 || hottest[0].Label != "CPU" || hottest[0].Celsius != 999 {
		t.Fatal(hottest)
	}
}

type fractionalPerformanceFixture struct{ *performanceFixture }

func (c *fractionalPerformanceFixture) Cores(context.Context) ([]Counters, error) {
	return []Counters{{User: float64(c.step), Idle: float64(c.step) * 2}}, nil
}

func TestPerformanceWireResolutionKeepsExactBaselines(t *testing.T) {
	now := time.Unix(100, 0)
	c := &fractionalPerformanceFixture{performanceFixture: &performanceFixture{}}
	s := NewWithCollector(c, func() time.Time { return now })
	_, _ = s.Read(t.Context())
	now = now.Add(2003 * time.Millisecond)
	value, err := s.Read(t.Context())
	if err != nil || value.Cores.Value[0] != 33 || value.Disk.Value.ReadBytesPerSecond != 499 || value.Network.Value.ReceiveBytesPerSecond != 1498 || value.Disk.Value.BusyPercent != 10 {
		t.Fatal("wire precision", value, err)
	}
	if s.performance.coresBefore[0].User != 2 || s.performance.diskBefore.ReadBytes != 2000 || s.performance.networkBefore.ReceiveBytes != 6000 {
		t.Fatal("quantized source baseline")
	}
}
