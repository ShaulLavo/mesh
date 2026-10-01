package hostmetrics

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeCollector struct {
	counts                                       counters
	memory                                       MemoryUsage
	temperature                                  Temperature
	uptime                                       uint64
	cpuErr, memoryErr, temperatureErr, uptimeErr error
	calls, sensors                               int
	block                                        chan struct{}
	entered                                      chan struct{}
}

func goodCollector() *fakeCollector {
	return &fakeCollector{counts: counters{user: 10, idle: 10}, memory: MemoryUsage{100, 60}, temperature: Temperature{"cpu-thermal", 42}, uptime: 100}
}

func (c *fakeCollector) CPU(ctx context.Context) (counters, error) {
	c.calls++
	if c.entered != nil {
		close(c.entered)
		c.entered = nil
	}
	if c.block != nil {
		select {
		case <-c.block:
		case <-ctx.Done():
			return counters{}, ctx.Err()
		}
	}
	return c.counts, c.cpuErr
}
func (c *fakeCollector) Memory(context.Context) (MemoryUsage, error) { return c.memory, c.memoryErr }
func (c *fakeCollector) Temperature(context.Context) (Temperature, error) {
	c.sensors++
	return c.temperature, c.temperatureErr
}
func (c *fakeCollector) Uptime(context.Context) (uint64, error) { return c.uptime, c.uptimeErr }

func readSample(t *testing.T, s *Sampler) Snapshot {
	t.Helper()
	v, err := s.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestSamplerDemandCacheIndependentAgesAndLastGood(t *testing.T) {
	now := time.Now()
	c := goodCollector()
	s := newSampler(c, func() time.Time { return now })
	first := readSample(t, s)
	if first.CPU.State != Unavailable || first.CPU.Sample != "" || first.Memory.State != Available {
		t.Fatalf("first sample = %+v", first)
	}
	now = now.Add(time.Second)
	cached := readSample(t, s)
	if c.calls != 1 || cached.Memory.Sample != first.Memory.Sample || cached.Memory.AgeMillis != 1000 {
		t.Fatalf("cache refreshed or failed to age: %+v, calls %d", cached, c.calls)
	}
	now = now.Add(time.Second)
	c.counts.user += 10
	c.counts.idle += 10
	second := readSample(t, s)
	if second.CPU.State != Available || second.CPU.Value != 50 || second.Memory.Sample == first.Memory.Sample {
		t.Fatalf("second sample = %+v", second)
	}
	if second.Temperature.Sample != first.Temperature.Sample || second.Temperature.AgeMillis != 2000 || c.sensors != 1 {
		t.Fatalf("sensor resampled at CPU cadence: %+v", second.Temperature)
	}
	now = now.Add(2 * time.Second)
	c.counts.user += 5
	c.counts.idle += 5
	c.memoryErr = errors.New("memory denied")
	third := readSample(t, s)
	if third.Memory.Sample != second.Memory.Sample || third.Memory.AgeMillis != 2000 || third.Memory.Problem != "memory denied" || third.Memory.State != Available {
		t.Fatalf("last good memory not retained: %+v", third.Memory)
	}
	if third.CPU.Sample == second.CPU.Sample || third.CPU.AgeMillis != 0 {
		t.Fatalf("CPU froze with RAM: %+v", third.CPU)
	}
	now = now.Add(6 * time.Second)
	c.counts.user += 10
	c.counts.idle += 10
	fourth := readSample(t, s)
	if c.sensors != 2 || fourth.Temperature.Sample == first.Temperature.Sample || fourth.Temperature.AgeMillis != 0 {
		t.Fatalf("sensor did not refresh after ten seconds: %+v", fourth.Temperature)
	}
}

func TestSamplerCPUResetLongGapAndCounterDecrease(t *testing.T) {
	now := time.Now()
	c := goodCollector()
	s := newSampler(c, func() time.Time { return now })
	readSample(t, s)
	now = now.Add(2 * time.Second)
	c.counts.user += 2
	c.counts.idle += 2
	first := readSample(t, s)
	now = now.Add(11 * time.Second)
	c.counts.user += 20
	gap := readSample(t, s)
	if gap.CPU.Sample != first.CPU.Sample || gap.CPU.Problem == "" || gap.CPU.AgeMillis != 11000 {
		t.Fatalf("gap represented as fresh utilization: %+v", gap.CPU)
	}
	now = now.Add(2 * time.Second)
	c.counts.user += 2
	c.counts.idle += 2
	second := readSample(t, s)
	if sampleSegment(first.CPU.Sample) == sampleSegment(second.CPU.Sample) {
		t.Fatal("long gap did not break graph segment")
	}
	now = now.Add(2 * time.Second)
	c.counts.idle -= 1
	c.counts.user += 20
	reset := readSample(t, s)
	if reset.CPU.Sample != second.CPU.Sample || reset.CPU.Problem == "" {
		t.Fatalf("counter decrease represented as fresh: %+v", reset.CPU)
	}
	now = now.Add(2 * time.Second)
	c.counts.user += 2
	c.counts.idle += 2
	third := readSample(t, s)
	if sampleSegment(second.CPU.Sample) == sampleSegment(third.CPU.Sample) {
		t.Fatal("counter reset did not break graph segment")
	}
}

func sampleSegment(sample string) string { return sample[:strings.LastIndex(sample, "/")] }

func TestUtilizationCountsIdleIOWaitAndNoGuestTwice(t *testing.T) {
	percent, valid := utilization(counters{}, counters{user: 20, nice: 10, system: 10, idle: 50, iowait: 10})
	if !valid || math.Abs(percent-40) > 0.001 {
		t.Fatalf("percentage = %v, valid %v", percent, valid)
	}
	_, valid = utilization(counters{idle: 50}, counters{user: 100, idle: 49})
	if valid {
		t.Fatal("individual decreasing counter accepted despite increasing total")
	}
}

func TestSamplerConcurrentReadersCoalesce(t *testing.T) {
	c := goodCollector()
	now := time.Now()
	s := newSampler(c, func() time.Time { return now })
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			_, err := s.Read(context.Background())
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if c.calls != 1 || c.sensors != 1 {
		t.Fatalf("shared reads collected %d CPUs and %d sensors", c.calls, c.sensors)
	}
}

func TestSamplerCancellationDoesNotWaitForGate(t *testing.T) {
	c := goodCollector()
	c.block, c.entered = make(chan struct{}), make(chan struct{})
	entered := c.entered
	s := newSampler(c, time.Now)
	done := make(chan error, 1)
	go func() { _, err := s.Read(context.Background()); done <- err }()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Read(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("blocked read ignored cancellation: %v", err)
	}
	close(c.block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSamplerUnsupportedAndInvalidReadings(t *testing.T) {
	c := goodCollector()
	c.temperatureErr = ErrUnsupported
	c.counts.user = math.NaN()
	c.memory.AvailableBytes = 101
	s := newSampler(c, time.Now)
	v := readSample(t, s)
	if v.Temperature.State != Unsupported || v.CPU.State != Unavailable || v.Memory.State != Unavailable {
		t.Fatalf("invalid or unsupported reading = %+v", v)
	}
}
