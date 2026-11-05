package daemon

import (
	"context"
	"errors"
	"log"
	"math"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	meshserve "github.com/shaul/mesh/internal/serve"
)

// demandClock is the manager's clock for kept scheduling, moved only by the
// test.
type demandClock struct {
	mu  sync.Mutex
	now time.Time
}

func newDemandClock(manager *demandManager) *demandClock {
	clock := &demandClock{now: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)}
	manager.now = clock.read
	return clock
}

func (c *demandClock) read() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *demandClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func keptService() meshserve.Service {
	service := demandService(0)
	service.Demand.KeepRunning = true
	return service
}

type keptFixture struct {
	sessions *fakeDemandSessions
	manager  *demandManager
	clock    *demandClock
	log      *demandLogSink
}

// newKeptFixture builds a manager with a fake clock and a synchronized log.
// The route is not synced yet, so a test can install barriers first.
func newKeptFixture(t *testing.T) keptFixture {
	t.Helper()
	sessions := newFakeDemandSessions()
	manager := testDemandManager(t, sessions, func() bool { return true })
	sink := &demandLogSink{}
	manager.logger = log.New(sink, "", 0)
	return keptFixture{sessions: sessions, manager: manager, clock: newDemandClock(manager), log: sink}
}

func (f keptFixture) syncKept(t *testing.T) {
	t.Helper()
	f.manager.Sync([]meshserve.Service{keptService()})
	settleDemand(t, f.manager)
}

// settleDemand joins every start, stop and adoption the manager began, so
// what they log and record is complete before the test reads it.
func settleDemand(t *testing.T, manager *demandManager) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		manager.goroutines.Wait()
		close(done)
	}()
	awaitDemandSignal(t, done, "the route's transitions")
}

func (f keptFixture) route() *demandRoute { return f.manager.route("dev") }

func (f keptFixture) restartIn() time.Duration {
	route := f.route()
	route.mu.Lock()
	defer route.mu.Unlock()
	if route.restartAt.IsZero() {
		return 0
	}
	return route.restartAt.Sub(f.clock.read())
}

func (f keptFixture) crash(t *testing.T, code int) {
	t.Helper()
	f.sessions.crash(f.manager.Status("dev").SessionID, code)
	f.manager.supervise()
}

func (f keptFixture) started() int {
	started, _ := f.sessions.counts()
	return started
}

func (f keptFixture) assertStatus(t *testing.T, state, id string, held bool) {
	t.Helper()
	status := f.manager.Status("dev")
	if status.State != state || status.SessionID != id || status.Held != held {
		t.Fatalf("status = %+v, want %s, session %q, held %v", status, state, id, held)
	}
}

// expectedKeptDelays is the contract: doubling from one second, then five
// minutes forever.
var expectedKeptDelays = []time.Duration{
	time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second,
	32 * time.Second, 64 * time.Second, 128 * time.Second, 256 * time.Second,
}

func expectedKeptDelay(failure int) time.Duration {
	if failure <= len(expectedKeptDelays) {
		return expectedKeptDelays[failure-1]
	}
	return 5 * time.Minute
}

func TestKeptRestartDelaySaturates(t *testing.T) {
	for failure := 1; failure <= 200; failure++ {
		if got, want := keptRestartDelay(failure), expectedKeptDelay(failure); got != want {
			t.Fatalf("delay after %d failures = %s, want %s", failure, got, want)
		}
	}
	for _, failures := range []int{math.MinInt, -1, 0} {
		if got := keptRestartDelay(failures); got != time.Second {
			t.Fatalf("delay for %d failures = %s, want 1s", failures, got)
		}
	}
	if got := keptRestartDelay(math.MaxInt); got != 5*time.Minute {
		t.Fatalf("delay for the largest count = %s, want 5m", got)
	}
}

func TestKeptRouteStartsWithoutTrafficAndNeverIdles(t *testing.T) {
	f := newKeptFixture(t)
	f.syncKept(t)
	f.assertStatus(t, protocol.DemandRunning, "S001", false)
	release, started := f.route().hold(listenerCause(5173))
	if started != nil {
		t.Fatal("a connection to a running kept route began a transition")
	}
	release()
	route := f.route()
	route.mu.Lock()
	token, timer := route.idleToken, route.idleTimer
	route.mu.Unlock()
	if timer != nil {
		t.Fatal("a kept route armed an idle clock")
	}
	route.idleExpired(token)
	f.assertStatus(t, protocol.DemandRunning, "S001", false)
	if started, stopped := f.sessions.counts(); started != 1 || stopped != 0 {
		t.Fatalf("started %d, stopped %d; want one start and no stop", started, stopped)
	}
	if got := strings.Count(f.log.String(), "daemon: route /dev: started session S001 (kept route startup)\n"); got != 1 {
		t.Fatalf("start logged %d times, want once: %q", got, f.log.String())
	}
}

