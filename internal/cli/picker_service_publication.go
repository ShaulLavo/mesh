package cli

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/protocol"
)

type pickerServicePublication struct {
	mu           sync.Mutex
	current      PickerServicesUpdate
	cacheProblem string
}

func (p *pickerServicePublication) observe(host HostRecord, state StateView) {
	p.mu.Lock()
	defer p.mu.Unlock()
	section := state.Sections[protocol.TopicServices]
	p.current.Problem = state.Problem
	p.current.Catalog.Stale = section.Stale(time.Now(), state.LastReply)
	if section.ReceivedAt.IsZero() {
		return
	}
	rows := liveServiceCatalogRows(host, remoteServiceSnapshot{PrivateName: state.PrivateName, Services: state.Services, ServiceHealthSupported: state.ServiceHealthSupported})
	for index := range rows {
		rows[index].Stale = p.current.Catalog.Stale
	}
	p.current.Catalog.Rows = rows
}

func (p *pickerServicePublication) send(ctx context.Context, updates chan<- PickerServicesUpdate) {
	p.mu.Lock()
	current := p.current
	if p.cacheProblem != "" {
		current.Problem = p.cacheProblem
	}
	p.mu.Unlock()
	select {
	case updates <- current:
	case <-ctx.Done():
	}
}

func (p *pickerServicePublication) cacheResult(ctx context.Context, updates chan<- PickerServicesUpdate, err error) {
	problem := ""
	if err != nil {
		problem = fmt.Sprintf("cache services: %v", err)
	}
	p.mu.Lock()
	changed := p.cacheProblem != problem
	p.cacheProblem = problem
	p.mu.Unlock()
	if changed {
		p.send(ctx, updates)
	}
}
