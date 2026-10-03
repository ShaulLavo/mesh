package cli

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/protocol"
)

type PickerServicesUpdate struct {
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
	for _, host := range m.hosts {
		readers.Go(func() { m.watchHost(run, host, updates) })
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case update := <-updates:
			publish(update)
		}
	}
}

func (m *pickerServiceMonitor) watchHost(ctx context.Context, host HostRecord, updates chan<- PickerServicesUpdate) {
	current := PickerServicesUpdate{Host: host, Catalog: PickerServiceCatalog{Stale: true}}
	if m.cache != nil {
		readCtx, cancel := context.WithTimeout(ctx, catalogCacheReadTimeout)
		cached, err := m.cache.LoadServices(readCtx, host)
		cancel()
		current.Catalog.Rows = cachedServiceCatalogRows(host, cached)
		if err != nil {
			current.Problem = err.Error()
		}
	}
	send := func() {
		select {
		case updates <- current:
		case <-ctx.Done():
		}
	}
	send()
	var saved pickerSavedCatalog
	_ = m.watcher.Watch(ctx, host, protocol.StateWatch{Topics: []string{protocol.TopicServices}}, func(state StateView) {
		section := state.Sections[protocol.TopicServices]
		current.Problem = state.Problem
		current.Catalog.Stale = section.Stale(time.Now(), state.LastReply)
		if section.ReceivedAt.IsZero() {
			send()
			return
		}
		current.Catalog.Rows = liveServiceCatalogRows(host, remoteServiceSnapshot{PrivateName: state.PrivateName, Services: state.Services})
		for index := range current.Catalog.Rows {
			current.Catalog.Rows[index].Stale = current.Catalog.Stale
			current.Catalog.Rows[index].HealthUnsupported = !state.ServiceHealthSupported
		}
		if err := m.saveCatalog(ctx, host, state, current.Catalog.Stale, &saved); err != nil {
			current.Problem = fmt.Sprintf("cache services: %v", err)
		}
		send()
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
	if m.cache == nil || stale {
		return nil
	}
	if saved.received && saved.privateName == state.PrivateName && reflect.DeepEqual(saved.services, state.Services) {
		return nil
	}
	writeCtx, cancel := context.WithTimeout(ctx, serviceCacheWriteTimeout)
	defer cancel()
	if err := m.cache.SaveServices(writeCtx, host, state.PrivateName, state.Services); err != nil {
		return fmt.Errorf("save catalog: %w", err)
	}
	saved.services = cloneWireServices(state.Services)
	saved.privateName, saved.received = state.PrivateName, true
	return nil
}
