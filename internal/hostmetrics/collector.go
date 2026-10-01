package hostmetrics

import (
	"context"
	"fmt"
	"runtime"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
)

type systemCollector struct{}

func supported(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("host metrics context: %w", err)
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return ErrUnsupported
	}
	return nil
}
func (systemCollector) CPU(ctx context.Context) (Counters, error) {
	if err := supported(ctx); err != nil {
		return Counters{}, fmt.Errorf("host CPU counters: %w", err)
	}
	values, err := cpu.TimesWithContext(ctx, false)
	if err != nil {
		return Counters{}, fmt.Errorf("host CPU counters: %w", err)
	}
	if len(values) != 1 {
		return Counters{}, fmt.Errorf("aggregate CPU counter count %d", len(values))
	}
	v := values[0]
	return Counters{User: v.User, Nice: v.Nice, System: v.System, Idle: v.Idle, IOWait: v.Iowait, IRQ: v.Irq, SoftIRQ: v.Softirq, Steal: v.Steal}, nil
}
func (systemCollector) Memory(ctx context.Context) (Memory, error) {
	if err := supported(ctx); err != nil {
		return Memory{}, fmt.Errorf("host available memory: %w", err)
	}
	v, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil {
		return Memory{}, fmt.Errorf("host available memory: %w", err)
	}
	estimate := "Linux MemAvailable estimate"
	if runtime.GOOS == "darwin" {
		estimate = "Darwin free and inactive page estimate"
	}
	return Memory{TotalBytes: v.Total, AvailableBytes: v.Available, Estimate: estimate}, nil
}
func (systemCollector) Uptime(ctx context.Context) (uint64, error) {
	if err := supported(ctx); err != nil {
		return 0, err
	}
	value, err := host.UptimeWithContext(ctx)
	if err != nil {
		return 0, fmt.Errorf("host uptime: %w", err)
	}
	return value, nil
}
