package cli

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/protocol"
)

// pickerState owns the selected remote host's stream across short refresh calls.
type pickerState struct {
	serviceMu   sync.Mutex
	serviceHost string
	services    *PickerServiceCatalog
	servicesAt  time.Time
	cache       CatalogCache
	cacheErr    error
	ctx         context.Context
	watcher     *StateWatcher
	switchMu    sync.Mutex
	mu          sync.Mutex
	host        HostRecord
	view        StateView
	ready       chan struct{}
	cancel      context.CancelFunc
	done        chan struct{}
}

func newPickerState(ctx context.Context, dialControl HostDialer) *pickerState {
	return &pickerState{ctx: ctx, watcher: NewStateWatcher(dialControl)}
}
func (p *pickerState) close() {
	p.switchMu.Lock()
	defer p.switchMu.Unlock()
	p.stop()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.host = HostRecord{}
	p.ready = nil
	p.view = StateView{}
}
func (p *pickerState) stop() {
	if p.cancel != nil {
		p.cancel()
		<-p.done
		p.cancel = nil
	}
}
func (p *pickerState) selectHost(host HostRecord) (chan struct{}, chan struct{}) {
	p.switchMu.Lock()
	defer p.switchMu.Unlock()
	if p.cancel != nil && p.host.ID == host.ID && p.host.MeshIdentity == host.MeshIdentity && p.host.Endpoint == host.Endpoint {
		return p.ready, p.done
	}
	p.stop()
	cached, cacheErr := p.loadCache(host)
	ctx, cancel := context.WithCancel(p.ctx)
	p.cancel = cancel
	done := make(chan struct{})
	p.done = done
	ready := make(chan struct{})
	var first sync.Once
	p.mu.Lock()
	p.host = host
	p.view = StateView{Sessions: cloneSessionInfo(cached)}
	p.cacheErr = cacheErr
	p.ready = ready
	p.mu.Unlock()
	go p.run(ctx, host, ready, done, cached, cacheErr, &first)
	return ready, done
}
func (p *pickerState) loadCache(host HostRecord) ([]protocol.SessionInfo, error) {
	if p.cache == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(p.ctx, catalogCacheReadTimeout)
	defer cancel()
	rows, err := p.cache.Load(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("picker cache: %w", err)
	}
	return rows, nil
}
func (p *pickerState) run(ctx context.Context, host HostRecord, ready, done chan struct{}, cached []protocol.SessionInfo, cacheErr error, first *sync.Once) {
	defer close(done)
	_ = p.watcher.Watch(ctx, host, protocol.StateWatch{Topics: []string{protocol.TopicSessions}}, func(view StateView) {
		if ctx.Err() != nil {
			return
		}
		if !view.initialized && view.Sections[protocol.TopicSessions].ReceivedAt.IsZero() {
			view.Sessions = cloneSessionInfo(cached)
		}
		cached, cacheErr = p.saveCache(ctx, host, view, cached, cacheErr)
		p.mu.Lock()
		p.cacheErr = cacheErr
		p.view = view
		p.mu.Unlock()
		first.Do(func() { close(ready) })
	})
}
func (p *pickerState) saveCache(ctx context.Context, host HostRecord, view StateView, cached []protocol.SessionInfo, previousErr error) ([]protocol.SessionInfo, error) {
	section := view.Sections[protocol.TopicSessions]
	if p.cache == nil || section.Observation.Failing || !view.initialized && section.ReceivedAt.IsZero() {
		return cached, previousErr
	}
	if reflect.DeepEqual(cached, view.Sessions) && previousErr == nil {
		return cached, nil
	}
	cacheCtx, cancel := context.WithTimeout(ctx, catalogCacheWriteTimeout)
	defer cancel()
	if err := p.cache.Save(cacheCtx, host, view.Sessions); err != nil {
		return cached, fmt.Errorf("picker cache: %w", err)
	}
	return cloneSessionInfo(view.Sessions), nil
}
func (p *pickerState) read(ctx context.Context, host HostRecord) (HostSessions, error) {
	ready, done := p.selectHost(host) //nolint:contextcheck // The stream uses the picker lifetime, not this short refresh.
	select {
	case <-ctx.Done():
		return HostSessions{}, fmt.Errorf("picker state: %w", ctx.Err())
	case <-ready:
	case <-done:
		return HostSessions{}, fmt.Errorf("picker state selection changed")
	}
	p.mu.Lock()
	if p.ready != ready {
		p.mu.Unlock()
		return HostSessions{}, fmt.Errorf("picker state selection changed")
	}
	cacheErr := p.cacheErr
	view := p.view.Clone()
	p.mu.Unlock()
	now := time.Now()
	rows := cloneSessionInfo(view.Sessions)
	for i := range rows {
		memory, exists := view.Memory[rows[i].ID]
		if exists && memory.Available && memory.AgeMillis >= 0 && memory.AgeMillis < 30000 && time.Duration(memory.AgeMillis)*time.Millisecond+now.Sub(view.MemoryReceivedAt) < 30*time.Second {
			rows[i].MemoryBytes = memory.Bytes
		}
	}
	return HostSessions{Host: host, Sessions: rows, CacheErr: cacheErr, Stale: view.Sections[protocol.TopicSessions].Stale(now, view.LastReply)}, nil
}

func (a *application) refreshWatchedPickerHost(ctx context.Context, host HostRecord, cache pickerCatalogCache, state *pickerState) (PickerHostSnapshot, error) {
	serviceResults := make(chan *PickerServiceCatalog, 1)
	go func() { serviceResults <- a.watchedPickerServices(ctx, host, cache, state) }()
	sessions, err := state.read(ctx, host)
	services := <-serviceResults
	if err != nil {
		return PickerHostSnapshot{}, err
	}
	return PickerHostSnapshot{Sessions: sessions, Services: services}, nil
}
func (a *application) watchedPickerServices(ctx context.Context, host HostRecord, cache pickerServiceCache, state *pickerState) *PickerServiceCatalog {
	state.serviceMu.Lock()
	defer state.serviceMu.Unlock()
	if state.serviceHost == host.ID && time.Since(state.servicesAt) < 10*time.Second {
		return state.services
	}
	services := a.refreshPickerServices(ctx, host, cache)
	if ctx.Err() != nil {
		return services
	}
	state.services = services
	state.servicesAt = time.Now()
	state.serviceHost = host.ID
	return services
}
