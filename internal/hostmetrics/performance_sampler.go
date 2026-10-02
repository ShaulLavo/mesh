package hostmetrics

import (
	"context"
	"errors"
	"math"
	"time"
)

type performanceState struct {
	temperatureSample                         string
	temperaturesFailing                       bool
	gpu                                       cached[GPU]
	disk                                      cached[Disk]
	network                                   cached[Network]
	cores                                     cached[[]float64]
	battery                                   cached[Battery]
	temperatures                              map[string]cached[ComponentTemperature]
	diskBefore                                DiskCounters
	networkBefore                             NetworkCounters
	coresBefore                               []Counters
	diskAt, networkAt, coresAt                time.Time
	diskSegment, networkSegment, coresSegment uint64
}

func (s *Sampler) collectPerformance(ctx context.Context, sensors bool) {
	collector, ok := s.collector.(performanceCollector)
	if !ok {
		return
	}
	p := &s.performance
	if p.temperatures == nil {
		p.temperatures = make(map[string]cached[ComponentTemperature])
		p.gpu = empty[GPU]()
		p.disk = empty[Disk]()
		p.network = empty[Network]()
		p.cores = empty[[]float64]()
		p.battery = empty[Battery]()
	}
	s.collectDisk(ctx, collector)
	s.collectNetwork(ctx, collector)
	s.collectCores(ctx, collector)
	if !sensors {
		return
	}
	gpu, err := collector.GPU(ctx)
	if err == nil && (!finite(gpu.Utilization) || gpu.Utilization < 0 || gpu.Utilization > 100 || gpu.MemoryKind != "shared" && gpu.MemoryUsedBytes > gpu.MemoryTotalBytes) {
		err = errors.New("invalid GPU counters")
	}
	p.gpu = record(p.gpu, gpu, err, s.now(), s.key(), 0)
	battery, err := collector.Battery(ctx)
	if err == nil && (!finite(battery.Percent) || battery.Percent < 0 || battery.Percent > 100 || !validBatteryState(battery.State) || battery.SecondsRemaining > 7*86400) {
		err = errors.New("invalid battery counters")
	}
	p.battery = record(p.battery, battery, err, s.now(), s.key(), 0)
	s.collectTemperatures(ctx, collector)
}
func (s *Sampler) collectTemperatures(ctx context.Context, collector performanceCollector) {
	p := &s.performance
	values, err := collector.Components(ctx)
	p.temperaturesFailing = err != nil
	if err != nil {
		return
	}
	now := s.now()
	if len(values) > 0 {
		p.temperatureSample = s.key()
	}
	for _, value := range HottestTemperatures(values) {
		if value.AgeMillis < 0 {
			continue
		}
		at := now.Add(-time.Duration(value.AgeMillis) * time.Millisecond)
		old := p.temperatures[value.Kind]
		if !old.at.IsZero() && !at.After(old.at) {
			continue
		}
		value.AgeMillis = 0
		p.temperatures[value.Kind] = record(old, value, nil, at, s.key(), 0)
	}
}
func validBatteryState(state string) bool {
	return state == BatteryCharging || state == BatteryDischarging || state == BatteryFull || state == BatteryAC
}
func continuous(at, now time.Time) bool {
	return !at.IsZero() && now.Sub(at) > 0 && now.Sub(at) <= BaselineGap
}
func (s *Sampler) collectDisk(ctx context.Context, c performanceCollector) {
	p := &s.performance
	current, err := c.Disk(ctx)
	now := s.now()
	if err != nil {
		p.disk = record(p.disk, Disk{}, err, now, "", p.diskSegment)
		return
	}
	before := p.diskBefore
	elapsed := now.Sub(p.diskAt)
	good := continuous(p.diskAt, now) && current.Devices == before.Devices && current.ReadBytes >= before.ReadBytes && current.WriteBytes >= before.WriteBytes && current.BusyMillis >= before.BusyMillis
	p.diskBefore = current
	p.diskAt = now
	if !good {
		p.diskSegment++
		p.disk = record(p.disk, Disk{}, errors.New("disk baseline warming up"), now, "", p.diskSegment)
		return
	}
	value := Disk{ReadBytesPerSecond: math.Round(float64(current.ReadBytes-before.ReadBytes) / elapsed.Seconds()), WriteBytesPerSecond: math.Round(float64(current.WriteBytes-before.WriteBytes) / elapsed.Seconds()), BusyAvailable: current.BusyAvailable}
	if current.BusyAvailable {
		value.BusyPercent = math.Round(min(100, float64(current.BusyMillis-before.BusyMillis)/(elapsed.Seconds()*1000)*100))
	}
	p.disk = record(p.disk, value, nil, now, s.key(), p.diskSegment)
}
func (s *Sampler) collectNetwork(ctx context.Context, c performanceCollector) {
	p := &s.performance
	current, err := c.Network(ctx)
	now := s.now()
	if err != nil {
		p.network = record(p.network, Network{}, err, now, "", p.networkSegment)
		return
	}
	before := p.networkBefore
	elapsed := now.Sub(p.networkAt)
	good := continuous(p.networkAt, now) && current.Devices == before.Devices && current.ReceiveBytes >= before.ReceiveBytes && current.SendBytes >= before.SendBytes
	p.networkBefore = current
	p.networkAt = now
	if !good {
		p.networkSegment++
		p.network = record(p.network, Network{}, errors.New("network baseline warming up"), now, "", p.networkSegment)
		return
	}
	value := Network{ReceiveBytesPerSecond: math.Round(float64(current.ReceiveBytes-before.ReceiveBytes) / elapsed.Seconds()), SendBytesPerSecond: math.Round(float64(current.SendBytes-before.SendBytes) / elapsed.Seconds())}
	p.network = record(p.network, value, nil, now, s.key(), p.networkSegment)
}
func (s *Sampler) collectCores(ctx context.Context, c performanceCollector) {
	p := &s.performance
	current, err := c.Cores(ctx)
	now := s.now()
	if err == nil && (len(current) == 0 || len(current) > MaximumCores) {
		err = errors.New("invalid per-core counter count")
	}
	if err != nil {
		p.cores = record(p.cores, nil, err, now, "", p.coresSegment)
		return
	}
	before := p.coresBefore
	good := continuous(p.coresAt, now) && len(before) == len(current)
	values := make([]float64, len(current))
	if good {
		for i := range current {
			value, ok := Utilization(before[i], current[i])
			good = good && ok
			// Dashboard percentages have whole-percent resolution; counters stay exact for the next delta.
			values[i] = math.Round(value)
		}
	}
	p.coresBefore = append([]Counters(nil), current...)
	p.coresAt = now
	if !good {
		p.coresSegment++
		p.cores = record(p.cores, nil, errors.New("per-core CPU baseline warming up"), now, "", p.coresSegment)
		return
	}
	p.cores = record(p.cores, values, nil, now, s.key(), p.coresSegment)
}
func (s *Sampler) snapshot(now time.Time) Snapshot {
	value := Snapshot{CPU: aged(s.cpu, now), RAM: aged(s.ram, now), Temperature: aged(s.temperature, now), Uptime: aged(s.uptime, now)}
	if _, ok := s.collector.(performanceCollector); !ok {
		return value
	}
	p := s.performance
	value.PerformanceVersion = PerformanceVersion
	value.TemperaturesSample = p.temperatureSample
	value.TemperaturesFailing = p.temperaturesFailing
	value.GPU = optionalReading(p.gpu, now)
	value.Disk = optionalReading(p.disk, now)
	value.Network = optionalReading(p.network, now)
	value.Cores = optionalReading(p.cores, now)
	value.Battery = optionalReading(p.battery, now)
	if value.Cores != nil {
		value.Cores.Value = append([]float64(nil), value.Cores.Value...)
	}
	for _, kind := range []string{KindCPU, KindGPU, KindSoC, KindNVMe, KindRAM} {
		if cached, ok := p.temperatures[kind]; ok {
			entry := cached.reading.Value
			entry.AgeMillis = max(0, now.Sub(cached.at).Milliseconds())
			value.Temperatures = append(value.Temperatures, entry)
		}
	}
	return value
}
func optionalReading[T any](value cached[T], now time.Time) *Reading[T] {
	if value.reading.Availability == "" || value.reading.Availability == Unsupported && value.reading.Sample == "" {
		return nil
	}
	reading := aged(value, now)
	return &reading
}
