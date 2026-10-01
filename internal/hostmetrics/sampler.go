// Package hostmetrics owns independent last-good host measurements and shared demand.
package hostmetrics

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
)

const (
	Available           = "available"
	Unavailable         = "unavailable"
	Unsupported         = "unsupported"
	MinimumInterval     = 2 * time.Second
	MaximumInterval     = 10 * time.Second
	TemperatureInterval = 10 * time.Second
	BaselineGap         = 2 * MaximumInterval
)

var ErrUnsupported = errors.New("host metric unsupported")

type Reading[T any] struct {
	Availability string `json:"availability"`
	Value        T      `json:"value"`
	Sample       string `json:"sample,omitempty"`
	Segment      uint64 `json:"segment,omitempty"`
	AgeMillis    int64  `json:"ageMillis"`
	Failing      bool   `json:"failing,omitempty"`
	Problem      string `json:"problem,omitempty"`
}
type Memory struct {
	TotalBytes     uint64 `json:"totalBytes"`
	AvailableBytes uint64 `json:"availableBytes"`
	Estimate       string `json:"estimate"`
}

func (m Memory) UsedBytes() uint64 { return m.TotalBytes - m.AvailableBytes }

type Temperature struct {
	Sensor  string  `json:"sensor"`
	Celsius float64 `json:"celsius"`
}
type Snapshot struct {
	CPU         Reading[float64]     `json:"cpu"`
	RAM         Reading[Memory]      `json:"ram"`
	Temperature Reading[Temperature] `json:"temperature"`
	Uptime      Reading[uint64]      `json:"uptime"`
}

// Counters excludes guest, which is already included in Linux user/nice counters.
type Counters struct{ User, Nice, System, Idle, IOWait, IRQ, SoftIRQ, Steal float64 }
type Collector interface {
	CPU(context.Context) (Counters, error)
	Memory(context.Context) (Memory, error)
	Temperature(context.Context) (Temperature, error)
	Uptime(context.Context) (uint64, error)
}
type cached[T any] struct {
	reading Reading[T]
	at      time.Time
}
type Sampler struct {
	gate                       chan struct{}
	collector                  Collector
	now                        func() time.Time
	instance                   string
	sequence, segment          uint64
	last, sensorAt, baselineAt time.Time
	baseline                   Counters
	cpu                        cached[float64]
	ram                        cached[Memory]
	temperature                cached[Temperature]
	uptime                     cached[uint64]
	mu                         sync.Mutex
	demands                    map[uint64]time.Duration
	next                       uint64
	changed                    chan struct{}
}

func New() *Sampler { return NewWithCollector(systemCollector{}, time.Now) }
func NewWithCollector(c Collector, now func() time.Time) *Sampler {
	return &Sampler{gate: make(chan struct{}, 1), collector: c, now: now, instance: rand.Text(), demands: make(map[uint64]time.Duration), changed: make(chan struct{}, 1), cpu: empty[float64](), ram: empty[Memory](), temperature: empty[Temperature](), uptime: empty[uint64]()}
}
func empty[T any]() cached[T] { return cached[T]{reading: Reading[T]{Availability: Unavailable}} }
func (s *Sampler) Demand(every time.Duration) func() {
	every = max(MinimumInterval, min(MaximumInterval, every))
	s.mu.Lock()
	s.next++
	id := s.next
	s.demands[id] = every
	s.mu.Unlock()
	s.notify()
	var once sync.Once
	return func() { once.Do(func() { s.mu.Lock(); delete(s.demands, id); s.mu.Unlock(); s.notify() }) }
}
func (s *Sampler) notify() {
	select {
	case s.changed <- struct{}{}:
	default:
	}
}
func (s *Sampler) Interval() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	var every time.Duration
	for _, requested := range s.demands {
		every = max(every, requested)
	}
	return every
}

