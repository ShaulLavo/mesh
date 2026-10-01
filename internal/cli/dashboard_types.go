package cli

import (
	"context"
	"time"
)

type DashboardFunc func(context.Context, DashboardInput) error
type DashboardWatch func(context.Context, func(DashboardHostView)) error

type DashboardInput struct {
	Hosts []DashboardHost
	Wall  bool
	Watch DashboardWatch
}

type DashboardHost struct {
	ID    string
	Alias string
	Local bool
}

type DashboardReachability string

const (
	DashboardConnecting  DashboardReachability = "connecting"
	DashboardReachable   DashboardReachability = "reachable"
	DashboardUnreachable DashboardReachability = "unreachable"
	DashboardRefused     DashboardReachability = "refused"
)

type DashboardMeasurement[T any] struct {
	State      string
	Value      T
	Sample     string
	MeasuredAt time.Time
	Problem    string
	Stale      bool
}

type DashboardMemory struct {
	TotalBytes     uint64
	AvailableBytes uint64
}

type DashboardTemperature struct {
	Sensor  string
	Celsius float64
}

type DashboardSession struct {
	ID                string
	Name              string
	State             string
	Command           string
	ObservedAt        time.Time
	LastOutputAt      *time.Time
	InspectionProblem string
}

type DashboardService struct {
	Name    string
	State   string
	Problem string
}

type DashboardHostView struct {
	Host               DashboardHost
	Reachability       DashboardReachability
	LastReply          time.Time
	Problem            string
	CPU                DashboardMeasurement[float64]
	Memory             DashboardMeasurement[DashboardMemory]
	Temperature        DashboardMeasurement[DashboardTemperature]
	Uptime             DashboardMeasurement[uint64]
	Sessions           []DashboardSession
	Services           []DashboardService
	SessionsObservedAt time.Time
	ServicesObservedAt time.Time
	SessionsStale      bool
	ServicesStale      bool
	SessionsProblem    string
	ServicesProblem    string
}