// A crash loop of any length waits exactly the bounded delay before each
// restart, and nothing before the deadline starts it.
func TestKeptCrashLoopWaitsTheBoundedDelay(t *testing.T) {
	f := newKeptFixture(t)
	f.syncKept(t)
	for failure := 1; failure <= 100; failure++ {
		f.crash(t, 1)
		want := expectedKeptDelay(failure)
		if got := f.restartIn(); got != want {
			t.Fatalf("failure %d: restart in %s, want %s", failure, got, want)
		}
		route := f.route()
		route.mu.Lock()
		failures := route.restartFailures
		route.mu.Unlock()
		if failures > keptRestartSteps {
			t.Fatalf("failure count %d grew past its cap %d", failures, keptRestartSteps)
		}
		started := f.started()
		f.clock.advance(want - time.Nanosecond)
		f.manager.supervise()
		if f.started() != started {
			t.Fatalf("failure %d: restarted before its delay", failure)
		}
		f.clock.advance(time.Nanosecond)
		f.manager.supervise()
		settleDemand(t, f.manager)
		if f.started() != started+1 || f.manager.Status("dev").State != protocol.DemandRunning {
			t.Fatalf("failure %d: started %d, status %+v; want one restart at the deadline", failure, f.started(), f.manager.Status("dev"))
		}
	}
}

func TestKeptBackoffResetsOnlyAfterAStableRunOrStart(t *testing.T) {
	f := newKeptFixture(t)
	f.syncKept(t)
	f.crash(t, 1)
	f.clock.advance(time.Second)
	f.manager.supervise()
	settleDemand(t, f.manager)
	// S002 runs for two minutes: its exit starts the count over.
	f.clock.advance(2 * time.Minute)
	f.crash(t, 1)
	if got := f.restartIn(); got != time.Second {
		t.Fatalf("restart after a stable run in %s, want 1s", got)
	}
	f.clock.advance(time.Second)
	f.manager.supervise()
	settleDemand(t, f.manager)
	// S003 ends at once. The earlier session's long run does not count.
	f.crash(t, 1)
	if got := f.restartIn(); got != 2*time.Second {
		t.Fatalf("restart after a short run in %s, want 2s", got)
	}
	// Traffic changes nothing; mesh serve start overrides and resets.
	for range 3 {
		_, _ = f.manager.Enter(context.Background(), "dev")
	}
	if got := f.restartIn(); got != 2*time.Second {
		t.Fatalf("traffic moved the restart to %s", got)
	}
	if err := f.manager.Start(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	f.crash(t, 1)
	if got := f.restartIn(); got != time.Second {
		t.Fatalf("restart after mesh serve start in %s, want the reset 1s", got)
	}
}

// An adopted session's stability clock starts at adoption.
func TestKeptAdoptedSessionStabilityStartsAtAdoption(t *testing.T) {
	for _, test := range []struct {
		name  string
		ran   time.Duration
		delay time.Duration
	}{
		{name: "short", ran: 59 * time.Second, delay: expectedKeptDelay(6)},
		{name: "stable", ran: 61 * time.Second, delay: time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertAdoptedStability(t, test.ran, test.delay)
		})
	}
}

// keptBackoffCase leaves a kept route waiting to restart in a given way.
type keptBackoffCase struct {
	name    string
	prepare func(t *testing.T, f keptFixture)
}

var keptBackoffCases = []keptBackoffCase{
	{name: "launch failed without a worker", prepare: func(t *testing.T, f keptFixture) {
		f.sessions.noWorkerErr = errors.New("fork: resource temporarily unavailable")
		f.syncKept(t)
		f.sessions.mu.Lock()
		f.sessions.noWorkerErr = nil
		f.sessions.mu.Unlock()
	}},
	{name: "exited before ready", prepare: func(t *testing.T, f keptFixture) {
		code := 3
		f.sessions.exitNow = &code
		f.syncKept(t)
		f.sessions.mu.Lock()
		f.sessions.exitNow = nil
		f.sessions.mu.Unlock()
	}},
	{name: "clean exit while serving", prepare: func(t *testing.T, f keptFixture) {
		f.syncKept(t)
		f.crash(t, 0)
		if status := f.manager.Status("dev"); status.State != protocol.DemandFailed || !strings.Contains(status.Failure, "status 0") || !strings.Contains(status.Failure, "restarts at") {
			t.Fatalf("status after a clean exit = %+v, want a failure naming the restart", status)
		}
	}},
}

// No traffic path bypasses or moves a kept route's restart delay: tailnet
// requests, a new listener connection, and a request on an open one all get
// the same logged answer, and the deadline brings exactly one launch.
func assertAdoptedStability(t *testing.T, ran, delay time.Duration) {
	t.Helper()
	f := newKeptFixture(t)
	f.sessions.adoptID, f.sessions.adoptFor = "OLD1", "serve /dev"
	f.syncKept(t)
	f.assertStatus(t, protocol.DemandRunning, "OLD1", false)
	route := f.route()
	route.mu.Lock()
	route.restartFailures = 5
	route.mu.Unlock()
	f.clock.advance(ran)
	f.crash(t, 1)
	if got := f.restartIn(); got != delay {
		t.Fatalf("restart in %s, want %s", got, delay)
	}
	if f.started() != 0 {
		t.Fatal("adoption launched a second worker")
	}
}

func TestKeptTrafficCannotBypassTheRestartDelay(t *testing.T) {
	for _, test := range keptBackoffCases {
		t.Run(test.name, func(t *testing.T) {
			assertTrafficKeepsRestartDelay(t, test)
		})
	}
}

