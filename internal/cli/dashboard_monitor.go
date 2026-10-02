package cli

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/storage"
)

type dashboardMonitor struct {
	records []HostRecord
	localID string
	watcher *StateWatcher
	cache   *SQLiteCatalogCache
}

func (m *dashboardMonitor) Run(ctx context.Context, publish func(DashboardHostView)) error {
	if m.watcher == nil || publish == nil {
		return fmt.Errorf("dashboard requires a watcher and publisher")
	}
	run, cancel := context.WithCancel(ctx)
	var readers sync.WaitGroup
	updates := make(chan DashboardHostView)
	exits := make(chan error, 1)
	defer func() { cancel(); readers.Wait() }()
	m.startReaders(run, publish, updates, exits, &readers)
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-exits:
			if ctx.Err() != nil {
				return nil
			}
			return err
		case view := <-updates:
			if ctx.Err() == nil {
				publish(view)
			}
		}
	}
}
func (m *dashboardMonitor) startReaders(ctx context.Context, publish func(DashboardHostView), updates chan<- DashboardHostView, exits chan<- error, readers *sync.WaitGroup) {
	for _, record := range m.records {
		if ctx.Err() != nil {
			return
		}
		initial := m.cachedView(ctx, record)
		if ctx.Err() != nil {
			return
		}
		publish(initial)
		readers.Go(func() {
			err := m.watchHost(ctx, record, initial, updates)
			select {
			case exits <- err:
			case <-ctx.Done():
			}
		})
	}
}

func (m *dashboardMonitor) watchHost(ctx context.Context, record HostRecord, retained DashboardHostView, updates chan<- DashboardHostView) error {
	request := protocol.StateWatch{Topics: []string{protocol.TopicSessions, protocol.TopicServices, protocol.TopicMetrics}, MetricsEveryMillis: 2000}
	return m.watcher.watch(ctx, record, request, func(state StateView) {
		next := projectDashboardState(retained.Host, state)
		if state.Sections[protocol.TopicSessions].ReceivedAt.IsZero() {
			next.Sessions = retained.Sessions
			next.Sessions.Failing = true
		}
		if state.Sections[protocol.TopicServices].ReceivedAt.IsZero() {
			next.Services = retained.Services
			next.Services.Failing = true
		}
		retained = next
		select {
		case updates <- next:
		case <-ctx.Done():
		}
	})
}
func (m *dashboardMonitor) cachedView(ctx context.Context, record HostRecord) DashboardHostView {
	view := DashboardHostView{Host: dashboardHost(record, m.localID), Connection: StateConnecting}
	if m.cache == nil || m.cache.store == nil {
		return view
	}
	readCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	stored, err := m.cache.store.GetHost(readCtx, storage.HostID(record.ID))
	if err != nil || stored.MeshIdentity != record.MeshIdentity {
		return view
	}
	rows, err := m.cache.Load(readCtx, record)
	if err == nil {
		view.Sessions = projectDashboardSessions(rows, ObservedSection{})
		view.Sessions.ObservedAt, view.Sessions.Failing = stored.LastSeenAt, true
	}
	services, err := m.cache.LoadServices(readCtx, record)
	if err != nil {
		return view
	}
	wire := make([]protocol.ServiceInfo, 0, len(services))
	observed := time.Time{}
	for _, row := range services {
		wire = append(wire, protocol.ServiceInfo{DisplayName: row.Service.DisplayName, Name: row.Service.Name, Healthy: row.Healthy, Problem: row.Problem})
		if observed.IsZero() || row.ObservedAt.Before(observed) {
			observed = row.ObservedAt
		}
	}
	view.Services = projectDashboardServices(wire, ObservedSection{}, false)
	view.Services.ObservedAt, view.Services.Failing = observed, true
	return view
}
