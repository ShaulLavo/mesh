package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
	meshserve "github.com/shaul/mesh/internal/serve"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/storage"
)

type runtimeSessions struct {
	*fakeDemandSessions
	refsMu          sync.Mutex
	refs            map[string]session.Reservation
	times           map[string]time.Time
	unknown         map[string]bool
	now             func() time.Time
	boot            string
	reserveErr      error
	beforeStart     func(session.Reservation, meshserve.Service)
	observationGate *demandBarrier
}

func newRuntimeSessions() *runtimeSessions {
	return &runtimeSessions{fakeDemandSessions: newFakeDemandSessions(), refs: map[string]session.Reservation{}, times: map[string]time.Time{}, unknown: map[string]bool{}, now: time.Now, boot: "boot-a"}
}
func (s *runtimeSessions) reserveLabelled(context.Context) (session.Reservation, error) {
	s.mu.Lock()
	id := fmt.Sprintf("S%03d", s.next+1)
	s.mu.Unlock()
	if s.reserveErr != nil {
		return session.Reservation{}, s.reserveErr
	}
	ref, err := session.NewReservation(id, s.boot, session.Managed)
	if err != nil {
		return ref, fmt.Errorf("fixture reserve: %w", err)
	}
	return ref, nil
}
func (s *runtimeSessions) startReserved(ctx context.Context, ref session.Reservation, service meshserve.Service) error {
	if s.beforeStart != nil {
		s.beforeStart(ref, service)
	}
	s.refsMu.Lock()
	s.refs[ref.ID] = ref
	s.refsMu.Unlock()
	id, err := s.startLabelled(ctx, service.Label(), []string{"sh", "-lc", service.Demand.Command}, service.Demand.Cwd, service.Demand.Env)
	if id == "" {
		return &recovery.LaunchFailure{Err: err}
	}
	if id != ref.ID {
		return fmt.Errorf("fixture instance mismatch")
	}
	return err
}
func (s *runtimeSessions) ownedFate(ref session.Reservation) ownedObservation {
	s.refsMu.Lock()
	current, exists := s.refs[ref.ID]
	unknown, at, boot := s.unknown[ref.ID], s.times[ref.ID], s.boot
	gate := s.observationGate
	s.refsMu.Unlock()
	if gate != nil {
		gate.wait()
	}
	if ref.BootID != "" && boot != "" && ref.BootID != boot {
		return ownedObservation{fate: sessionEnded, interrupted: true}
	}
	if !exists || current != ref || unknown {
		return ownedObservation{}
	}
	if code, ended := s.sessionExit(ref.ID); ended {
		return ownedObservation{fate: sessionEnded, code: code, exitedAt: at}
	}
	return ownedObservation{fate: sessionLive}
}
func (s *runtimeSessions) stopOwned(ctx context.Context, ref session.Reservation) error {
	if s.ownedFate(ref).fate == sessionUnknown {
		return fmt.Errorf("unknown exact instance")
	}
	return s.stopSession(ctx, ref.ID)
}
func (s *runtimeSessions) crashAt(id string, at time.Time) {
	s.refsMu.Lock()
	s.times[id] = at
	s.refsMu.Unlock()
	s.crash(id, 1)
}

type runtimeFixture struct {
	store    *storage.Store
	sessions *runtimeSessions
	manager  *demandManager
	clock    *demandClock
	service  meshserve.Service
}

