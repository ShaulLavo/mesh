package cli

import (
	"context"
	"sync"
)

// pickerOperationGate owns callbacks launched by one picker instance: no work
// may start after it returns, and all work that already started finishes first.
type pickerOperationGate struct {
	mu        sync.Mutex
	accepting bool
	active    sync.WaitGroup
}

func newPickerOperationGate() *pickerOperationGate {
	return &pickerOperationGate{accepting: true}
}

func (g *pickerOperationGate) begin(ctx context.Context) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.accepting || ctx == nil || ctx.Err() != nil {
		return false
	}
	g.active.Add(1)
	return true
}

func (g *pickerOperationGate) done() {
	g.active.Done()
}

func (g *pickerOperationGate) stop() {
	g.mu.Lock()
	g.accepting = false
	g.mu.Unlock()
}

func (g *pickerOperationGate) wait() {
	g.active.Wait()
}

func (g *pickerOperationGate) stopAndWait() {
	g.stop()
	g.wait()
}
