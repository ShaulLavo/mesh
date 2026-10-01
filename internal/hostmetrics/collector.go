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
		return err
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return ErrUnsupported
	}
	return nil
}

func (systemCollector) CPU(ctx context.Context) (counters, error) {
	if err := supported(ctx); err != nil {
		return counters{}, err
	}
	values, err := cpu.TimesWithContext(ctx, false)
	if err != nil {
		return counters{}, err
	}
	if len(values) != 1 {
		return counters{}, fmt.Errorf("expected aggregate CPU counters, got %d", len(values))
	}
	v := values[0]
	return counters{user: v.User, nice: v.Nice, system: v.System, idle: v.Idle, iowait: v.Iowait, irq: v.Irq, softirq: v.Softirq, steal: v.Steal}, nil
}

func (systemCollector) Memory(ctx context.Context) (MemoryUsage, error) {
	if err := supported(ctx); err != nil {
		return MemoryUsage{}, err
	}
	v, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil {
		return MemoryUsage{}, err
	}
	// Available uses Linux MemAvailable or Darwin free+inactive pages. UsedPercent
	// has different semantics and is deliberately not used for dashboard RAM.
	return MemoryUsage{TotalBytes: v.Total, AvailableBytes: v.Available}, nil
}

func (systemCollector) Uptime(ctx context.Context) (uint64, error) {
	if err := supported(ctx); err != nil {
		return 0, err
	}
	return host.UptimeWithContext(ctx)
}
