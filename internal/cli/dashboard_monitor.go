package cli

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/domainpolicy"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/storage"
)

type dashboardMonitorUpdate struct {
	view   DashboardHostView
	policy domainpolicy.CatalogToken
}

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
	updates := make(chan dashboardMonitorUpdate)
	retained := make(map[string]dashboardMonitorUpdate)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	exits := make(chan error, 1)
	defer func() { cancel(); readers.Wait() }()
	m.startReaders(run, publish, updates, exits, &readers)
	for {
		finished, err := dashboardMonitorStep(ctx, updates, exits, ticker.C, retained, publish)
		if finished {
			return dashboardMonitorResult(ctx, err)
		}
	}
}
func dashboardMonitorStep(ctx context.Context, updates <-chan dashboardMonitorUpdate, exits <-chan error, ticks <-chan time.Time, retained map[string]dashboardMonitorUpdate, publish func(DashboardHostView)) (bool, error) {
	select {
	case <-ctx.Done():
		return true, fmt.Errorf("dashboard monitor stopped: %w", ctx.Err())
	case err := <-exits:
		return true, err
	case next := <-updates:
		if err := ctx.Err(); err != nil {
			return true, fmt.Errorf("dashboard monitor stopped: %w", err)
		}
		next = fenceDashboardPolicy(ctx, next)
		retained[next.view.Host.ID] = next
		publish(next.view)
	case <-ticks:
		refreshDashboardPolicy(ctx, retained, publish)
	}
	return false, nil
}

func (m *dashboardMonitor) startReaders(ctx context.Context, publish func(DashboardHostView), updates chan<- dashboardMonitorUpdate, exits chan<- error, readers *sync.WaitGroup) {
	for _, record := range m.records {
		if ctx.Err() != nil {
			return
		}
		initial := m.cachedView(ctx, record)
		if ctx.Err() != nil {
			return
		}
		publish(initial)
		readers.Go(func() { m.watchReader(ctx, record, initial, updates, exits) })
	}
}

func (m *dashboardMonitor) watchHost(ctx context.Context, record HostRecord, retained DashboardHostView, updates chan<- dashboardMonitorUpdate) error {
	request := protocol.StateWatch{Topics: []string{protocol.TopicHost, protocol.TopicSessions, protocol.TopicServices, protocol.TopicMetrics}, MetricsEveryMillis: 2000}
	return m.watcher.watch(ctx, record, request, func(state StateView) {
		state = fenceServiceState(ctx, state)
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
		case updates <- dashboardMonitorUpdate{view: next, policy: state.Policy}:
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

func fenceDashboardPolicy(ctx context.Context, next dashboardMonitorUpdate) dashboardMonitorUpdate {
	status, err := observeServicePolicy(ctx, next.policy)
	if status == servicePolicyKnownCurrent {
		return next
	}
	if status == servicePolicyChanged || next.policy.Generation == 0 {
		next.view.Services = DashboardCatalog[DashboardService]{}
	}
	next.view.Services.Failing = true
	next.view.Host.NameVerified, next.view.NameFailing = false, true
	next.view.Problem = err.Error()
	return next
}

func refreshDashboardPolicy(ctx context.Context, retained map[string]dashboardMonitorUpdate, publish func(DashboardHostView)) {
	for id, next := range retained {
		if servicePolicyCurrent(ctx, next.policy) {
			continue
		}
		next = fenceDashboardPolicy(ctx, next)
		retained[id] = next
		publish(next.view)
	}
}

func (m *dashboardMonitor) watchReader(ctx context.Context, record HostRecord, initial DashboardHostView, updates chan<- dashboardMonitorUpdate, exits chan<- error) {
	err := m.watchHost(ctx, record, initial, updates)
	select {
	case exits <- err:
	case <-ctx.Done():
	}
}

func dashboardMonitorResult(ctx context.Context, err error) error {
	select {
	case <-ctx.Done():
		return nil
	default:
		return err
	}
}