func newRuntimeFixture(t *testing.T) *runtimeFixture {
	t.Helper()
	store, err := storage.Open(t.Context(), filepath.Join(t.TempDir(), "mesh.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	service, err := meshserve.Normalize(keptService())
	if err != nil {
		t.Fatal(err)
	}
	f := &runtimeFixture{store: store, sessions: newRuntimeSessions(), service: service}
	f.manager = f.open(t, store)
	f.clock = newDemandClock(f.manager)
	f.sessions.now = f.clock.read
	return f
}
func (f *runtimeFixture) open(t *testing.T, store serviceRuntimeStore) *demandManager {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	m := newDemandManager(ctx, f.sessions, func(error) {})
	m.bind = func(uint16) (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") }
	m.dial = func(context.Context, string) error { return nil }
	m.poll, m.cleanupRetry, m.adoptionRetry = time.Millisecond, 0, 0
	if f.clock != nil {
		m.now = f.clock.read
	}
	if err := m.restoreRuntime(store); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); m.Close(); m.goroutines.Wait() })
	return m
}
func (f *runtimeFixture) sync(t *testing.T) {
	t.Helper()
	f.manager.Sync([]meshserve.Service{f.service})
	settleDemand(t, f.manager)
}
func (f *runtimeFixture) reopen(t *testing.T) {
	t.Helper()
	f.manager.Close()
	f.manager = f.open(t, f.store)
	f.sync(t)
}
func (f *runtimeFixture) record(t *testing.T) storage.ServiceRuntime {
	t.Helper()
	record, err := f.store.GetServiceRuntime(t.Context(), f.service.Name)
	if err != nil {
		t.Fatal(err)
	}
	return record
}
func (f *runtimeFixture) crash(t *testing.T) {
	t.Helper()
	f.sessions.crashAt(f.manager.Status("dev").SessionID, f.clock.read())
	f.manager.supervise()
	settleDemand(t, f.manager)
}

func TestKeptBackoffSurvivesReopen(t *testing.T) {
	f := newRuntimeFixture(t)
	f.sync(t)
	for failure := 1; failure <= 5; failure++ {
		f.crash(t)
		if failure == 5 {
			break
		}
		f.clock.advance(keptRestartDelay(failure))
		f.manager.supervise()
		settleDemand(t, f.manager)
	}
	f.clock.advance(time.Second)
	f.reopen(t)
	if starts, _ := f.sessions.counts(); starts != 5 {
		t.Fatalf("starts after reopen = %d, want 5", starts)
	}
	record := f.record(t)
	if record.Failures != 5 || record.RestartAt.Sub(f.clock.read()) != 15*time.Second {
		t.Fatalf("restored runtime = %+v", record)
	}
	f.clock.advance(15 * time.Second)
	f.manager.supervise()
	settleDemand(t, f.manager)
	if starts, _ := f.sessions.counts(); starts != 6 {
		t.Fatalf("starts at deadline = %d, want 6", starts)
	}
}

func TestRuntimeDowntimeExitCountedOnce(t *testing.T) {
	for _, stable := range []bool{false, true} {
		t.Run(fmt.Sprint(stable), func(t *testing.T) { testRuntimeDowntimeExit(t, stable) })
	}
}
func testRuntimeDowntimeExit(t *testing.T, stable bool) {
	f := newRuntimeFixture(t)
	f.sync(t)
	before := f.record(t)
	ready := before.Attempt.ReadyAt
	f.manager.Close()
	seeded := cloneRuntime(before)
	seeded.Version++
	seeded.Failures = 4
	if err := f.store.CompareServiceRuntime(t.Context(), &before, &seeded); err != nil {
		t.Fatal(err)
	}
	duration := 5 * time.Second
	if stable {
		duration = 10 * time.Minute
	}
	f.sessions.crashAt("S001", ready.Add(duration))
	f.clock.advance(duration + 2*time.Hour)
	f.reopen(t)
	// The old exit's deadline passed during downtime, so one successor is admitted.
	if starts, _ := f.sessions.counts(); starts != 2 {
		t.Fatalf("starts = %d", starts)
	}
	record := f.record(t)
	wantFailures := 5
	if stable {
		wantFailures = 1
	}
	if record.Failures != wantFailures {
		t.Fatalf("failure streak = %d, want %d", record.Failures, wantFailures)
	}
	f.reopen(t)
	if starts, _ := f.sessions.counts(); starts != 2 {
		t.Fatalf("repeated reopen started %d", starts)
	}
}

