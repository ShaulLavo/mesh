package daemon

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	meshserve "github.com/shaul/mesh/internal/serve"
)

// A Stop is answered by the stop that carried it out. When that stop has
// already failed by the time the caller looks at the route again, the caller
// reports that failure; it does not kill the same session a second time
// outside the cleanup schedule.

func TestKeptStopAnswersWithTheFailedStopItBegan(t *testing.T) {
	f := newKeptFixture(t)
	f.syncKept(t)
	f.sessions.failStops(errWorkerSilent)
	f.manager.stopLooks = func() { f.manager.goroutines.Wait() }
	err := f.manager.Stop(context.Background(), "dev")
	if !errors.Is(err, errWorkerSilent) || !strings.Contains(err.Error(), "stop session S001") {
		t.Fatalf("Stop = %v, want the failure of the stop of S001", err)
	}
	if started, stopped := f.sessions.counts(); started != 1 || stopped != 1 {
		t.Fatalf("started %d, stopped %d; want only the stop the Stop began", started, stopped)
	}
	f.assertStatus(t, protocol.DemandFailed, "S001", true)
}

func TestKeptStopDuringStartAnswersWithTheStopTheStartBegan(t *testing.T) {
	f := newKeptFixture(t)
	ready := newDemandBarrier(t)
	f.manager.dial = func(context.Context, string) error { ready.wait(); return nil }
	f.manager.Sync([]meshserve.Service{keptService()})
	awaitDemandSignal(t, ready.entered, "kept start readiness")
	f.sessions.failStops(errWorkerSilent)
	var looks atomic.Int32
	f.manager.stopLooks = func() {
		if looks.Add(1) == 1 {
			return
		}
		f.manager.goroutines.Wait()
	}
	f.manager.stopWaits = ready.unblock
	stop := beginAdoptionCall(t, func(ctx context.Context) error { return f.manager.Stop(ctx, "dev") })
	if err := stop.wait(t); !errors.Is(err, errWorkerSilent) || !strings.Contains(err.Error(), "stop session S001") {
		t.Fatalf("Stop = %v, want the failure of the stop the start began", err)
	}
	settleDemand(t, f.manager)
	if started, stopped := f.sessions.counts(); started != 1 || stopped != 1 {
		t.Fatalf("started %d, stopped %d; want only the stop the start began", started, stopped)
	}
	f.assertStatus(t, protocol.DemandFailed, "S001", true)
}

func TestDemandStopAfterAdoptionAnswersWithTheCleanupThatRan(t *testing.T) {
	sessions, manager := unresolvedAdoption(t, demandService(time.Minute))
	sessions.setLookup(nil, "OLD1", true)
	gate := sessions.blockLookup(t)
	var looks atomic.Int32
	manager.stopLooks = func() {
		if looks.Add(1) == 1 {
			return
		}
		manager.supervise()
		manager.goroutines.Wait()
	}
	result := beginAdoptionCall(t, func(ctx context.Context) error { return manager.Stop(ctx, "dev") })
	awaitAdoptionSignal(t, gate.entered)
	sessions.failStops(errWorkerSilent)
	gate.unblock()
	if err := result.wait(t); !errors.Is(err, errWorkerSilent) || !strings.Contains(err.Error(), "stop session OLD1") {
		t.Fatalf("Stop = %v, want the failure of the cleanup stop of OLD1", err)
	}
	joinAdoptionTransitions(t, manager)
	assertAdoptionCounts(t, sessions, 0, 1)
	if sessions.liveCount() != 1 {
		t.Fatal("a failed stop reported the adopted worker gone")
	}
}

