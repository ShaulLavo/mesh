package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/protocol"
)

type dashboardReadKind uint8

const (
	dashboardReadMetrics dashboardReadKind = iota
	dashboardReadSessions
	dashboardReadServices
	dashboardReadKinds
)

type dashboardHostState struct {
	record HostRecord
	view DashboardHostView
	due [dashboardReadKinds]time.Time
	busy bool
	retry time.Time
	failures int
	sectionFailures [dashboardReadKinds]int
	build string
	unsupported bool
}

type dashboardMonitor struct {
	hosts []*dashboardHostState
	localID string
	localSocket string
	dial HostDialer
	now func() time.Time
	cache *SQLiteCatalogCache
}

type dashboardJob struct {
	index int
	kind dashboardReadKind
	host HostRecord
	build string
	unsupported bool
	inspect []string
}

type dashboardReadResult struct {
	job dashboardJob
	repliedAt time.Time
	build string
	refused bool
	err error
	metrics *protocol.HostMetrics
	unsupported bool
	sessions []protocol.SessionInfo
	services []protocol.ServiceInfo
	privateName string
	inspections map[string]SessionInspection
}

func newDashboardMonitor(hosts []HostRecord, localID string, dial HostDialer, now func() time.Time) *dashboardMonitor {
	m := &dashboardMonitor{localID: localID, dial: dial, now: now}
	for _, record := range hosts {
		m.hosts = append(m.hosts, &dashboardHostState{
			record: record,
			view: DashboardHostView{
				Host: DashboardHost{ID: record.ID, Alias: record.Alias, Local: record.ID == localID},
				Reachability: DashboardConnecting,
			},
		})
	}
	return m
}

func (m *dashboardMonitor) Run(ctx context.Context, publish func(DashboardHostView)) error {
	if ctx == nil || publish == nil || m.now == nil || m.dial == nil {
		return errors.New("dashboard watch requires context, publisher, clock, and one-shot dialer")
	}
	if ctx.Err() != nil {
		return nil
	}
	runCtx, cancel := context.WithCancel(ctx)
	jobs := make(chan dashboardJob, dashboardConcurrency)
	results := make(chan dashboardReadResult, dashboardConcurrency)
	var workers sync.WaitGroup
	for range dashboardConcurrency {
		workers.Go(func() { m.worker(runCtx, jobs, results) })
	}
	defer func() { cancel(); workers.Wait() }()
	for _, current := range m.hosts {
		publish(cloneDashboardView(current.view))
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	inFlight := 0
	for {
		inFlight += m.dispatch(jobs, dashboardConcurrency-inFlight)
		select {
		case <-runCtx.Done():
			return nil
		case result := <-results:
			inFlight--
			if runCtx.Err() == nil {
				m.apply(result, publish)
			}
		case <-ticker.C:
		}
	}
}

func (m *dashboardMonitor) worker(ctx context.Context, jobs <-chan dashboardJob, results chan<- dashboardReadResult) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-jobs:
			result := m.read(ctx, job)
			select {
			case results <- result:
			case <-ctx.Done():
				return
			}
		}
	}
}

func (m *dashboardMonitor) dispatch(jobs chan<- dashboardJob, capacity int) int {
	count := 0
	for count < capacity {
		index, kind := m.next(m.now())
		if index < 0 {
			return count
		}
		current := m.hosts[index]
		job := dashboardJob{index: index, kind: kind, host: current.record, build: current.build, unsupported: current.unsupported}
		if kind == dashboardReadSessions {
			job.inspect = m.inspectionIDs(index)
		}
		current.busy = true
		jobs <- job
		count++
	}
	return count
}

func (m *dashboardMonitor) next(now time.Time) (int, dashboardReadKind) {
	index := -1
	var selected dashboardReadKind
	var earliest time.Time
	for i, current := range m.hosts {
		if current.busy || now.Before(current.retry) {
			continue
		}
		kind, due := current.nextRead()
		if due.After(now) || index >= 0 && !due.Before(earliest) {
			continue
		}
		index, selected, earliest = i, kind, due
	}
	return index, selected
}

func (h *dashboardHostState) nextRead() (dashboardReadKind, time.Time) {
	kind := dashboardReadMetrics
	for candidate := dashboardReadSessions; candidate < dashboardReadKinds; candidate++ {
		if h.due[candidate].Before(h.due[kind]) {
			kind = candidate
		}
	}
	return kind, h.due[kind]
}

func (m *dashboardMonitor) inspectionIDs(index int) []string {
	remaining := dashboardInspectionLimit
	for i, current := range m.hosts {
		ids := dashboardLiveIDs(current.view.Sessions)
		if i == index {
			return ids[:min(len(ids), remaining)]
		}
		remaining -= min(len(ids), remaining)
	}
	return nil
}

func dashboardLiveIDs(rows []DashboardSession) []string {
	var ids []string
	for _, row := range rows {
		if row.State == "running" || row.State == "detached" {
			ids = append(ids, row.ID)
		}
	}
	slices.Sort(ids)
	return ids
}

func (m *dashboardMonitor) apply(result dashboardReadResult, publish func(DashboardHostView)) {
	current := m.hosts[result.job.index]
	current.busy = false
	now := m.now()
	if result.repliedAt.IsZero() {
		current.failures++
		current.retry = now.Add(dashboardBackoff(current.failures))
		current.view.Reachability = DashboardUnreachable
		if result.refused {
			current.view.Reachability = DashboardRefused
		}
		current.view.Problem = result.err.Error()
		markDashboardStale(&current.view)
		publish(cloneDashboardView(current.view))
		return
	}
	current.failures = 0
	current.retry = time.Time{}
	current.view.Reachability = DashboardReachable
	current.view.LastReply = result.repliedAt
	current.view.Problem = ""
	if result.build != current.build {
		current.build, current.unsupported = result.build, false
		current.due[dashboardReadMetrics] = time.Time{}
	}
	interval := dashboardCatalogInterval
	if result.job.kind == dashboardReadMetrics {
		interval = dashboardMetricsInterval
	}
	if result.err != nil {
		current.sectionFailures[result.job.kind]++
		interval = dashboardBackoff(current.sectionFailures[result.job.kind])
	} else {
		current.sectionFailures[result.job.kind] = 0
	}
	current.due[result.job.kind] = now.Add(interval)
	m.applySection(current, result, now)
	publish(cloneDashboardView(current.view))
}

func dashboardBackoff(failures int) time.Duration {
	return min(60*time.Second, 5*time.Second*time.Duration(1<<min(max(failures-1, 0), 4)))
}

func markDashboardStale(view *DashboardHostView) {
	view.CPU.Stale, view.Memory.Stale = true, true
	view.Temperature.Stale, view.Uptime.Stale = true, true
	view.SessionsStale, view.ServicesStale = true, true
}

func cloneDashboardView(view DashboardHostView) DashboardHostView {
	view.Sessions = slices.Clone(view.Sessions)
	for i := range view.Sessions {
		view.Sessions[i].LastOutputAt = cloneTime(view.Sessions[i].LastOutputAt)
	}
	view.Services = slices.Clone(view.Services)
	return view
}

func dashboardBuildKey(info protocol.HostInfo) string {
	if info.Build == nil {
		return "unknown"
	}
	return fmt.Sprintf("%s/%s/%s/%t", info.Build.Version, info.Build.Commit, info.Build.Digest, info.Build.Modified)
}
