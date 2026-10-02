package cli

import (
	"context"
	"time"
)

type DashboardFunc func(context.Context, DashboardInput) error

// DashboardWatch publishes owned values serially and joins its readers before returning.
type DashboardWatch func(context.Context, func(DashboardHostView)) error

type DashboardInput struct {
	Hosts []DashboardHost
	Wall  bool
	Watch DashboardWatch
}

type DashboardHost struct {
	ID, Alias string
	Local     bool
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
type DashboardSession struct{ ID, State, Command string }
type DashboardService struct {
	Name, State, Problem string
	Failed               bool
}
type DashboardCatalog[T any] struct {
	Rows       []T
	Total      int
	ObservedAt time.Time
	Failing    bool
}

func (c DashboardCatalog[T]) Stale(now, lastReply time.Time) bool {
	return c.Failing || c.ObservedAt.IsZero() || lastReply.IsZero() || now.Sub(c.ObservedAt) >= 30*time.Second || now.Sub(lastReply) >= 30*time.Second
}

type DashboardHostView struct {
	Host        DashboardHost
	Connection  StateConnection
	Problem     string
	LastReply   time.Time
	CPU         DashboardMeasurement[float64]
	RAM         DashboardMeasurement[DashboardMemory]
	Temperature DashboardMeasurement[DashboardTemperature]
	Uptime      DashboardMeasurement[uint64]
	Sessions    DashboardCatalog[DashboardSession]
	Services    DashboardCatalog[DashboardService]
}