// A start that leaves a worker it could not confirm stopped blocks every
// launch until cleanup proves it gone, however late the restart deadline is.
func assertTrafficKeepsRestartDelay(t *testing.T, test keptBackoffCase) {
	t.Helper()
	f := newKeptFixture(t)
	test.prepare(t, f)
	delay, started := f.restartIn(), f.started()
	if delay != time.Second {
		t.Fatalf("restart in %s, want 1s", delay)
	}
	before := strings.Count(f.log.String(), "on-demand failure")
	references := map[string]bool{}
	answer := func(err error) {
		t.Helper()
		if err == nil {
			t.Fatal("a request started a kept route during its restart delay")
		}
		response := httptest.NewRecorder()
		meshserve.WriteDemandFailure(response, err, f.manager.logger)
		references[response.Body.String()] = true
	}
	for range 3 {
		_, err := f.manager.Enter(context.Background(), "dev")
		answer(err)
	}
	release, began := f.route().hold(listenerCause(5173))
	answer(f.route().awaitStart(context.Background(), began, listenerCause(5173)))
	answer(f.route().ready(context.Background(), listenerCause(5173)))
	release()
	if f.started() != started || f.restartIn() != delay {
		t.Fatalf("traffic started %d sessions and moved the restart to %s", f.started()-started, f.restartIn())
	}
	if got := strings.Count(f.log.String(), "on-demand failure") - before; got != 1 || len(references) != 1 {
		t.Fatalf("logged %d diagnostics with %d references for one restart delay, want 1 and 1", got, len(references))
	}
	f.clock.advance(delay)
	var arrivals sync.WaitGroup
	for range 8 {
		arrivals.Go(func() { enterAndReleaseDemand(f.manager) })
	}
	arrivals.Go(f.manager.supervise)
	arrivals.Wait()
	settleDemand(t, f.manager)
	if f.started() != started+1 {
		t.Fatalf("arrivals at the deadline started %d sessions, want 1", f.started()-started)
	}
}

func TestKeptRestartWaitsForUncertainCleanup(t *testing.T) {
	f := newKeptFixture(t)
	f.sessions.startErr = errors.New("readiness: worker did not answer")
	f.syncKept(t)
	f.sessions.mu.Lock()
	f.sessions.startErr = nil
	f.sessions.mu.Unlock()
	f.sessions.failStops(errWorkerSilent)
	f.clock.advance(time.Hour)
	for range 3 {
		f.manager.supervise()
		settleDemand(t, f.manager)
		if _, err := f.manager.Enter(context.Background(), "dev"); err == nil {
			t.Fatal("a request started the route while its first worker may still run")
		}
	}
	if f.started() != 1 || f.sessions.liveCount() != 1 {
		t.Fatalf("started %d with %d live while cleanup failed, want 1 and 1", f.started(), f.sessions.liveCount())
	}
	f.sessions.failStops(nil)
	f.manager.supervise()
	settleDemand(t, f.manager)
	if f.started() != 2 || f.sessions.liveCount() != 1 {
		t.Fatalf("after cleanup started %d with %d live, want one replacement", f.started(), f.sessions.liveCount())
	}
}

func TestKeptStopDuringPendingStartHoldsAfterCancelledWaiter(t *testing.T) {
	f := newKeptFixture(t)
	ready := newDemandBarrier(t)
	f.manager.dial = func(context.Context, string) error { ready.wait(); return nil }
	f.manager.Sync([]meshserve.Service{keptService()})
	awaitDemandSignal(t, ready.entered, "kept start readiness")
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	stop := beginAdoptionCall(t, func(context.Context) error { return f.manager.Stop(ctx, "dev") })
	awaitRoute(t, f.route(), "Stop accepted", stopAccepted)
	cancel()
	if err := stop.wait(t); !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "waiting for route /dev to start") {
		t.Fatalf("Stop = %v, want a cancelled wait for the start", err)
	}
	ready.unblock()
	settleDemand(t, f.manager)
	f.assertStatus(t, protocol.DemandStopped, "S001", true)
	f.clock.advance(time.Hour)
	f.manager.supervise()
	if _, err := f.manager.Enter(context.Background(), "dev"); err == nil {
		t.Fatal("a request started a held route")
	}
	if started, stopped := f.sessions.counts(); started != 1 || stopped != 1 || f.sessions.liveCount() != 0 {
		t.Fatalf("started %d, stopped %d, live %d; want the start stopped once and held", started, stopped, f.sessions.liveCount())
	}
	if !strings.Contains(f.log.String(), "daemon: route /dev: stopped session S001 (mesh serve stop)\n") {
		t.Fatalf("log lacks the stop: %q", f.log.String())
	}
}

func TestKeptStopDuringCleanupHoldsAfterCancelledWaiter(t *testing.T) {
	f := newKeptFixture(t)
	f.syncKept(t)
	stopping := newDemandBarrier(t)
	f.sessions.mu.Lock()
	f.sessions.stopGate = stopping
	f.sessions.mu.Unlock()
	changed := keptService()
	changed.Demand.Command = "bun run dev --port 1"
	f.manager.Sync([]meshserve.Service{changed})
	awaitDemandSignal(t, stopping.entered, "redefinition stop")
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	stop := beginAdoptionCall(t, func(context.Context) error { return f.manager.Stop(ctx, "dev") })
	awaitRoute(t, f.route(), "Stop accepted", stopAccepted)
	cancel()
	if err := stop.wait(t); !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "to stop session S001") {
		t.Fatalf("Stop = %v, want a cancelled wait naming session S001", err)
	}
	stopping.unblock()
	settleDemand(t, f.manager)
	f.assertStatus(t, protocol.DemandStopped, "S001", true)
	f.clock.advance(time.Hour)
	f.manager.supervise()
	if started, stopped := f.sessions.counts(); started != 1 || stopped != 1 {
		t.Fatalf("started %d, stopped %d; want the held route left stopped", started, stopped)
	}
}

