package cli

import (
	"context"
	"github.com/shaul/mesh/internal/hostmetrics"
	"github.com/shaul/mesh/internal/privacy"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/usagefeed"
	"time"
)

type DashboardFunc func(context.Context, DashboardInput) error

// DashboardWatch publishes owned values serially and joins its readers before returning.
type DashboardWatch func(context.Context, func(DashboardHostView)) error

type DashboardInput struct {
	Notice     string
	Privacy    *privacy.Mask
	Hosts      []DashboardHost
	Wall       bool
	Theme      string
	Watch      DashboardWatch
	UsageWatch func(context.Context, func(usagefeed.Result)) error
	Inspect    PickerInspectFunc
}

type DashboardHost struct {
	ID, MachineName string
	Local           bool
	NameVerified    bool
	NameConflict    bool
	NamePriority    bool
	NameSuffix      string
	NameRevision    uint64
}
type DashboardMeasurement[T any] struct {
	State      string
	Value      T
	Sample     string
	Segment    uint64
	MeasuredAt time.Time
	Failing    bool
	Problem    string
}
type DashboardMemory struct {
	TotalBytes, AvailableBytes uint64
	Estimate                   string
}
type DashboardTemperature struct {
	Sensor  string
	Celsius float64
}
type DashboardSession struct{ ID, Name, Label, State, Command string }
type DashboardService struct {
	Name, State, Problem string
	Failed               bool
	HealthUnknown        bool
}
type DashboardCatalog[T any] struct {
	Rows                         []T
	Total                        int
	Ready, Failed, Idle, Unknown int // Service state totals are counted before row bounding.
	ObservedAt                   time.Time
	Failing                      bool
}

func (c DashboardCatalog[T]) Stale(now, lastReply time.Time) bool {
	return c.Failing || c.ObservedAt.IsZero() || lastReply.IsZero() || now.Sub(c.ObservedAt) >= 30*time.Second || now.Sub(lastReply) >= 30*time.Second
}

type DashboardHostView struct {
	Notice             string
	Build              release.Build
	Host               DashboardHost
	Connection         StateConnection
	Problem            string
	MetricsUnsupported bool
	LastReply          time.Time
	NameObservedAt     time.Time
	NameFailing        bool
	CPU                DashboardMeasurement[float64]
	RAM                DashboardMeasurement[DashboardMemory]
	Temperature        DashboardMeasurement[DashboardTemperature]
	Battery            *DashboardMeasurement[hostmetrics.Battery]
	PerformanceVersion int
	Temperatures       []DashboardMeasurement[hostmetrics.ComponentTemperature]
	GPU                *DashboardMeasurement[hostmetrics.GPU]
	Disk               *DashboardMeasurement[hostmetrics.Disk]
	Network            *DashboardMeasurement[hostmetrics.Network]
	Cores              *DashboardMeasurement[[]float64]
	Uptime             DashboardMeasurement[uint64]
	Sessions           DashboardCatalog[DashboardSession]
	Services           DashboardCatalog[DashboardService]
}

func (h DashboardHostView) NameLabel(now time.Time) string {
	record := HostRecord{ID: h.Host.ID, MachineName: h.Host.MachineName, NameConflict: h.Host.NameConflict, NamePriority: h.Host.NamePriority, NameSuffix: h.Host.NameSuffix}
	label := HostLabel(record)
	if h.Host.MachineName != "" && (!h.Host.NameVerified || h.Connection != StateReachable || h.NameFailing || h.NameObservedAt.IsZero() || now.Sub(h.NameObservedAt) >= 30*time.Second || now.Sub(h.LastReply) >= 30*time.Second) {
		label += retainedNameSuffix
	}
	return label
}

func (h DashboardHost) Label() string {
	return HostLabel(HostRecord{ID: h.ID, MachineName: h.MachineName, NameConflict: h.NameConflict, NamePriority: h.NamePriority, NameSuffix: h.NameSuffix})
}
