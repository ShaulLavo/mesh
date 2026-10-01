package cli

import (
	"context"
	"fmt"
	"github.com/shaul/mesh/internal/protocol"
	"sync"
	"time"
)

// pickerState owns the selected remote host's stream across short refresh calls.
type pickerState struct {
	serviceMu   sync.Mutex
	serviceHost string
	services    *PickerServiceCatalog
	servicesAt  time.Time
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
func (p *pickerState) close() { p.switchMu.Lock(); defer p.switchMu.Unlock(); p.stop() }
func (p *pickerState) stop() {
	if p.cancel != nil {
		p.cancel()
		<-p.done
		p.cancel = nil
	}
}
func (p *pickerState) selectHost(host HostRecord) {
	p.switchMu.Lock()
	defer p.switchMu.Unlock()
	if p.cancel != nil && p.host.ID == host.ID && p.host.Endpoint == host.Endpoint {
		return
	}
	p.stop()
	ctx, cancel := context.WithCancel(p.ctx)
	p.cancel = cancel
	p.done = make(chan struct{})
	ready := make(chan struct{})
	var first sync.Once
	p.mu.Lock()
	p.host = host
	p.view = StateView{}
	p.ready = ready
	p.mu.Unlock()
	go func() {
		defer close(p.done)
		_ = p.watcher.Watch(ctx, host, protocol.StateWatch{Topics: []string{protocol.TopicSessions}}, func(view StateView) {
			if ctx.Err() != nil {
				return
			}
			p.mu.Lock()
			p.view = view
			p.mu.Unlock()
			first.Do(func() { close(ready) })
		})
	}()
}
func (p *pickerState) read(ctx context.Context, host HostRecord) (HostSessions, error) {
	p.selectHost(host) //nolint:contextcheck // The stream uses the picker lifetime, not this short refresh.
	p.mu.Lock()
	ready := p.ready
	p.mu.Unlock()
	select {
	case <-ctx.Done():
		return HostSessions{}, fmt.Errorf("picker state: %w", ctx.Err())
	case <-ready:
	}
	p.mu.Lock()
	view := p.view.Clone()
	p.mu.Unlock()
	now := time.Now()
	rows := cloneSessionInfo(view.Sessions)
	for i := range rows {
		memory, exists := view.Memory[rows[i].ID]
		if exists && memory.Available && time.Duration(memory.AgeMillis)*time.Millisecond+now.Sub(view.MemoryReceivedAt) < 30*time.Second {
			rows[i].MemoryBytes = memory.Bytes
		}
	}
	return HostSessions{Host: host, Sessions: rows, Stale: view.Sections[protocol.TopicSessions].Stale(now, view.LastReply)}, nil
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
