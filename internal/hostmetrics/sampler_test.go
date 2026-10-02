package hostmetrics

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeCollector struct {
	calls      int
	cpu        Counters
	memoryFail bool
}

func (f *fakeCollector) CPU(context.Context) (Counters, error) {
	f.calls++
	f.cpu.User += 10
	f.cpu.Idle += 10
	return f.cpu, nil
}
func (f *fakeCollector) Memory(context.Context) (Memory, error) {
	if f.memoryFail {
		return Memory{}, errors.New("memory read failed")
	}
	return Memory{TotalBytes: 100, AvailableBytes: 40, Estimate: "OS available memory"}, nil
}
func (*fakeCollector) Temperature(context.Context) (Temperature, error) {
	return Temperature{Sensor: "cpu-thermal", Celsius: 42}, nil
}
func (*fakeCollector) Uptime(context.Context) (uint64, error) { return 100, nil }

func TestSamplerCadenceIndependentFailuresAndBaseline(t *testing.T) {
	now := time.Now()
	f := &fakeCollector{}
	s := NewWithCollector(f, func() time.Time { return now })
	first, err := s.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.CPU.Availability != Unavailable || first.RAM.Value.UsedBytes() != 60 {
		t.Fatalf("first %+v", first)
	}
	now = now.Add(2 * time.Second)
	good, _ := s.Read(context.Background())
	if good.CPU.Value != 50 || good.CPU.Sample == "" {
		t.Fatalf("CPU %+v", good.CPU)
	}
	f.memoryFail = true
	now = now.Add(2 * time.Second)
	failed, _ := s.Read(context.Background())
	if !failed.RAM.Failing || failed.RAM.Sample != good.RAM.Sample || failed.RAM.AgeMillis != 2000 || failed.CPU.AgeMillis != 0 {
		t.Fatalf("independent ages %+v", failed)
	}
	now = now.Add(BaselineGap + time.Second)
	reset, _ := s.Read(context.Background())
	if !reset.CPU.Failing || reset.CPU.Sample != good.CPU.Sample && reset.CPU.AgeMillis == 0 {
		t.Fatalf("long gap %+v", reset.CPU)
	}
}
func TestConcurrentReadsCoalesce(t *testing.T) {
	f := &fakeCollector{}
	s := NewWithCollector(f, time.Now)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			_, err := s.Read(context.Background())
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if f.calls != 1 {
		t.Fatalf("collections=%d", f.calls)
	}
}
func TestSlowestDemandAndRelease(t *testing.T) {
	s := NewWithCollector(&fakeCollector{}, time.Now)
	releaseA := s.Demand(2 * time.Second)
	releaseB := s.Demand(8 * time.Second)
	if s.Interval() != 8*time.Second {
		t.Fatal(s.Interval())
	}
	releaseB()
	if s.Interval() != 2*time.Second {
		t.Fatal(s.Interval())
	}
	releaseA()
	releaseA()
	if s.Interval() != 0 {
		t.Fatal(s.Interval())
	}
}
func TestUtilizationCounterResetAndIOWait(t *testing.T) {
	v, ok := Utilization(Counters{}, Counters{User: 20, Idle: 10, IOWait: 10})
	if !ok || v != 50 {
		t.Fatal(v, ok)
	}
	if _, ok := Utilization(Counters{User: 20}, Counters{User: 10}); ok {
		t.Fatal("counter reset accepted")
	}
}

func TestSlowestCadenceCPUAndDemandLoopStandDown(t *testing.T) {
	now := time.Now()
	sampler := NewWithCollector(&fakeCollector{}, func() time.Time { return now })
	if _, err := sampler.Read(t.Context()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(MaximumInterval + time.Millisecond)
	value, err := sampler.Read(t.Context())
	if err != nil || value.CPU.Availability != Available || value.CPU.Failing {
		t.Fatal("slowest cadence reset CPU baseline", value.CPU, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	active := NewWithCollector(&fakeCollector{}, time.Now)
	samples := make(chan Snapshot, 3)
	done := make(chan struct{})
	go func() { defer close(done); active.Run(ctx, func(value Snapshot) { samples <- value }) }()
	select {
	case <-samples:
		t.Fatal("no demand sampled")
	case <-time.After(100 * time.Millisecond):
	}
	release := active.Demand(MinimumInterval)
	select {
	case <-samples:
	case <-time.After(3 * time.Second):
		t.Fatal("demand did not sample")
	}
	release()
	select {
	case <-samples:
		t.Fatal("sampling continued after release")
	case <-time.After(MinimumInterval + 100*time.Millisecond):
	}
	cancel()
	<-done
}

func TestMetricsDemandChurnDoesNotPostponeSampling(t *testing.T) {
	sampler := NewWithCollector(&fakeCollector{}, time.Now)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	samples := make(chan Snapshot, 1)
	release := sampler.Demand(MinimumInterval)
	defer release()
	go func() {
		defer close(done)
		sampler.Run(ctx, func(snapshot Snapshot) {
			select {
			case samples <- snapshot:
			default:
			}
		})
	}()
	defer func() { cancel(); <-done }()
	churn := time.NewTicker(100 * time.Millisecond)
	defer churn.Stop()
	deadline := time.NewTimer(MinimumInterval + time.Second)
	defer deadline.Stop()
	for {
		select {
		case <-churn.C:
			extra := sampler.Demand(MinimumInterval)
			extra()
		case <-samples:
			return
		case <-deadline.C:
			t.Fatal("unchanged metric demand repeatedly postponed the sample")
		}
	}
}