func TestKeptFailedStopStaysHeldAndOwned(t *testing.T) {
	f := newKeptFixture(t)
	f.syncKept(t)
	f.sessions.failStops(errWorkerSilent)
	if err := f.manager.Stop(context.Background(), "dev"); !errors.Is(err, errWorkerSilent) {
		t.Fatalf("Stop = %v, want the stop failure", err)
	}
	f.assertStatus(t, protocol.DemandFailed, "S001", true)
	for range 3 {
		f.clock.advance(time.Hour)
		f.manager.supervise()
		settleDemand(t, f.manager)
	}
	if started, stopped := f.sessions.counts(); started != 1 || stopped != 4 {
		t.Fatalf("started %d, stopped %d; want retried cleanup and no launch", started, stopped)
	}
	f.assertStatus(t, protocol.DemandFailed, "S001", true)
	if strings.Contains(f.log.String(), "stopped session S001") || !strings.Contains(f.log.String(), "stopping session S001 failed (mesh serve stop)") {
		t.Fatalf("log = %q, want only the failure for S001", f.log.String())
	}
	f.sessions.failStops(nil)
	f.manager.supervise()
	settleDemand(t, f.manager)
	f.assertStatus(t, protocol.DemandStopped, "S001", true)
	f.manager.supervise()
	if f.started() != 1 {
		t.Fatal("a held route started once its cleanup finished")
	}
}

