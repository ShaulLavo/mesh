package cli

import (
	"context"
	"time"

	"github.com/shaul/mesh/internal/hostmetrics"
	"github.com/shaul/mesh/internal/privacy"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/usagefeed"
)

type DashboardFunc func(context.Context, DashboardInput) error

// DashboardWatch publishes owned values serially and joins its readers before returning.
type DashboardWatch func(context.Context, func(DashboardHostView)) error

type DashboardInput struct {
	ConfigError      error
	ConfigWatch      func(context.Context, func(error)) error
	Notice           string
	Privacy          *privacy.Mask
	Hosts            []DashboardHost
	Wall             bool
	Theme            string
	Watch            DashboardWatch
	UpdateWatch      func(context.Context, func(DashboardUpdates)) error
	UsageWatch       func(context.Context, func(DashboardUsageUpdate)) error
	UsageAcknowledge func(DashboardUsageAcknowledgement)
	Inspect          PickerInspectFunc
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

const DashboardMeasurementPending = "pending"

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
	// Held counts kept routes stopped by `mesh serve stop`. They wait for
	// `mesh serve start`, so they are not idle.
	Held       int
	ObservedAt time.Time
	Failing    bool
}

func (c DashboardCatalog[T]) Stale(now, lastReply time.Time) bool {
	return c.Failing || c.ObservedAt.IsZero() || lastReply.IsZero() || now.Sub(c.ObservedAt) >= 30*time.Second || now.Sub(lastReply) >= 30*time.Second
}

type DashboardHostView struct {
	Notice             string
	Build              release.Build
	HostUpdate         *protocol.HostUpdateSummary
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

// DashboardUsageUpdate carries one configuration generation through the renderer.
type DashboardUsageUpdate struct {
	Publication uint64
	Generation  uint64
	Enabled     bool
	State       string
	Digest      string
	URL         string
	Result      usagefeed.Result
}
type DashboardUsageAcknowledgement struct {
	Publication  uint64
	Generation   uint64
	Enabled      bool
	State        string
	Digest       string
	FeedRevision uint64
}

func (u DashboardUsageUpdate) Acknowledgement() DashboardUsageAcknowledgement {
	return DashboardUsageAcknowledgement{u.Publication, u.Generation, u.Enabled, u.State, u.Digest, u.Result.Revision}
}

// DashboardStatusState describes a read without asserting host reachability.
type DashboardStatusState string

const (
	DashboardStatusObserved    DashboardStatusState = "observed"
	DashboardStatusUnavailable DashboardStatusState = agentUnavailable
	DashboardStatusRefused     DashboardStatusState = "refused"
)

type DashboardStatusObservation struct {
	State   DashboardStatusState `json:"state"`
	At      time.Time            `json:"at"`
	Problem string               `json:"problem,omitempty"`
}
type DashboardStatus[T any] struct {
	Observation DashboardStatusObservation `json:"observation"`
	Value       *T                         `json:"value"`
}
type DashboardUpdateSource struct {
	Coordinator   update.Host                                  `json:"coordinator"`
	Configuration *DashboardStatus[update.ConfigurationStatus] `json:"configuration,omitempty"`
}
type DashboardUpdates struct {
	Generation uint64                  `json:"generation"`
	Sources    []DashboardUpdateSource `json:"sources"`
	Problem    string                  `json:"problem,omitempty"`
}
type DashboardUpdateRow struct {
	HostID    string
	Text      string
	Attention bool
	Pending   bool
}