func TestRuntimeReopenKeepsReadyOrigin(t *testing.T) {
	f := newRuntimeFixture(t)
	f.sync(t)
	ready := f.record(t).Attempt.ReadyAt
	f.clock.advance(10 * time.Minute)
	f.reopen(t)
	if !f.record(t).Attempt.ReadyAt.Equal(ready) {
		t.Fatal("ready origin changed")
	}
	if starts, stops := f.sessions.counts(); starts != 1 || stops != 0 {
		t.Fatalf("starts %d, stops %d", starts, stops)
	}
}

func TestRuntimeRecipeChangeRetiresExactAttempt(t *testing.T) {
	f := newRuntimeFixture(t)
	f.sync(t)
	f.manager.Close()
	f.service.Demand.Env = []string{"MODE=new"}
	f.manager = f.open(t, f.store)
	f.sync(t)
	if starts, stops := f.sessions.counts(); starts != 2 || stops != 1 {
		t.Fatalf("starts %d stops %d", starts, stops)
	}
	if f.record(t).Attempt.Launched.Demand.Env[0] != "MODE=new" {
		t.Fatal("launched environment was lost")
	}
}

func TestRuntimeReusedInstanceCannotBeAdoptedOrStopped(t *testing.T) {
	f := newRuntimeFixture(t)
	f.sync(t)
	f.manager.Close()
	f.sessions.refsMu.Lock()
	replacement, _ := session.NewReservation("S001", "boot-a", session.Managed)
	f.sessions.refs["S001"] = replacement
	f.sessions.refsMu.Unlock()
	f.reopen(t)
	f.manager.supervise()
	settleDemand(t, f.manager)
	if starts, stops := f.sessions.counts(); starts != 1 || stops != 0 {
		t.Fatalf("starts %d stops %d", starts, stops)
	}
	if f.manager.Status("dev").State == protocol.DemandRunning {
		t.Fatal("reused instance adopted")
	}
}

func TestRuntimeKnownBootInterruptionRetainsStreak(t *testing.T) {
	f := newRuntimeFixture(t)
	f.sync(t)
	f.crash(t)
	f.clock.advance(time.Second)
	f.manager.supervise()
	settleDemand(t, f.manager)
	f.manager.Close()
	f.sessions.refsMu.Lock()
	f.sessions.boot = "boot-b"
	f.sessions.refsMu.Unlock()
	f.reopen(t)
	if record := f.record(t); record.Failures != 2 || record.RestartAt.Sub(f.clock.read()) != 2*time.Second {
		t.Fatalf("interrupted runtime = %+v", record)
	}
}

var errRuntimeWrite = errors.New("fixture runtime write unavailable")

type runtimeFaultStore struct {
	serviceRuntimeStore
	mu             sync.Mutex
	failWrite      bool
	failRead       bool
	committedError bool
	clearCommitted bool
	calls          int
	gate           *demandBarrier
}

func (s *runtimeFaultStore) CompareServiceRuntime(ctx context.Context, before, next *storage.ServiceRuntime) error {
	s.mu.Lock()
	s.calls++
	fail, committed, gate := s.failWrite, s.committedError, s.gate
	if s.clearCommitted {
		s.committedError = false
	}
	s.mu.Unlock()
	if gate != nil {
		gate.wait()
	}
	if fail {
		return errRuntimeWrite
	}
	err := s.serviceRuntimeStore.CompareServiceRuntime(ctx, before, next)
	if err == nil && committed {
		return errRuntimeWrite
	}
	if err != nil {
		return fmt.Errorf("fixture runtime write: %w", err)
	}
	return nil
}
func (s *runtimeFaultStore) GetServiceRuntime(ctx context.Context, name string) (storage.ServiceRuntime, error) {
	s.mu.Lock()
	fail := s.failRead
	s.mu.Unlock()
	if fail {
		return storage.ServiceRuntime{}, errRuntimeWrite
	}
	record, err := s.serviceRuntimeStore.GetServiceRuntime(ctx, name)
	if err != nil {
		return record, fmt.Errorf("fixture runtime read: %w", err)
	}
	return record, nil
}