func TestKeptStopKeepsOwnFailureAcrossLaterStop(t *testing.T) {
	f := newKeptFixture(t)
	f.syncKept(t)
	firstErr := errors.New("first accepted stop failed")
	secondErr := errors.New("later accepted stop failed")
	paused := newDemandBarrier(t)
	var looks atomic.Int32
	f.manager.stopLooks = func() {
		if looks.Add(1) == 1 {
			paused.wait()
			return
		}
		f.manager.goroutines.Wait()
	}
	f.sessions.failStops(firstErr)
	first := beginAdoptionCall(t, func(ctx context.Context) error { return f.manager.Stop(ctx, "dev") })
	t.Cleanup(paused.unblock)
	awaitDemandSignal(t, paused.entered, "first Stop accepted")
	settleDemand(t, f.manager)
	f.sessions.failStops(secondErr)
	if err := f.manager.Stop(context.Background(), "dev"); !errors.Is(err, secondErr) {
		t.Fatalf("later Stop = %v, want its own failure", err)
	}
	paused.unblock()
	err := first.wait(t)
	if !errors.Is(err, firstErr) || errors.Is(err, secondErr) {
		t.Fatalf("first Stop = %v; first identity=%v, later identity=%v", err, errors.Is(err, firstErr), errors.Is(err, secondErr))
	}
}

func TestKeptJoiningStopKeepsOwnFailureAcrossLaterStop(t *testing.T) {
	f := newKeptFixture(t)
	f.syncKept(t)
	firstErr := errors.New("shared stop failed")
	secondErr := errors.New("later stop failed")
	stopping, paused := newDemandBarrier(t), newDemandBarrier(t)
	f.sessions.mu.Lock()
	f.sessions.stopGate = stopping
	f.sessions.mu.Unlock()
	f.sessions.failStops(firstErr)
	accepted := make(chan struct{}, 2)
	var looks atomic.Int32
	f.manager.stopLooks = func() {
		if looks.Add(1) <= 2 {
			accepted <- struct{}{}
			paused.wait()
			return
		}
		f.manager.goroutines.Wait()
	}
	first := beginAdoptionCall(t, func(ctx context.Context) error { return f.manager.Stop(ctx, "dev") })
	t.Cleanup(paused.unblock)
	t.Cleanup(stopping.unblock)
	awaitDemandSignal(t, accepted, "first Stop accepted")
	awaitDemandSignal(t, stopping.entered, "first stop in flight")
	joined := beginAdoptionCall(t, func(ctx context.Context) error { return f.manager.Stop(ctx, "dev") })
	t.Cleanup(paused.unblock)
	awaitDemandSignal(t, accepted, "joining Stop accepted")
	stopping.unblock()
	settleDemand(t, f.manager)
	f.sessions.failStops(secondErr)
	if err := f.manager.Stop(context.Background(), "dev"); !errors.Is(err, secondErr) {
		t.Fatalf("later Stop = %v, want its own failure", err)
	}
	paused.unblock()
	for _, caller := range []*adoptionCall{first, joined} {
		err := caller.wait(t)
		if !errors.Is(err, firstErr) || errors.Is(err, secondErr) {
			t.Fatalf("shared Stop = %v, want only the shared attempt's failure", err)
		}
	}
	if started, stopped := f.sessions.counts(); started != 1 || stopped != 2 {
		t.Fatalf("started %d, stopped %d; want two explicit attempts and no launch", started, stopped)
	}
}

func TestKeptNoopStopDoesNotJoinALaterStop(t *testing.T) {
	f := newKeptFixture(t)
	f.syncKept(t)
	if err := f.manager.Stop(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	paused := newDemandBarrier(t)
	var looks atomic.Int32
	f.manager.stopLooks = func() {
		if looks.Add(1) == 1 {
			paused.wait()
			return
		}
		f.manager.goroutines.Wait()
	}
	first := beginAdoptionCall(t, func(ctx context.Context) error { return f.manager.Stop(ctx, "dev") })
	t.Cleanup(paused.unblock)
	awaitDemandSignal(t, paused.entered, "no-op Stop accepted")
	if err := f.manager.Start(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	f.sessions.failStops(errWorkerSilent)
	if err := f.manager.Stop(context.Background(), "dev"); !errors.Is(err, errWorkerSilent) {
		t.Fatalf("later Stop = %v, want its own failure", err)
	}
	paused.unblock()
	if err := first.wait(t); err != nil {
		t.Fatalf("no-op Stop = %v, want no work owed", err)
	}
}