// Run owns the single demand timer. Its publisher must enqueue without blocking.
func (s *Sampler) Run(ctx context.Context, publish func(Snapshot)) {
	var timer *time.Timer
	var tick <-chan time.Time
	var scheduled time.Duration
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	reset := func() {
		if timer != nil {
			timer.Stop()
		}
		tick = nil
		scheduled = s.Interval()
		if scheduled > 0 {
			timer = time.NewTimer(scheduled)
			tick = timer.C
		}
	}
	reset()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.changed:
			if s.Interval() != scheduled {
				reset()
			}
		case <-tick:
			s.publishSample(ctx, publish)
			reset()
		}
	}
}
func (s *Sampler) Read(ctx context.Context) (Snapshot, error) {
	if ctx == nil {
		return Snapshot{}, errors.New("host metrics: nil context")
	}
	select {
	case s.gate <- struct{}{}:
	case <-ctx.Done():
		return Snapshot{}, fmt.Errorf("host metrics context: %w", ctx.Err())
	}
	defer func() { <-s.gate }()
	if err := ctx.Err(); err != nil {
		return Snapshot{}, fmt.Errorf("host metrics context: %w", err)
	}
	now := s.now()
	if s.last.IsZero() || now.Sub(s.last) >= MinimumInterval {
		s.collect(ctx)
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, fmt.Errorf("host metrics context: %w", err)
	}
	now = s.now()
	return Snapshot{CPU: aged(s.cpu, now), RAM: aged(s.ram, now), Temperature: aged(s.temperature, now), Uptime: aged(s.uptime, now)}, nil
}
func (s *Sampler) collect(ctx context.Context) {
	s.last = s.now()
	s.collectCPU(ctx)
	memory, err := s.collector.Memory(ctx)
	if err == nil && (memory.TotalBytes == 0 || memory.AvailableBytes > memory.TotalBytes || memory.Estimate == "") {
		err = errors.New("invalid OS available memory estimate")
	}
	s.ram = record(s.ram, memory, err, s.now(), s.key(), 0)
	uptime, err := s.collector.Uptime(ctx)
	s.uptime = record(s.uptime, uptime, err, s.now(), s.key(), 0)
	if s.sensorAt.IsZero() || s.now().Sub(s.sensorAt) >= TemperatureInterval {
		s.sensorAt = s.now()
		temperature, err := s.collector.Temperature(ctx)
		if err == nil && (temperature.Sensor == "" || !finite(temperature.Celsius) || temperature.Celsius < -273.15 || temperature.Celsius > 1000) {
			err = errors.New("invalid CPU temperature")
		}
		s.temperature = record(s.temperature, temperature, err, s.now(), s.key(), 0)
	}
}
func (s *Sampler) collectCPU(ctx context.Context) {
	current, err := s.collector.CPU(ctx)
	now := s.now()
	if err == nil && !current.valid() {
		err = errors.New("invalid CPU counters")
	}
	if err != nil {
		s.cpu = record(s.cpu, 0, err, now, "", s.segment)
		return
	}
	value, continuous := Utilization(s.baseline, current)
	continuous = continuous && !s.baselineAt.IsZero() && now.Sub(s.baselineAt) > 0 && now.Sub(s.baselineAt) <= BaselineGap
	s.baseline = current
	s.baselineAt = now
	if !continuous {
		s.segment++
		s.cpu = record(s.cpu, 0, errors.New("CPU baseline warming up"), now, "", s.segment)
		return
	}
	s.cpu = record(s.cpu, value, nil, now, s.key(), s.segment)
}
func (s *Sampler) key() string { s.sequence++; return fmt.Sprintf("%s/%d", s.instance, s.sequence) }
func record[T any](old cached[T], value T, err error, now time.Time, key string, segment uint64) cached[T] {
	if err == nil {
		return cached[T]{reading: Reading[T]{Availability: Available, Value: value, Sample: key, Segment: segment}, at: now}
	}
	old.reading.Failing = true
	old.reading.Problem = err.Error()
	if len(old.reading.Problem) > 256 {
		old.reading.Problem = old.reading.Problem[:256]
	}
	if old.reading.Sample == "" && errors.Is(err, ErrUnsupported) {
		old.reading.Availability = Unsupported
		old.reading.Failing = false
	}
	return old
}
func aged[T any](value cached[T], now time.Time) Reading[T] {
	r := value.reading
	if !value.at.IsZero() {
		r.AgeMillis = max(0, now.Sub(value.at).Milliseconds())
	}
	return r
}
func (c Counters) values() [8]float64 {
	return [8]float64{c.User, c.Nice, c.System, c.Idle, c.IOWait, c.IRQ, c.SoftIRQ, c.Steal}
}
func (c Counters) valid() bool {
	for _, v := range c.values() {
		if !finite(v) || v < 0 {
			return false
		}
	}
	return true
}
func Utilization(before, after Counters) (float64, bool) {
	if !before.valid() || !after.valid() {
		return 0, false
	}
	a, b := before.values(), after.values()
	var total float64
	for i, v := range b {
		if v < a[i] {
			return 0, false
		}
		total += v - a[i]
	}
	if total <= 0 {
		return 0, false
	}
	idle := after.Idle - before.Idle + after.IOWait - before.IOWait
	return min(100, max(0, (total-idle)/total*100)), true
}
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func (s *Sampler) publishSample(ctx context.Context, publish func(Snapshot)) {
	if s.Interval() == 0 {
		return
	}
	sampleCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	value, err := s.Read(sampleCtx)
	cancel()
	if err != nil {
		value, err = s.current(ctx)
	}
	if err == nil && ctx.Err() == nil && s.Interval() > 0 {
		publish(value)
	}
}
func (s *Sampler) current(ctx context.Context) (Snapshot, error) {
	select {
	case s.gate <- struct{}{}:
	case <-ctx.Done():
		return Snapshot{}, fmt.Errorf("host metrics context: %w", ctx.Err())
	}
	defer func() { <-s.gate }()
	now := s.now()
	return Snapshot{CPU: aged(s.cpu, now), RAM: aged(s.ram, now), Temperature: aged(s.temperature, now), Uptime: aged(s.uptime, now)}, nil
}