func TestRuntimeWriteFailureStartsNoProcess(t *testing.T) {
	f := newRuntimeFixture(t)
	fault := &runtimeFaultStore{serviceRuntimeStore: f.store, failWrite: true, failRead: true}
	f.manager.persistence.store = fault
	f.sync(t)
	if starts, _ := f.sessions.counts(); starts != 0 {
		t.Fatalf("started %d with failed reservation write", starts)
	}
	if err := f.manager.Start(t.Context(), "dev"); err == nil {
		t.Fatal("uncertain runtime admitted a control")
	}
}

func TestRuntimeReservationFailureStartsNoProcess(t *testing.T) {
	f := newRuntimeFixture(t)
	f.sessions.reserveErr = errRuntimeWrite
	f.sync(t)
	if starts, _ := f.sessions.counts(); starts != 0 {
		t.Fatalf("started %d after reservation failure", starts)
	}
}

func TestRuntimeReopenEveryPhase(t *testing.T) {
	for _, phase := range []storage.ServicePhase{storage.ServiceStarting, storage.ServiceReadiness, storage.ServiceReady, storage.ServiceCleanup} {
		t.Run(string(phase), func(t *testing.T) { testRuntimeReopenPhase(t, phase) })
	}
}
func testRuntimeReopenPhase(t *testing.T, phase storage.ServicePhase) {
	f := newRuntimeFixture(t)
	f.sync(t)
	f.manager.Close()
	before := f.record(t)
	next := cloneRuntime(before)
	next.Version++
	next.Phase = phase
	if phase == storage.ServiceCleanup {
		oweRuntimeStop(&next, causeReadiness)
	}
	// oweRuntimeStop retains an existing token, so enter cleanup from ready.
	if phase == storage.ServiceCleanup {
		next.Phase = storage.ServiceReady
		oweRuntimeStop(&next, causeReadiness)
	}
	if err := f.store.CompareServiceRuntime(t.Context(), &before, &next); err != nil {
		t.Fatal(err)
	}
	f.reopen(t)
	starts, stops := f.sessions.counts()
	if phase == storage.ServiceCleanup {
		if starts != 1 || stops != 1 || f.record(t).Phase != storage.ServiceNone {
			t.Fatalf("cleanup restored as %d starts %d stops %+v", starts, stops, f.record(t))
		}
		return
	}
	if starts != 1 || stops != 0 || f.manager.Status("dev").State != protocol.DemandRunning {
		t.Fatalf("phase %s: starts %d stops %d status %+v", phase, starts, stops, f.manager.Status("dev"))
	}
}

func TestRuntimeExpiredReadinessKeepsOriginalCleanupDebt(t *testing.T) {
	f := newRuntimeFixture(t)
	f.sync(t)
	f.manager.Close()
	before := f.record(t)
	next := cloneRuntime(before)
	next.Version++
	next.Phase = storage.ServiceReadiness
	if err := f.store.CompareServiceRuntime(t.Context(), &before, &next); err != nil {
		t.Fatal(err)
	}
	f.clock.advance(2 * time.Second)
	f.sessions.failStops(errRuntimeWrite)
	f.reopen(t)
	record := f.record(t)
	if record.Phase != storage.ServiceCleanup || !record.Attempt.Deadline.Equal(before.Attempt.Deadline) {
		t.Fatalf("expired phase = %+v", record)
	}
	token := record.Attempt.StopToken
	f.manager.supervise()
	settleDemand(t, f.manager)
	f.reopen(t)
	if got := f.record(t); got.Phase != storage.ServiceCleanup || got.Attempt.StopToken != token {
		t.Fatalf("cleanup debt changed: %+v", got)
	}
	f.sessions.failStops(nil)
	f.manager.supervise()
	settleDemand(t, f.manager)
	if f.record(t).Attempt != nil {
		t.Fatal("confirmed cleanup retained ownership")
	}
}

func TestRuntimeAcceptedWriteUnknownReadbackBlocksUntilConfirmed(t *testing.T) {
	f := newRuntimeFixture(t)
	f.sync(t)
	fault := &runtimeFaultStore{serviceRuntimeStore: f.store, committedError: true, failRead: true}
	f.manager.persistence.store = fault
	if err := f.manager.Stop(t.Context(), "dev"); err == nil {
		t.Fatal("uncertain write acknowledged")
	}
	if f.record(t).Phase != storage.ServiceCleanup {
		t.Fatal("accepted cleanup intent lost")
	}
	if _, stops := f.sessions.counts(); stops != 0 {
		t.Fatal("unconfirmed write signalled a worker")
	}
	if err := f.manager.Start(t.Context(), "dev"); err == nil {
		t.Fatal("unknown write permitted replacement")
	}
	fault.mu.Lock()
	fault.committedError, fault.failRead = false, false
	fault.mu.Unlock()
	f.manager.supervise()
	settleDemand(t, f.manager)
	if _, stops := f.sessions.counts(); stops != 1 {
		t.Fatalf("confirmed stop count = %d", stops)
	}
	if f.record(t).Attempt != nil {
		t.Fatal("stop result not durable")
	}
}

func TestRuntimeCallerCancellationDoesNotAbandonAcceptedWrite(t *testing.T) {
	f := newRuntimeFixture(t)
	f.sync(t)
	gate := newDemandBarrier(t)
	fault := &runtimeFaultStore{serviceRuntimeStore: f.store, gate: gate}
	f.manager.persistence.store = fault
	ctx, cancel := context.WithCancel(t.Context())
	answer := make(chan error, 1)
	go func() { answer <- f.manager.Stop(ctx, "dev") }()
	awaitDemandSignal(t, gate.entered, "accepted stop write")
	cancel()
	select {
	case err := <-answer:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled waiter = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled caller waited for SQLite")
	}
	gate.unblock()
	settleDemand(t, f.manager)
	if _, stops := f.sessions.counts(); stops != 1 {
		t.Fatalf("accepted stop count = %d", stops)
	}
	if f.record(t).Attempt != nil {
		t.Fatal("accepted stop lost after caller cancellation")
	}
}

func TestRuntimeShutdownJoinsAcceptedWriteAndKillsNothing(t *testing.T) {
	f := newRuntimeFixture(t)
	f.sync(t)
	gate := newDemandBarrier(t)
	fault := &runtimeFaultStore{serviceRuntimeStore: f.store, gate: gate}
	f.manager.persistence.store = fault
	answer := make(chan error, 1)
	go func() { answer <- f.manager.Stop(t.Context(), "dev") }()
	awaitDemandSignal(t, gate.entered, "accepted stop write")
	closed := make(chan struct{})
	go func() { f.manager.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("shutdown did not join its runtime writer")
	case <-time.After(20 * time.Millisecond):
	}
	gate.unblock()
	awaitDemandSignal(t, closed, "joined runtime writer")
	<-answer
	if _, stops := f.sessions.counts(); stops != 0 {
		t.Fatal("shutdown admitted a kill")
	}
	if f.record(t).Phase != storage.ServiceCleanup {
		t.Fatal("shutdown lost owed cleanup")
	}
}

func TestRuntimeRemovedCleanupSurvivesReopen(t *testing.T) {
	f := newRuntimeFixture(t)
	f.sync(t)
	f.sessions.failStops(errRuntimeWrite)
	f.manager.Sync(nil)
	settleDemand(t, f.manager)
	f.manager.Close()
	f.manager = f.open(t, f.store)
	f.manager.Sync(nil)
	settleDemand(t, f.manager)
	if len(f.manager.Retiring()) != 1 || f.record(t).Phase != storage.ServiceCleanup {
		t.Fatal("orphan cleanup was forgotten")
	}
	f.sessions.failStops(nil)
	f.manager.supervise()
	settleDemand(t, f.manager)
	f.manager.supervise()
	settleDemand(t, f.manager)
	if rows, err := f.store.ListServiceRuntime(t.Context()); err != nil || len(rows) != 0 {
		t.Fatalf("retired runtime = %+v %v", rows, err)
	}
}

