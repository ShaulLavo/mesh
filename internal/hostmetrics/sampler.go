package hostmetrics

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"time"
)

const (
	Available   = "available"
	Unavailable = "unavailable"
	Unsupported = "unsupported"
	sampleEvery = 2 * time.Second
	sensorEvery = 10 * time.Second
	baselineGap = 10 * time.Second
)

var ErrUnsupported = errors.New("host metric unsupported")

type Reading[T any] struct {
	State     string
	Value     T
	Sample    string
	AgeMillis int64
	Problem   string
}

type MemoryUsage struct{ TotalBytes, AvailableBytes uint64 }
type Temperature struct {
	Sensor  string
	Celsius float64
}
type Snapshot struct {
	CPU         Reading[float64]
	Memory      Reading[MemoryUsage]
	Temperature Reading[Temperature]
	Uptime      Reading[uint64]
}

// Counters excludes guest fields: Linux already includes them in user/nice.
type counters struct{ user, nice, system, idle, iowait, irq, softirq, steal float64 }

type collector interface {
	CPU(context.Context) (counters, error)
	Memory(context.Context) (MemoryUsage, error)
	Temperature(context.Context) (Temperature, error)
	Uptime(context.Context) (uint64, error)
}

type cached[T any] struct {
	reading Reading[T]
	at      time.Time
}

// Sampler coalesces demand from every client without a background sampling loop.
type Sampler struct {
	gate       chan struct{}
	collector  collector
	now        func() time.Time
	instance   string
	sequence   uint64
	segment    uint64
	last       time.Time
	sensorAt   time.Time
	baseline   counters
	baselineAt time.Time
	cpu        cached[float64]
	memory     cached[MemoryUsage]
	sensor     cached[Temperature]
	uptime     cached[uint64]
}

func New() *Sampler { return newSampler(systemCollector{}, time.Now) }

func newSampler(c collector, now func() time.Time) *Sampler {
	return &Sampler{
		gate: make(chan struct{}, 1), collector: c, now: now, instance: rand.Text(),
		cpu: empty[float64](), memory: empty[MemoryUsage](),
		sensor: empty[Temperature](), uptime: empty[uint64](),
	}
}

func empty[T any]() cached[T] { return cached[T]{reading: Reading[T]{State: Unavailable}} }

func (s *Sampler) Read(ctx context.Context) (Snapshot, error) {
	if ctx == nil {
		return Snapshot{}, errors.New("host metrics: nil context")
	}
	select {
	case s.gate <- struct{}{}:
	case <-ctx.Done():
		return Snapshot{}, ctx.Err()
	}
	defer func() { <-s.gate }()
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	now := s.now()
	if s.last.IsZero() || now.Sub(s.last) >= sampleEvery {
		s.sample(ctx)
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	now = s.now()
	return Snapshot{CPU: aged(s.cpu, now), Memory: aged(s.memory, now), Temperature: aged(s.sensor, now), Uptime: aged(s.uptime, now)}, nil
}

func (s *Sampler) sample(ctx context.Context) {
	s.last = s.now()
	s.sampleCPU(ctx)
	value, err := s.collector.Memory(ctx)
	if err == nil && (value.TotalBytes == 0 || value.AvailableBytes > value.TotalBytes) {
		err = errors.New("invalid memory counters")
	}
	s.memory = record(s.memory, value, err, s.now(), s.key("memory"))
	uptime, err := s.collector.Uptime(ctx)
	s.uptime = record(s.uptime, uptime, err, s.now(), s.key("uptime"))
	if s.sensorAt.IsZero() || s.now().Sub(s.sensorAt) >= sensorEvery {
		s.sampleTemperature(ctx)
	}
}

func (s *Sampler) sampleTemperature(ctx context.Context) {
	s.sensorAt = s.now()
	value, err := s.collector.Temperature(ctx)
	if err == nil && (value.Sensor == "" || !isFinite(value.Celsius) || value.Celsius < -273.15 || value.Celsius > 1000) {
		err = errors.New("invalid temperature reading")
	}
	s.sensor = record(s.sensor, value, err, s.now(), s.key("temperature"))
}

func (s *Sampler) sampleCPU(ctx context.Context) {
	current, err := s.collector.CPU(ctx)
	if err == nil && !current.valid() {
		err = errors.New("invalid CPU counters")
	}
	if err != nil {
		s.cpu = record(s.cpu, 0, err, s.now(), "")
		return
	}
	now := s.now()
	percent, continuous := utilization(s.baseline, current)
	continuous = continuous && !s.baselineAt.IsZero() && now.Sub(s.baselineAt) <= baselineGap
	s.baseline, s.baselineAt = current, now
	if !continuous {
		s.segment++
		s.cpu = record(s.cpu, 0, errors.New("CPU baseline warming up"), now, "")
		return
	}
	s.cpu = record(s.cpu, percent, nil, now, s.key("cpu"))
}

func (s *Sampler) key(metric string) string {
	s.sequence++
	segment := uint64(0)
	if metric == "cpu" {
		segment = s.segment
	}
	return fmt.Sprintf("%s/%d/%d", s.instance, segment, s.sequence)
}

func record[T any](old cached[T], value T, err error, now time.Time, sample string) cached[T] {
	if err == nil {
		return cached[T]{reading: Reading[T]{State: Available, Value: value, Sample: sample}, at: now}
	}
	old.reading.Problem = err.Error()
	if len(old.reading.Problem) > 1024 {
		old.reading.Problem = old.reading.Problem[:1024]
	}
	if old.reading.Sample == "" && errors.Is(err, ErrUnsupported) {
		old.reading.State = Unsupported
	}
	return old
}

func aged[T any](value cached[T], now time.Time) Reading[T] {
	result := value.reading
	if !value.at.IsZero() {
		result.AgeMillis = max(0, now.Sub(value.at).Milliseconds())
	}
	return result
}

func (c counters) values() [8]float64 {
	return [8]float64{c.user, c.nice, c.system, c.idle, c.iowait, c.irq, c.softirq, c.steal}
}

func (c counters) valid() bool {
	for _, value := range c.values() {
		if !isFinite(value) || value < 0 {
			return false
		}
	}
	return true
}

func utilization(previous, current counters) (float64, bool) {
	before, after := previous.values(), current.values()
	var total float64
	for index, value := range after {
		if value < before[index] {
			return 0, false
		}
		total += value - before[index]
	}
	if total <= 0 {
		return 0, false
	}
	idle := current.idle - previous.idle + current.iowait - previous.iowait
	return min(100, max(0, (total-idle)/total*100)), true
}

func isFinite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }
