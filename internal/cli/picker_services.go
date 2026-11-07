package cli

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/domainpolicy"
	"github.com/shaul/mesh/internal/protocol"
)

type PickerServicesUpdate struct {
	Policy  domainpolicy.CatalogToken
	Host    HostRecord
	Catalog PickerServiceCatalog
	Problem string
}

type PickerServicesWatchFunc func(context.Context, func(PickerServicesUpdate)) error

type pickerServiceMonitor struct {
	hosts   []HostRecord
	watcher *StateWatcher
	cache   pickerServiceCache
}

func (m *pickerServiceMonitor) Run(ctx context.Context, publish func(PickerServicesUpdate)) error {
	run, cancel := context.WithCancel(ctx)
	var readers sync.WaitGroup
	defer func() { cancel(); readers.Wait() }()
	updates := make(chan PickerServicesUpdate)
	retained := make(map[string]PickerServicesUpdate)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for _, host := range m.hosts {
		readers.Go(func() { m.watchHost(run, host, updates) })
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case update := <-updates:
			update = fencePickerPolicy(ctx, update)
			retained[update.Host.ID] = update
			publish(update)
		case <-ticker.C:
			refreshPickerPolicy(ctx, retained, publish)
		}
	}
}

func (m *pickerServiceMonitor) watchHost(ctx context.Context, host HostRecord, updates chan<- PickerServicesUpdate) {
	token, _ := loadedServicePolicy(ctx)
	current := PickerServicesUpdate{Policy: token, Host: host, Catalog: PickerServiceCatalog{Stale: true}}
	if m.cache != nil {
		readCtx, cancel := context.WithTimeout(ctx, catalogCacheReadTimeout)
		cached, err := m.cache.LoadServices(readCtx, host)
		cancel()
		current.Catalog.Rows = cachedServiceCatalogRows(host, cached)
		if err != nil {
			current.Problem = err.Error()
		}
	}
	publication := &pickerServicePublication{current: current}
	publication.send(ctx, updates)
	var saved pickerSavedCatalog
	queue := func(StateView) {}
	stop := func() {}
	if m.cache != nil {
		queue, stop = startPickerCacheWriter(ctx, func(writeCtx context.Context, state StateView) {
			stale := state.Sections[protocol.TopicServices].Stale(time.Now(), state.LastReply)
			err := m.saveCatalog(writeCtx, host, state, stale, &saved)
			publication.cacheResult(writeCtx, updates, err)
		})
	}
	defer stop()
	_ = m.watcher.Watch(ctx, host, protocol.StateWatch{Topics: []string{protocol.TopicServices}}, func(state StateView) {
		if !servicePolicyCurrent(ctx, state.Policy) {
			state = invalidateServiceState(state)
		}
		publication.observe(host, state)
		publication.mu.Lock()
		if !state.Sections[protocol.TopicServices].ReceivedAt.IsZero() {
			publication.current.Policy = state.Policy
		}
		publication.mu.Unlock()
		publication.send(ctx, updates)
		queue(state)
	})
}

func PickerServiceState(row ServiceCatalogRow) string {
	state := projectDashboardService(row.Service, !row.HealthUnsupported).State
	if state == "ready" {
		return "running"
	}
	return state
}

type pickerSavedCatalog struct {
	services    []protocol.ServiceInfo
	privateName string
	received    bool
}

func (m *pickerServiceMonitor) saveCatalog(ctx context.Context, host HostRecord, state StateView, stale bool, saved *pickerSavedCatalog) error {
	if m.cache == nil || stale || !servicePolicyCurrent(ctx, state.Policy) {
		return nil
	}
	if saved.received && saved.privateName == state.PrivateName && reflect.DeepEqual(saved.services, state.Services) {
		return nil
	}
	writeCtx, cancel := context.WithTimeout(ctx, serviceCacheWriteTimeout)
	defer cancel()
	persisted, err := m.cache.SaveServices(writeCtx, host, state.PrivateName, state.Services)
	if err != nil {
		return fmt.Errorf("save catalog: %w", err)
	}
	if !persisted {
		return nil
	}
	saved.services = cloneWireServices(state.Services)
	saved.privateName, saved.received = state.PrivateName, true
	return nil
}

func fencePickerPolicy(ctx context.Context, next PickerServicesUpdate) PickerServicesUpdate {
	if servicePolicyCurrent(ctx, next.Policy) {
		return next
	}
	next.Catalog.Rows, next.Catalog.Stale, next.Problem = nil, true, errCatalogPolicyChanged.Error()
	return next
}

func refreshPickerPolicy(ctx context.Context, retained map[string]PickerServicesUpdate, publish func(PickerServicesUpdate)) {
	for id, update := range retained {
		if servicePolicyCurrent(ctx, update.Policy) {
			continue
		}
		update = fencePickerPolicy(ctx, update)
		retained[id] = update
		publish(update)
	}
}