func TestKeptStopDuringBackoffSettlesHeldWithoutClaimingAStop(t *testing.T) {
	f := newKeptFixture(t)
	f.syncKept(t)
	f.crash(t, 1)
	if err := f.manager.Stop(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	status := f.manager.Status("dev")
	if status.State != protocol.DemandStopped || !status.Held || status.Failure != "" {
		t.Fatalf("status = %+v, want stopped and held without the restart notice", status)
	}
	if f.restartIn() != 0 {
		t.Fatal("a held route kept its restart deadline")
	}
	f.clock.advance(time.Hour)
	f.manager.supervise()
	if started, stopped := f.sessions.counts(); started != 1 || stopped != 0 {
		t.Fatalf("started %d, stopped %d; want nothing started or stopped", started, stopped)
	}
	if strings.Contains(f.log.String(), "stopped session") {
		t.Fatalf("log claims a stop of a session that had already ended: %q", f.log.String())
	}
	if err := f.manager.Start(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	f.assertStatus(t, protocol.DemandRunning, "S002", false)
}

// Stop, then Start, while a start is pending: the Stop finishes, then the
// route launches once more and the Start's waiter gets that session.
func TestKeptStopThenStartLaunchesOnceAfterTheStop(t *testing.T) {
	f := newKeptFixture(t)
	ready := newDemandBarrier(t)
	f.manager.dial = func(context.Context, string) error { ready.wait(); return nil }
	f.manager.Sync([]meshserve.Service{keptService()})
	awaitDemandSignal(t, ready.entered, "kept start readiness")
	stop := beginAdoptionCall(t, func(ctx context.Context) error { return f.manager.Stop(ctx, "dev") })
	awaitRoute(t, f.route(), "Stop accepted", stopAccepted)
	start := beginAdoptionCall(t, func(ctx context.Context) error { return f.manager.Start(ctx, "dev") })
	awaitRoute(t, f.route(), "Start accepted after the Stop", startAccepted)
	ready.unblock()
	if err := start.wait(t); err != nil {
		t.Fatalf("Start = %v", err)
	}
	if err := stop.wait(t); err != nil {
		t.Fatalf("Stop = %v", err)
	}
	settleDemand(t, f.manager)
	f.assertStatus(t, protocol.DemandRunning, "S002", false)
	if started, stopped := f.sessions.counts(); started != 2 || stopped != 1 || f.sessions.liveCount() != 1 {
		t.Fatalf("started %d, stopped %d, live %d; want the stop then one launch", started, stopped, f.sessions.liveCount())
	}
}

// Start, then Stop: the later Stop wins and the route ends held.
func TestKeptStartThenStopEndsHeld(t *testing.T) {
	f := newKeptFixture(t)
	f.syncKept(t)
	if err := f.manager.Stop(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	ready := newDemandBarrier(t)
	f.manager.dial = func(context.Context, string) error { ready.wait(); return nil }
	start := beginAdoptionCall(t, func(ctx context.Context) error { return f.manager.Start(ctx, "dev") })
	awaitDemandSignal(t, ready.entered, "explicit start readiness")
	stop := beginAdoptionCall(t, func(ctx context.Context) error { return f.manager.Stop(ctx, "dev") })
	awaitRoute(t, f.route(), "Stop accepted after the Start", stopAccepted)
	ready.unblock()
	if err := start.wait(t); err == nil {
		t.Fatal("Start succeeded although a later Stop ended its session")
	}
	if err := stop.wait(t); err != nil {
		t.Fatalf("Stop = %v", err)
	}
	settleDemand(t, f.manager)
	f.assertStatus(t, protocol.DemandStopped, "S002", true)
	f.clock.advance(time.Hour)
	f.manager.supervise()
	if started, stopped := f.sessions.counts(); started != 2 || stopped != 2 {
		t.Fatalf("started %d, stopped %d; want both sessions stopped and none restarted", started, stopped)
	}
}

func unrelatedService(target, command string) meshserve.Service {
	return meshserve.Service{
		Name: "api", Kind: meshserve.Proxy, Target: target,
		Listens: []meshserve.Listen{{Public: 3001, Upstream: 13001}},
		Demand:  &meshserve.Demand{Command: command, Cwd: "/tmp", Idle: time.Minute, ReadyTimeout: time.Second},
	}
}

// Sync runs for every catalog change. A held route stays held through all
// of them; only its own operational edit releases it.
func TestKeptHoldSurvivesUnrelatedSync(t *testing.T) {
	f := newKeptFixture(t)
	f.syncKept(t)
	if err := f.manager.Stop(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	kept := keptService()
	labelled := keptService()
	labelled.DisplayName = "Model proxy"
	relabelledB := unrelatedService("3000", "bun run api")
	relabelledB.DisplayName = "API"
	for _, catalog := range [][]meshserve.Service{
		{kept},
		{kept, unrelatedService("3000", "bun run api")},
		{kept, unrelatedService("3000", "bun run api --watch")},
		{kept, relabelledB},
		{kept},
		{labelled},
		{labelled},
	} {
		f.manager.Sync(catalog)
		f.manager.supervise()
		settleDemand(t, f.manager)
		f.assertStatus(t, protocol.DemandStopped, "S001", true)
	}
	f.clock.advance(time.Hour)
	f.manager.supervise()
	if f.started() != 1 {
		t.Fatalf("unrelated catalog changes started the held route %d more times", f.started()-1)
	}
	changed := labelled
	changed.Demand = &meshserve.Demand{Command: "bun run dev --port 1", Cwd: "/tmp", ReadyTimeout: time.Second, KeepRunning: true}
	f.manager.Sync([]meshserve.Service{changed})
	settleDemand(t, f.manager)
	f.assertStatus(t, protocol.DemandRunning, "S002", false)
}

// A rejected mutation still synchronizes the unchanged catalog on its way
// out. That Sync is identical, so it keeps the hold.
func TestKeptHoldSurvivesDeferredSyncOfRejectedMutation(t *testing.T) {
	_, registry, controller := newServiceControllerTest(t, "/kept-hold")
	f := newKeptFixture(t)
	controller.demand = f.manager
	kept := keptService()
	kept.Demand.Cwd = t.TempDir()
	if err := registry.Replace([]meshserve.Service{kept}); err != nil {
		t.Fatal(err)
	}
	f.manager.Sync(registry.Services())
	settleDemand(t, f.manager)
	if err := f.manager.Stop(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	other := protocol.ServiceDefinitionInfo(meshserve.Service{Name: "api", Kind: meshserve.Proxy, Target: "3000"})
	stale := other
	stale.Target = "3999"
	_, _, err := controller.HandleControl(context.Background(), protocol.Control{
		Type: protocol.TypeServiceUpsert, RequestID: "kept-hold-1", Service: &other,
		ServicePreview: &protocol.ServicePreview{Service: stale},
	})
	if err == nil || !strings.Contains(err.Error(), "changed after preview") {
		t.Fatalf("upsert = %v, want the rejected preview after its deferred Sync", err)
	}
	f.manager.supervise()
	f.assertStatus(t, protocol.DemandStopped, "S001", true)
	if f.started() != 1 {
		t.Fatal("the deferred Sync of a rejected mutation restarted a held route")
	}
}

// Changing only lifetime keeps a live session and its ID, in either
// direction, and takes up the new policy at once.
func TestDemandLifetimeChangeKeepsTheRunningSession(t *testing.T) {
	f := newKeptFixture(t)
	finite := demandService(time.Hour)
	f.manager.Sync([]meshserve.Service{finite})
	if err := f.manager.Start(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	route := f.route()
	route.mu.Lock()
	staleToken := route.idleToken
	route.mu.Unlock()
	f.manager.Sync([]meshserve.Service{keptService()})
	settleDemand(t, f.manager)
	route.idleExpired(staleToken)
	route.mu.Lock()
	timer := route.idleTimer
	route.mu.Unlock()
	if timer != nil {
		t.Fatal("a route switched to kept still has an idle clock")
	}
	f.assertStatus(t, protocol.DemandRunning, "S001", false)
	f.manager.Sync([]meshserve.Service{finite})
	route.mu.Lock()
	timer = route.idleTimer
	route.mu.Unlock()
	if timer == nil {
		t.Fatal("a live route switched to finite with no connections did not arm its idle clock")
	}
	f.assertStatus(t, protocol.DemandRunning, "S001", false)
	if started, stopped := f.sessions.counts(); started != 1 || stopped != 0 {
		t.Fatalf("lifetime changes started %d and stopped %d sessions, want 1 and 0", started, stopped)
	}
}

func TestDemandLifetimeChangeWhileNotRunning(t *testing.T) {
	t.Run("finite to kept while stopped starts now", func(t *testing.T) {
		f := newKeptFixture(t)
		f.manager.Sync([]meshserve.Service{demandService(time.Hour)})
		f.manager.Sync([]meshserve.Service{keptService()})
		settleDemand(t, f.manager)
		f.assertStatus(t, protocol.DemandRunning, "S001", false)
	})
	t.Run("kept to finite during backoff waits for traffic", func(t *testing.T) {
		f := newKeptFixture(t)
		f.syncKept(t)
		f.crash(t, 1)
		f.manager.Sync([]meshserve.Service{demandService(time.Hour)})
		if f.restartIn() != 0 {
			t.Fatal("a finite route kept a kept route's restart deadline")
		}
		f.assertStatus(t, protocol.DemandStopped, "S001", false)
		f.clock.advance(time.Hour)
		f.manager.supervise()
		if f.started() != 1 {
			t.Fatal("a finite route restarted without traffic")
		}
	})
	t.Run("finite to kept while starting keeps one worker", func(t *testing.T) {
		f := newKeptFixture(t)
		ready := newDemandBarrier(t)
		f.manager.dial = func(context.Context, string) error { ready.wait(); return nil }
		f.manager.Sync([]meshserve.Service{demandService(time.Hour)})
		start := beginAdoptionCall(t, func(ctx context.Context) error { return f.manager.Start(ctx, "dev") })
		awaitDemandSignal(t, ready.entered, "finite start readiness")
		f.manager.Sync([]meshserve.Service{keptService()})
		ready.unblock()
		if err := start.wait(t); err != nil {
			t.Fatal(err)
		}
		settleDemand(t, f.manager)
		f.manager.supervise()
		f.assertStatus(t, protocol.DemandRunning, "S001", false)
		route := f.route()
		route.mu.Lock()
		timer := route.idleTimer
		route.mu.Unlock()
		if timer != nil || f.started() != 1 {
			t.Fatal("a route kept while starting armed an idle clock or launched twice")
		}
	})
	t.Run("kept to finite while starting arms idle once ready", func(t *testing.T) {
		f := newKeptFixture(t)
		ready := newDemandBarrier(t)
		f.manager.dial = func(context.Context, string) error { ready.wait(); return nil }
		f.manager.Sync([]meshserve.Service{keptService()})
		awaitDemandSignal(t, ready.entered, "kept start readiness")
		f.manager.Sync([]meshserve.Service{demandService(time.Hour)})
		ready.unblock()
		settleDemand(t, f.manager)
		route := f.route()
		route.mu.Lock()
		timer := route.idleTimer
		route.mu.Unlock()
		if timer == nil || f.started() != 1 {
			t.Fatal("a route made finite while starting has no idle clock or launched twice")
		}
	})
	t.Run("finite to kept while cleanup fails waits for cleanup", func(t *testing.T) {
		f := newKeptFixture(t)
		f.manager.Sync([]meshserve.Service{demandService(time.Hour)})
		if err := f.manager.Start(context.Background(), "dev"); err != nil {
			t.Fatal(err)
		}
		f.sessions.failStops(errWorkerSilent)
		if err := f.manager.Stop(context.Background(), "dev"); err == nil {
			t.Fatal("Stop succeeded although the stop failed")
		}
		f.manager.Sync([]meshserve.Service{keptService()})
		settleDemand(t, f.manager)
		f.manager.supervise()
		settleDemand(t, f.manager)
		if f.started() != 1 {
			t.Fatal("a kept route launched while its old worker may still run")
		}
		f.sessions.failStops(nil)
		f.manager.supervise()
		settleDemand(t, f.manager)
		if f.started() != 2 || f.sessions.liveCount() != 1 {
			t.Fatalf("after cleanup started %d with %d live, want one replacement", f.started(), f.sessions.liveCount())
		}
	})
	t.Run("kept recipe change stops the old worker first", func(t *testing.T) {
		f := newKeptFixture(t)
		f.syncKept(t)
		changed := keptService()
		changed.Demand.Command = "bun run dev --port 1"
		f.manager.Sync([]meshserve.Service{changed})
		settleDemand(t, f.manager)
		f.assertStatus(t, protocol.DemandRunning, "S002", false)
		if started, stopped := f.sessions.counts(); started != 2 || stopped != 1 || f.sessions.liveCount() != 1 {
			t.Fatalf("started %d, stopped %d, live %d; want one stop then one start", started, stopped, f.sessions.liveCount())
		}
		if !strings.Contains(f.log.String(), "daemon: route /dev: stopped session S001 (route definition changed)\n") {
			t.Fatalf("log lacks the redefinition stop: %q", f.log.String())
		}
	})
}

// After Close or a cancelled daemon context, nothing launches and no stop
// owed by a pending start, removal or Stop is sent. The worker survives and
// the next manager adopts it without launching another.
func TestKeptShutdownKeepsTheWorkerForTheNextDaemon(t *testing.T) {
	for _, test := range []struct {
		name    string
		owe     func(t *testing.T, f keptFixture)
		closeIt bool
	}{
		{name: "removed then closed", closeIt: true, owe: func(_ *testing.T, f keptFixture) { f.manager.Sync(nil) }},
		{name: "cancelled stop then closed", closeIt: true, owe: cancelledKeptStop},
		{name: "cancelled stop then context ended", owe: cancelledKeptStop},
		{name: "redefined then context ended", owe: func(_ *testing.T, f keptFixture) {
			changed := keptService()
			changed.Demand.Command = "bun run dev --port 1"
			f.manager.Sync([]meshserve.Service{changed})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertKeptShutdownSurvival(t, test.owe, test.closeIt)
		})
	}
}

func assertKeptShutdownSurvival(t *testing.T, owe func(*testing.T, keptFixture), closeIt bool) {
	t.Helper()
	sessions := newFakeDemandSessions()
	daemon, cancelDaemon := context.WithCancel(t.Context())
	t.Cleanup(cancelDaemon)
	manager := demandManagerWithParent(daemon, t, sessions)
	f := keptFixture{sessions: sessions, manager: manager, clock: newDemandClock(manager), log: &demandLogSink{}}
	manager.logger = log.New(f.log, "", 0)
	ready := newDemandBarrier(t)
	manager.dial = func(context.Context, string) error { ready.wait(); return nil }
	manager.Sync([]meshserve.Service{keptService()})
	awaitDemandSignal(t, ready.entered, "kept start readiness")
	owe(t, f)
	if closeIt {
		manager.Close()
	} else {
		cancelDaemon()
	}
	ready.unblock()
	joinAdoptionTransitions(t, manager)
	manager.supervise()
	if started, stopped := sessions.counts(); started != 1 || stopped != 0 || sessions.liveCount() != 1 {
		t.Fatalf("started %d, stopped %d, live %d; want the worker kept", started, stopped, sessions.liveCount())
	}
	manager.Close()
	manager.goroutines.Wait()

	sessions.adoptID, sessions.adoptFor = "S001", "serve /dev"
	next := testDemandManager(t, sessions, func() bool { return true })
	next.Sync([]meshserve.Service{keptService()})
	settleDemand(t, next)
	if status := next.Status("dev"); status.SessionID != "S001" || status.State != protocol.DemandRunning {
		t.Fatalf("next daemon status = %+v, want S001 adopted", status)
	}
	if started, _ := sessions.counts(); started != 1 {
		t.Fatal("the next daemon launched beside the surviving worker")
	}
}

func cancelledKeptStop(t *testing.T, f keptFixture) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	stop := beginAdoptionCall(t, func(context.Context) error { return f.manager.Stop(ctx, "dev") })
	awaitRoute(t, f.route(), "Stop accepted", stopAccepted)
	cancel()
	_ = stop.wait(t)
}

// Close waits for a kill it already sent and then sends no new ones; the
// stop's completion launches nothing.
func TestKeptCloseJoinsAnAdmittedKill(t *testing.T) {
	f := newKeptFixture(t)
	f.syncKept(t)
	stopping := newDemandBarrier(t)
	f.sessions.mu.Lock()
	f.sessions.stopGate = stopping
	f.sessions.mu.Unlock()
	changed := keptService()
	changed.Demand.Command = "bun run dev --port 1"
	f.manager.Sync([]meshserve.Service{changed})
	awaitDemandSignal(t, stopping.entered, "redefinition stop")
	events := &demandEvents{}
	closed := make(chan struct{})
	go func() {
		f.manager.Close()
		events.add("close returned")
		close(closed)
	}()
	t.Cleanup(func() { stopping.unblock(); <-closed })
	select {
	case <-closed:
	case <-time.After(20 * time.Millisecond):
	}
	events.add("kill released")
	stopping.unblock()
	awaitDemandSignal(t, closed, "Close")
	settleDemand(t, f.manager)
	if got := events.list(); len(got) != 2 || got[0] != "kill released" {
		t.Fatalf("events = %v, want Close to wait for the admitted kill", got)
	}
	if started, stopped := f.sessions.counts(); started != 1 || stopped != 1 {
		t.Fatalf("started %d, stopped %d; want the redefinition stop and no new launch", started, stopped)
	}
}

func TestKeptAdoptionDuringCloseLaunchesNothing(t *testing.T) {
	service := keptService()
	sessions, manager := unresolvedAdoption(t, service)
	sessions.setLookup(nil, "", false)
	gate := sessions.blockLookup(t)
	manager.supervise()
	awaitAdoptionSignal(t, gate.entered)
	manager.Close()
	gate.unblock()
	joinAdoptionTransitions(t, manager)
	manager.supervise()
	assertAdoptionCounts(t, sessions, 0, 0)
}

// The lifecycle log names the exact session and a fixed cause for each
// completed transition, once, and never the recipe, its output or a peer.
func TestDemandLifecycleLogIsExactAndSanitized(t *testing.T) {
	f := newKeptFixture(t)
	f.sessions.output = "SENTINEL_OUTPUT"
	f.sessions.adoptID, f.sessions.adoptFor = "OLD1", "serve /dev"
	service := keptService()
	service.Demand.Command, service.Demand.Cwd = "SENTINEL_COMMAND", "/SENTINEL_CWD"
	service.Demand.Env = []string{"SENTINEL_ENV=SENTINEL_VALUE"}
	f.manager.Sync([]meshserve.Service{service})
	settleDemand(t, f.manager)
	f.crash(t, 1)
	f.manager.supervise()
	f.clock.advance(time.Second)
	f.manager.supervise()
	settleDemand(t, f.manager)
	f.sessions.failStops(errWorkerSilent)
	_ = f.manager.Stop(context.Background(), "dev")
	f.sessions.failStops(nil)
	f.manager.supervise()
	settleDemand(t, f.manager)
	code := 9
	f.sessions.exitNow = &code
	_ = f.manager.Start(context.Background(), "dev")
	settleDemand(t, f.manager)
	logged := f.log.String()
	for _, want := range []string{
		"daemon: route /dev: adopted session OLD1 from an earlier daemon\n",
		"daemon: route /dev: session OLD1 ended (the command exited with status 1); restarting in 1s\n",
		"daemon: route /dev: started session S001 (kept route restart)\n",
		"daemon: route /dev: stopping session S001 failed (mesh serve stop); it may still be running and is stopped again later\n",
		"daemon: route /dev: stopped session S001 (mesh serve stop)\n",
	} {
		if got := strings.Count(logged, want); got != 1 {
			t.Fatalf("logged %q %d times, want once:\n%s", want, got, logged)
		}
	}
	for line := range strings.Lines(logged) {
		if strings.Contains(line, "on-demand failure") {
			continue
		}
		assertLifecycleLineSanitized(t, line)
	}
}

func assertLifecycleLineSanitized(t *testing.T, line string) {
	t.Helper()
	for _, sentinel := range []string{"SENTINEL", "127.0.0.1"} {
		if strings.Contains(line, sentinel) {
			t.Fatalf("lifecycle line carries %s: %q", sentinel, line)
		}
	}
}

func TestDemandReadinessCleanupLogsItsFixedCause(t *testing.T) {
	f := newKeptFixture(t)
	f.manager.dial = func(context.Context, string) error { return errWorkerSilent }
	service := demandService(time.Hour)
	service.Demand.ReadyTimeout = 30 * time.Millisecond
	f.manager.Sync([]meshserve.Service{service})
	if err := f.manager.Start(context.Background(), "dev"); err == nil {
		t.Fatal("a start that never became ready succeeded")
	}
	if !strings.Contains(f.log.String(), "daemon: route /dev: stopped session S001 (readiness failure cleanup)\n") {
		t.Fatalf("log lacks the readiness cleanup: %q", f.log.String())
	}
}

func TestDemandCancelledWaitsNameWhatTheyWaitedFor(t *testing.T) {
	f := newKeptFixture(t)
	ready := newDemandBarrier(t)
	f.manager.dial = func(context.Context, string) error { ready.wait(); return nil }
	f.manager.Sync([]meshserve.Service{demandService(time.Hour)})
	ctx, cancel := context.WithCancel(t.Context())
	entered := make(chan struct{})
	f.manager.entered = func() { close(entered) }
	enter := beginAdoptionCall(t, func(context.Context) error {
		_, err := f.manager.Enter(ctx, "dev")
		return err
	})
	awaitDemandSignal(t, entered, "Enter counting its connection")
	cancel()
	if err := enter.wait(t); !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "waiting for route /dev to start") {
		t.Fatalf("Enter = %v, want a cancelled wait for the start", err)
	}
	ready.unblock()
	settleDemand(t, f.manager)
}

// A slow log never holds the route lock or a listener's accept: status and
// new connections proceed while a lifecycle line is still being written.
func TestDemandSlowLogHoldsNoRouteLock(t *testing.T) {
	f := newKeptFixture(t)
	resume := make(chan struct{})
	var release sync.Once
	unblock := func() { release.Do(func() { close(resume) }) }
	t.Cleanup(unblock)
	sink := &demandLogSink{blocked: make(chan struct{}), resume: resume}
	f.manager.logger = log.New(sink, "", 0)
	f.manager.Sync([]meshserve.Service{keptService()})
	awaitDemandSignal(t, sink.blocked, "the start's log line")
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = f.manager.Status("dev")
		releaseConnection, _ := f.route().hold(listenerCause(5173))
		releaseConnection()
	}()
	awaitDemandSignal(t, done, "status and a connection while the log is blocked")
	unblock()
	settleDemand(t, f.manager)
	if !strings.Contains(sink.String(), "started session S001") {
		t.Fatalf("log = %q", sink.String())
	}
}

func TestDemandStatusShowsHeldOnlyForKeptRoutes(t *testing.T) {
	f := newKeptFixture(t)
	f.manager.Sync([]meshserve.Service{demandService(time.Hour)})
	if err := f.manager.Start(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	if err := f.manager.Stop(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	f.assertStatus(t, protocol.DemandStopped, "S001", false)
	if release, err := f.manager.Enter(context.Background(), "dev"); err != nil {
		t.Fatalf("a finite route did not start on its next connection after Stop: %v", err)
	} else {
		release()
	}
	f.assertStatus(t, protocol.DemandRunning, "S002", false)
	f.manager.Sync([]meshserve.Service{keptService()})
	if err := f.manager.Stop(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	f.assertStatus(t, protocol.DemandStopped, "S002", true)
}

func enterAndReleaseDemand(manager *demandManager) {
	if release, err := manager.Enter(context.Background(), "dev"); err == nil {
		release()
	}
}
