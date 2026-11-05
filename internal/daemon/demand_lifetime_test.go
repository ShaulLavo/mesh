package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	meshserve "github.com/shaul/mesh/internal/serve"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/worker"
)

// demandEvents records the order of fixture events across goroutines.
type demandEvents struct {
	mu     sync.Mutex
	events []string
}

func (e *demandEvents) add(event string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, event)
}

func (e *demandEvents) list() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.events...)
}

// awaitRoute waits until condition holds for the route under its lock.
func awaitRoute(t *testing.T, route *demandRoute, what string, condition func(*demandRoute) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		route.mu.Lock()
		reached := condition(route)
		route.mu.Unlock()
		if reached {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within five seconds", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func stopAccepted(route *demandRoute) bool  { return route.lastStop > route.lastStart }
func startAccepted(route *demandRoute) bool { return route.lastStart > route.lastStop }

func demandManagerWithParent(parent context.Context, t *testing.T, sessions demandSessions) *demandManager {
	t.Helper()
	ctx, cancel := context.WithCancel(parent)
	manager := newDemandManager(ctx, sessions, func(error) {})
	manager.poll = 5 * time.Millisecond
	manager.cleanupRetry = 0
	manager.bind = func(uint16) (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") }
	manager.dial = func(context.Context, string) error { return nil }
	t.Cleanup(func() {
		manager.Close()
		cancel()
		joinAdoptionTransitions(t, manager)
		manager.goroutines.Wait()
	})
	return manager
}

func awaitDemandSignal(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not happen within five seconds", what)
	}
}

// An idle stop is the leading explanation of a lost overnight service, and it
// left no trace. The stopped session and why must be in the daemon log, even
// after the next start has replaced it.
func TestDemandIdleStopLogsStoppedSessionAndReason(t *testing.T) {
	sessions := newFakeDemandSessions()
	manager := testDemandManager(t, sessions, func() bool { return true })
	sink := &demandLogSink{}
	manager.logger = log.New(sink, "", 0)
	manager.Sync([]meshserve.Service{demandService(time.Hour)})
	if err := manager.Start(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	route := manager.route("dev")
	route.mu.Lock()
	token := route.idleToken
	route.mu.Unlock()
	route.idleExpired(token)
	waitForSettledRoute(t, manager)
	if err := manager.Start(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	if status := manager.Status("dev"); status.SessionID != "S002" {
		t.Fatalf("status = %+v, want the replacement S002", status)
	}
	want := "daemon: route /dev: stopped session S001 (idle for 1h0m0s with no Mesh connections)\n"
	if !strings.Contains(sink.String(), want) {
		t.Fatalf("daemon log = %q, want %q", sink.String(), want)
	}
}

// A Stop whose caller gives up is still a Stop. The start it interrupted
// must end with its worker stopped, not running unowned by any intent.
func TestDemandStopDuringPendingStartSurvivesCancelledWaiter(t *testing.T) {
	sessions := newFakeDemandSessions()
	manager := testDemandManager(t, sessions, func() bool { return true })
	ready := newDemandBarrier(t)
	manager.dial = func(context.Context, string) error { ready.wait(); return nil }
	manager.Sync([]meshserve.Service{demandService(time.Hour)})
	start := beginAdoptionCall(t, func(ctx context.Context) error { return manager.Start(ctx, "dev") })
	awaitDemandSignal(t, ready.entered, "readiness check")

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	stop := beginAdoptionCall(t, func(context.Context) error { return manager.Stop(ctx, "dev") })
	awaitRoute(t, manager.route("dev"), "Stop accepted", stopAccepted)
	cancel()
	if err := stop.wait(t); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stop = %v, want its caller's cancellation", err)
	}
	ready.unblock()
	_ = start.wait(t)
	waitForSettledRoute(t, manager)
	manager.supervise()
	waitForSettledRoute(t, manager)
	if status := manager.Status("dev"); status.State != protocol.DemandStopped {
		t.Fatalf("status = %+v, want the accepted Stop carried out", status)
	}
	if started, stopped := sessions.counts(); started != 1 || stopped != 1 || sessions.liveCount() != 0 {
		t.Fatalf("started %d, stopped %d, live %d; want one start stopped once", started, stopped, sessions.liveCount())
	}
}

// Close waits for a launch it already admitted, so the daemon never exits
// while a worker it is creating has no owner yet.
func TestDemandCloseJoinsAnAdmittedLaunch(t *testing.T) {
	sessions := newFakeDemandSessions()
	manager := testDemandManager(t, sessions, func() bool { return true })
	launch := newDemandBarrier(t)
	sessions.launchGate = launch
	manager.Sync([]meshserve.Service{demandService(time.Hour)})
	start := beginAdoptionCall(t, func(ctx context.Context) error { return manager.Start(ctx, "dev") })
	awaitDemandSignal(t, launch.entered, "admitted launch")

	events := &demandEvents{}
	closed := make(chan struct{})
	go func() {
		manager.Close()
		events.add("close returned")
		close(closed)
	}()
	t.Cleanup(func() { launch.unblock(); <-closed })
	select {
	case <-closed:
	case <-time.After(20 * time.Millisecond):
	}
	events.add("launch released")
	launch.unblock()
	awaitDemandSignal(t, closed, "Close")
	_ = start.wait(t)
	if got := events.list(); len(got) != 2 || got[0] != "launch released" {
		t.Fatalf("events = %v, want Close to wait for the admitted launch", got)
	}
	if started, stopped := sessions.counts(); started != 1 || stopped != 0 || sessions.liveCount() != 1 {
		t.Fatalf("started %d, stopped %d, live %d; want the launched worker kept for the next daemon", started, stopped, sessions.liveCount())
	}
}

// Daemon shutdown cancels the manager context before Close runs. Nothing may
// launch in between.
func TestDemandCancelledDaemonContextAdmitsNoLaunch(t *testing.T) {
	sessions := newFakeDemandSessions()
	daemon, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	manager := demandManagerWithParent(daemon, t, sessions)
	manager.Sync([]meshserve.Service{demandService(time.Hour)})
	cancel()
	result := beginAdoptionCall(t, enterAdoption(manager))
	if err := result.wait(t); err == nil {
		t.Fatal("Enter succeeded after the daemon context ended")
	}
	if started, _ := sessions.counts(); started != 0 {
		t.Fatalf("started %d sessions after the daemon context ended, want 0", started)
	}
}

// A start whose readiness fails after Close keeps its worker: shutdown never
// begins a kill, and the next daemon adopts or cleans it up.
func TestDemandReadinessFailureAfterCloseDoesNotKill(t *testing.T) {
	sessions := newFakeDemandSessions()
	manager := testDemandManager(t, sessions, func() bool { return false })
	ready := newDemandBarrier(t)
	manager.dial = func(context.Context, string) error { ready.wait(); return syscall.ECONNREFUSED }
	service := demandService(time.Hour)
	service.Demand.ReadyTimeout = 30 * time.Millisecond
	manager.Sync([]meshserve.Service{service})
	start := beginAdoptionCall(t, func(ctx context.Context) error { return manager.Start(ctx, "dev") })
	awaitDemandSignal(t, ready.entered, "readiness check")
	manager.Close()
	ready.unblock()
	if err := start.wait(t); err == nil {
		t.Fatal("a start whose readiness failed succeeded")
	}
	waitForSettledRoute(t, manager)
	if started, stopped := sessions.counts(); started != 1 || stopped != 0 || sessions.liveCount() != 1 {
		t.Fatalf("started %d, stopped %d, live %d; want no kill after Close", started, stopped, sessions.liveCount())
	}
}

// A route redefined while it started would normally stop the old recipe's
// worker. After Close that stop belongs to the next daemon.
func TestDemandRedefinedStartAfterCloseDoesNotKill(t *testing.T) {
	sessions := newFakeDemandSessions()
	manager := testDemandManager(t, sessions, func() bool { return true })
	ready := newDemandBarrier(t)
	manager.dial = func(context.Context, string) error { ready.wait(); return nil }
	manager.Sync([]meshserve.Service{demandService(time.Hour)})
	start := beginAdoptionCall(t, func(ctx context.Context) error { return manager.Start(ctx, "dev") })
	awaitDemandSignal(t, ready.entered, "readiness check")
	changed := demandService(time.Hour)
	changed.Demand.Command = "bun run dev --port 1"
	manager.Sync([]meshserve.Service{changed})
	manager.Close()
	ready.unblock()
	_ = start.wait(t)
	waitForSettledRoute(t, manager)
	if started, stopped := sessions.counts(); started != 1 || stopped != 0 || sessions.liveCount() != 1 {
		t.Fatalf("started %d, stopped %d, live %d; want no kill after Close", started, stopped, sessions.liveCount())
	}
}

// historyLifecycle is the real session lifecycle over a real catalog, with a
// launcher that writes a live worker's metadata. It reports when a route's
// start has finished with its earlier history.
type historyLifecycle struct {
	*lifecycle
	historyDone chan struct{}
}

func (h *historyLifecycle) pruneLabelled(ctx context.Context, label, current string) []string {
	removed := h.lifecycle.pruneLabelled(ctx, label, current)
	close(h.historyDone)
	return removed
}

func newHistoryLifecycle(t *testing.T) *historyLifecycle {
	t.Helper()
	ctx := context.Background()
	sessionsDir := compactSocketTempDir(t)
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "mesh.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	catalog, err := NewCatalog(CatalogConfig{
		SessionsDir: sessionsDir,
		Host:        catalogTestHost(time.Now()),
		Store:       store,
		Probe:       probeFunc(func(context.Context, string) error { return nil }),
		BootID:      func() string { return "boot-a" },
	})
	if err != nil {
		t.Fatal(err)
	}
	next := 0
	launched := mustLifecycle(t, lifecycleConfig{
		Catalog:     catalog,
		Connector:   failingLifecycleConnector(),
		Host:        catalogTestHost(time.Now()),
		SessionsDir: sessionsDir,
		Launch: func(cfg worker.LaunchConfig) (worker.Launched, error) {
			next++
			id := []string{"N1VE", "N2VE", "N3VE"}[next-1]
			dir := filepath.Join(sessionsDir, id)
			meta := worker.Meta{
				ID: id, PID: os.Getpid(), Command: cfg.Command, Cwd: cfg.Cwd, Label: cfg.Label,
				State: worker.StateRunning, CreatedAt: time.Now(), BootID: "boot-a",
			}
			if err := os.Mkdir(dir, 0o700); err != nil {
				return worker.Launched{}, fmt.Errorf("fake worker %s: %w", id, err)
			}
			if err := worker.WriteMeta(dir, meta); err != nil {
				return worker.Launched{}, fmt.Errorf("fake worker %s: %w", id, err)
			}
			return worker.Launched{Meta: meta, Dir: dir}, nil
		},
	})
	return &historyLifecycle{lifecycle: launched, historyDone: make(chan struct{})}
}

func writeEndedLabelledSession(t *testing.T, sessionsDir, id, label string, createdAt time.Time, ran time.Duration) {
	t.Helper()
	exitedAt, code := createdAt.Add(ran), 1
	meta := worker.Meta{
		ID: id, PID: 4242, Command: []string{"sh", "-lc", "serve"}, Cwd: "/tmp", Label: label,
		State: worker.StateExited, CreatedAt: createdAt, ExitedAt: &exitedAt, ExitCode: &code, BootID: "boot-a",
	}
	writeCatalogMeta(t, sessionsDir, id, meta)
}

// Starting a route again must not erase the sessions that explain why it
// needed starting: an earlier exit is the evidence of the outage.
func TestDemandRouteStartKeepsEarlierLabelledHistory(t *testing.T) {
	sessions := newHistoryLifecycle(t)
	now := time.Now()
	for index, id := range []string{"0DD1", "0DD2", "0DD3"} {
		writeEndedLabelledSession(t, sessions.sessionsDir, id, "serve /dev", now.Add(-time.Duration(3-index)*time.Hour), 2*time.Minute)
	}
	manager := demandManagerWithParent(t.Context(), t, sessions)
	manager.Sync([]meshserve.Service{demandService(time.Hour)})
	if err := manager.Start(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	awaitDemandSignal(t, sessions.historyDone, "history cleanup after the start")
	for _, id := range []string{"0DD1", "0DD2", "0DD3"} {
		if _, err := os.Stat(filepath.Join(sessions.sessionsDir, id)); err != nil {
			t.Fatalf("earlier ended session %s was erased by the next start: %v", id, err)
		}
	}
}
