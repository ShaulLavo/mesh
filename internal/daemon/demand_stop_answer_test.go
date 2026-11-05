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
	looked := make(chan struct{})
	var looks atomic.Int32
	f.manager.stopLooks = func() {
		if looks.Add(1) == 1 {
			close(looked)
			return
		}
		f.manager.goroutines.Wait()
	}
	stop := beginAdoptionCall(t, func(ctx context.Context) error { return f.manager.Stop(ctx, "dev") })
	awaitDemandSignal(t, looked, "the Stop's first look")
	ready.unblock()
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