func TestRuntimeIdleSupervisorDoesNotWrite(t *testing.T) {
	f := newRuntimeFixture(t)
	f.sync(t)
	fault := &runtimeFaultStore{serviceRuntimeStore: f.store}
	f.manager.persistence.store = fault
	for range 5 {
		f.sync(t)
		f.manager.supervise()
		settleDemand(t, f.manager)
	}
	fault.mu.Lock()
	calls := fault.calls
	fault.mu.Unlock()
	if calls != 0 {
		t.Fatalf("idle runtime wrote %d times", calls)
	}
}

func TestRuntimeOldRouteCannotControlReaddedName(t *testing.T) {
	f := newRuntimeFixture(t)
	f.sync(t)
	old := f.manager.route("dev")
	f.manager.Sync(nil)
	settleDemand(t, f.manager)
	f.manager.supervise()
	settleDemand(t, f.manager)
	f.sync(t)
	if err := old.start(t.Context()); err == nil {
		t.Fatal("stale route accepted start")
	}
	if err := old.stop(t.Context()); err == nil {
		t.Fatal("stale route accepted stop")
	}
	if starts, _ := f.sessions.counts(); starts != 2 {
		t.Fatalf("stale route created %d launches", starts)
	}
}

func TestRuntimeBlockedDeleteFencesSameNameReadd(t *testing.T) {
	f := newRuntimeFixture(t)
	f.sync(t)
	if err := f.manager.Stop(t.Context(), "dev"); err != nil {
		t.Fatal(err)
	}
	gate := newDemandBarrier(t)
	fault := &runtimeDeleteStore{serviceRuntimeStore: f.store, gate: gate}
	f.manager.persistence.store = fault
	f.manager.Sync(nil)
	awaitDemandSignal(t, gate.entered, "runtime deletion")
	readded := make(chan struct{})
	go func() { f.manager.Sync([]meshserve.Service{f.service}); close(readded) }()
	select {
	case <-readded:
		t.Fatal("readd passed an unresolved deletion")
	case <-time.After(20 * time.Millisecond):
	}
	gate.unblock()
	awaitDemandSignal(t, readded, "serialized readd")
	settleDemand(t, f.manager)
	record := f.record(t)
	if record.Attempt == nil || record.Attempt.Reference.ID != "S002" {
		t.Fatalf("readded generation = %+v", record)
	}
	f.reopen(t)
	if starts, _ := f.sessions.counts(); starts != 2 {
		t.Fatalf("reopen duplicated readded worker: %d", starts)
	}
}

type runtimeDeleteStore struct {
	serviceRuntimeStore
	gate *demandBarrier
}

func (s *runtimeDeleteStore) CompareServiceRuntime(ctx context.Context, before, next *storage.ServiceRuntime) error {
	if next == nil {
		s.gate.wait()
	}
	if err := s.serviceRuntimeStore.CompareServiceRuntime(ctx, before, next); err != nil {
		return fmt.Errorf("fixture deletion: %w", err)
	}
	return nil
}

func TestRuntimeReaddFinishesAnAdmittedStopBeforeReplacement(t *testing.T) {
	f := newRuntimeFixture(t)
	f.sync(t)
	gate := newDemandBarrier(t)
	f.sessions.mu.Lock()
	f.sessions.stopGate = gate
	f.sessions.mu.Unlock()
	f.manager.Sync(nil)
	awaitDemandSignal(t, gate.entered, "admitted removal stop")
	f.manager.Sync([]meshserve.Service{f.service})
	if starts, _ := f.sessions.counts(); starts != 1 {
		t.Fatalf("readd launched before owed stop: %d", starts)
	}
	gate.unblock()
	settleDemand(t, f.manager)
	if starts, stops := f.sessions.counts(); starts != 2 || stops != 1 {
		t.Fatalf("settled readd starts %d stops %d", starts, stops)
	}
	if f.record(t).Attempt.Reference.ID != "S002" {
		t.Fatal("late stop result replaced successor identity")
	}
}

func TestRuntimeFailedSuccessorAdmissionDoesNotRecountOldTerminal(t *testing.T) {
	f := newRuntimeFixture(t)
	f.sync(t)
	f.manager.Close()
	f.sessions.crashAt("S001", f.clock.read())
	f.sessions.reserveErr = errRuntimeWrite
	f.clock.advance(time.Second)
	f.reopen(t)
	first := f.record(t).Failures
	if first != 2 {
		t.Fatalf("terminal plus admission failures = %d", first)
	}
	f.reopen(t)
	if got := f.record(t).Failures; got != first {
		t.Fatalf("reopen recounted terminal: %d to %d", first, got)
	}
}

func TestRuntimePersistsFullRecipeBeforeLauncher(t *testing.T) {
	f := newRuntimeFixture(t)
	f.service.Demand.Env = []string{"MODE=recipe"}
	observed := make(chan struct{}, 1)
	f.sessions.beforeStart = func(ref session.Reservation, service meshserve.Service) {
		record, err := f.store.GetServiceRuntime(t.Context(), service.Name)
		if err != nil {
			t.Error(err)
			return
		}
		if record.Phase != storage.ServiceStarting || record.Attempt == nil || record.Attempt.Reference != ref || !record.Attempt.Launched.Equal(service) {
			t.Errorf("launcher preceded exact recipe: %+v", record)
			return
		}
		if record.Attempt.Launched.Demand.Env[0] != "MODE=recipe" {
			t.Error("launcher recipe environment missing")
			return
		}
		observed <- struct{}{}
	}
	f.sync(t)
	awaitDemandSignal(t, observed, "durable exact recipe before launcher")
}

func TestRuntimeStopDuringRestoreSurvivesCancelledWaiter(t *testing.T) {
	f := newRuntimeFixture(t)
	f.sync(t)
	f.manager.Close()
	gate := newDemandBarrier(t)
	f.sessions.refsMu.Lock()
	f.sessions.observationGate = gate
	f.sessions.refsMu.Unlock()
	f.manager = f.open(t, f.store)
	f.manager.Sync([]meshserve.Service{f.service})
	awaitDemandSignal(t, gate.entered, "restored exact worker observation")
	ctx, cancel := context.WithCancel(t.Context())
	answer := make(chan error, 1)
	go func() { answer <- f.manager.Stop(ctx, "dev") }()
	awaitRoute(t, f.manager.route("dev"), "accepted stop during restore", stopAccepted)
	cancel()
	if err := <-answer; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled restore stop = %v", err)
	}
	gate.unblock()
	settleDemand(t, f.manager)
	if starts, stops := f.sessions.counts(); starts != 1 || stops != 1 {
		t.Fatalf("restored stop starts %d stops %d", starts, stops)
	}
	if record := f.record(t); record.Attempt != nil {
		t.Fatalf("restored cleanup remains: %+v", record)
	}
}

func TestRuntimeHealthObservationDoesNotOwnMutationAcceptance(t *testing.T) {
	f := newRuntimeFixture(t)
	registry, err := meshserve.NewRegistryWithReservedPrefix(nil, "/mesh")
	if err != nil {
		t.Fatal(err)
	}
	controller, err := newServiceController(t.Context(), t.TempDir(), f.store, registry)
	if err != nil {
		t.Fatal(err)
	}
	controller.demand = f.manager
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	err = controller.observeHealth(ctx)
	// A failed control must still leave the fixture's own acceptance joinable.
	f.manager.persistence.mu.Lock()
	leaked := f.manager.persistence.pending
	f.manager.persistence.mu.Unlock()
	if leaked != nil && leaked.definition && !leaked.finished {
		f.manager.endAcceptance(leaked, false)
	}
	if err != nil {
		t.Fatalf("health read retained mutation acceptance: %v", err)
	}
	if leaked != nil {
		t.Fatal("health read leaked a runtime mutation intent")
	}
	if err := controller.observeHealth(ctx); err != nil {
		t.Fatal(err)
	}
	f.sync(t)
}

func TestRuntimeCommittedErrorNeedsFreshDurableConfirmation(t *testing.T) {
	f := newRuntimeFixture(t)
	f.sync(t)
	fault := &runtimeFaultStore{serviceRuntimeStore: f.store, committedError: true, clearCommitted: true}
	f.manager.persistence.store = fault
	if err := f.manager.Stop(t.Context(), "dev"); err != nil {
		t.Fatal(err)
	}
	fault.mu.Lock()
	calls := fault.calls
	fault.mu.Unlock()
	if calls < 3 {
		t.Fatalf("only %d write attempts; readback replaced the confirmation barrier", calls)
	}
	if record := f.record(t); record.Attempt != nil {
		t.Fatalf("confirmed stop is not durable: %+v", record)
	}
}

type runtimeDeleteFaultStore struct{ *runtimeFaultStore }

func (s runtimeDeleteFaultStore) CompareServiceRuntime(ctx context.Context, before, next *storage.ServiceRuntime) error {
	if next == nil {
		return s.runtimeFaultStore.CompareServiceRuntime(ctx, before, next)
	}
	if err := s.serviceRuntimeStore.CompareServiceRuntime(ctx, before, next); err != nil {
		return fmt.Errorf("fixture non-delete write: %w", err)
	}
	return nil
}

func TestRuntimeUnknownDeleteFencesReaddForAbsentAndOldRows(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprint(committed), func(t *testing.T) { testRuntimeUnknownDelete(t, committed) })
	}
}
func testRuntimeUnknownDelete(t *testing.T, committed bool) {
	f := newRuntimeFixture(t)
	f.sync(t)
	generation := f.record(t).Generation
	if err := f.manager.Stop(t.Context(), "dev"); err != nil {
		t.Fatal(err)
	}
	fault := &runtimeFaultStore{serviceRuntimeStore: f.store, failWrite: !committed, committedError: committed, failRead: committed}
	f.manager.persistence.store = runtimeDeleteFaultStore{fault}
	f.manager.Sync(nil)
	settleDemand(t, f.manager)
	if release, err := f.manager.BeginDefinition(t.Context()); err == nil {
		release()
		t.Fatal("uncertain deletion accepted a same-name definition")
	}
	rows, err := f.store.ListServiceRuntime(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if committed && len(rows) != 0 || !committed && len(rows) != 1 {
		t.Fatalf("delete outcome committed %t rows %+v", committed, rows)
	}
	fault.mu.Lock()
	fault.failWrite, fault.committedError, fault.failRead = false, false, false
	fault.mu.Unlock()
	f.manager.supervise()
	settleDemand(t, f.manager)
	f.sync(t)
	if record := f.record(t); record.Generation == generation || record.Attempt == nil || record.Attempt.Reference.ID != "S002" {
		t.Fatalf("readded generation = %+v", record)
	}
}

func TestRuntimeRestoreRequiresDurableBarrierAfterRead(t *testing.T) {
	f := newRuntimeFixture(t)
	f.sync(t)
	fault := &runtimeFaultStore{serviceRuntimeStore: f.store, failWrite: true}
	manager := newDemandManager(t.Context(), f.sessions, func(error) {})
	if err := manager.restoreRuntime(fault); err == nil {
		t.Fatal("visible restored row skipped durable confirmation")
	}
	if manager.persistence != nil {
		t.Fatal("unconfirmed restore became authoritative")
	}
	manager.Close()
}
