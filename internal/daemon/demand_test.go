package daemon

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	meshserve "github.com/shaul/mesh/internal/serve"
)

// fakeDemandSessions is a lifecycle whose sessions are flags: a started
// session is live until stopped or told to exit.
type fakeDemandSessions struct {
	mu      sync.Mutex
	next    int
	live    map[string]string // id → label
	exits   map[string]int
	started []string
	stopped []string
	exitNow *int  // a started session exits at once with this code
	stopErr error // a stop fails and leaves the session running
	// publishErr makes a start launch its session and then fail, the way a
	// catalog that cannot record it does.
	publishErr error
	// startErr makes a start launch its session and then fail before it is
	// known to be ready, the way a worker that never answers does.
	startErr error
	adoptID  string
	adoptFor string
}

func newFakeDemandSessions() *fakeDemandSessions {
	return &fakeDemandSessions{live: map[string]string{}, exits: map[string]int{}}
}

func (f *fakeDemandSessions) startLabelled(_ context.Context, label string, command []string, cwd string, env []string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	id := fmt.Sprintf("S%03d", f.next)
	f.started = append(f.started, id)
	if f.exitNow != nil {
		f.exits[id] = *f.exitNow
		return id, nil
	}
	f.live[id] = label
	if f.startErr != nil {
		return id, fmt.Errorf("daemon: create: launch worker %s: %w", id, f.startErr)
	}
	if f.publishErr != nil {
		return id, publicationError{fmt.Errorf("daemon: publish session %s: %w", id, f.publishErr)}
	}
	return id, nil
}

func (f *fakeDemandSessions) stopSession(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, id)
	if f.stopErr != nil {
		return f.stopErr
	}
	if _, live := f.live[id]; live {
		delete(f.live, id)
		f.exits[id] = 129
	}
	return nil
}

func (f *fakeDemandSessions) sessionExit(id string) (*int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if code, ended := f.exits[id]; ended {
		return &code, true
	}
	return nil, false
}

func (f *fakeDemandSessions) outputTail(context.Context, string) string { return "npm ERR! boom" }

func (f *fakeDemandSessions) findLabelled(_ context.Context, label string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.adoptFor == label {
		f.live[f.adoptID] = label
		return f.adoptID, true, nil
	}
	return "", false, nil
}

func (f *fakeDemandSessions) forgetLabelled(context.Context, string, string) {}

func (f *fakeDemandSessions) crash(id string, code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.live, id)
	f.exits[id] = code
}

func (f *fakeDemandSessions) failStops(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopErr = err
}

func (f *fakeDemandSessions) liveCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.live)
}

func (f *fakeDemandSessions) counts() (started, stopped int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.started), len(f.stopped)
}

func testDemandManager(t *testing.T, sessions *fakeDemandSessions, upstreamReady func() bool) *demandManager {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	manager := newDemandManager(ctx, sessions, func(error) {})
	manager.poll = 5 * time.Millisecond
	manager.cleanupRetry = 0
	manager.bind = func(uint16) (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") }
	manager.dial = func(context.Context, string) error {
		if upstreamReady() {
			return nil
		}
		return syscall.ECONNREFUSED
	}
	t.Cleanup(func() {
		manager.Close()
		cancel()
	})
	return manager
}

func demandService(idle time.Duration) meshserve.Service {
	return meshserve.Service{
		Name: "dev", Kind: meshserve.Proxy, Target: "5173",
		Listens: []meshserve.Listen{{Public: 5173, Upstream: 15173}},
		Demand:  &meshserve.Demand{Command: "bun run dev", Cwd: "/tmp", Idle: idle, ReadyTimeout: time.Second},
	}
}

func waitForDemand(t *testing.T, manager *demandManager, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if manager.Status("dev").State == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("route state = %q, want %q", manager.Status("dev").State, want)
}

func TestDemandRouteStartsOnEnterAndStopsWhenIdle(t *testing.T) {
	sessions := newFakeDemandSessions()
	var ready sync.Mutex
	upstream := false
	manager := testDemandManager(t, sessions, func() bool { ready.Lock(); defer ready.Unlock(); return upstream })
	manager.Sync([]meshserve.Service{demandService(50 * time.Millisecond)})
	if state := manager.Status("dev").State; state != protocol.DemandStopped {
		t.Fatalf("new route state = %q, want stopped", state)
	}

	entered := make(chan error, 2)
	releases := make(chan func(), 2)
	for range 2 {
		go func() {
			release, err := manager.Enter(context.Background(), "dev")
			if err == nil {
				releases <- release
			}
			entered <- err
		}()
	}
	waitForDemand(t, manager, protocol.DemandStarting)
	select {
	case err := <-entered:
		t.Fatalf("Enter returned %v before the upstream accepted", err)
	case <-time.After(30 * time.Millisecond):
	}
	ready.Lock()
	upstream = true
	ready.Unlock()
	for range 2 {
		if err := <-entered; err != nil {
			t.Fatal(err)
		}
	}
	if started, _ := sessions.counts(); started != 1 {
		t.Fatalf("two waiting connections started %d sessions, want 1", started)
	}
	if status := manager.Status("dev"); status.State != protocol.DemandRunning || status.Connections != 2 {
		t.Fatalf("status = %+v, want running with 2 connections", status)
	}

	(<-releases)()
	time.Sleep(120 * time.Millisecond)
	if _, stopped := sessions.counts(); stopped != 0 {
		t.Fatal("route stopped while a connection was still open")
	}
	(<-releases)()
	waitForDemand(t, manager, protocol.DemandStopped)
	if _, stopped := sessions.counts(); stopped != 1 {
		t.Fatalf("idle route stopped %d sessions, want 1", stopped)
	}
}

func TestDemandRouteReportsACommandThatExits(t *testing.T) {
	sessions := newFakeDemandSessions()
	code := 3
	sessions.exitNow = &code
	manager := testDemandManager(t, sessions, func() bool { return false })
	manager.Sync([]meshserve.Service{demandService(time.Minute)})

	_, err := manager.Enter(context.Background(), "dev")
	if err == nil {
		t.Fatal("Enter succeeded for a command that exited")
	}
	for _, want := range []string{"route /dev", "status 3", "npm ERR! boom", "bun run dev"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("failure %q does not mention %q", err, want)
		}
	}
	status := manager.Status("dev")
	if status.State != protocol.DemandFailed || !strings.Contains(status.Failure, "status 3") || strings.Contains(status.Failure, "npm ERR") {
		t.Fatalf("status = %+v, want failed with a one-line summary", status)
	}

	// The next connection retries rather than replaying the old failure.
	sessions.exitNow = nil
	release, err := manager.Enter(context.Background(), "dev")
	if err == nil {
		release()
		t.Fatal("retry succeeded although the upstream never accepts")
	}
	if started, _ := sessions.counts(); started != 2 {
		t.Fatalf("retry started %d sessions in total, want 2", started)
	}
}

func TestDemandRouteReadyTimeoutStopsTheSession(t *testing.T) {
	sessions := newFakeDemandSessions()
	manager := testDemandManager(t, sessions, func() bool { return false })
	service := demandService(time.Minute)
	service.Demand.ReadyTimeout = 30 * time.Millisecond
	manager.Sync([]meshserve.Service{service})

	if err := manager.Start(context.Background(), "dev"); err == nil || !strings.Contains(err.Error(), "did not accept within") {
		t.Fatalf("Start = %v, want a ready timeout", err)
	}
	if _, stopped := sessions.counts(); stopped != 1 {
		t.Fatal("a session that never became ready was left running")
	}
}

func TestDemandRouteAdoptsARunningSessionWithAFullIdleWindow(t *testing.T) {
	sessions := newFakeDemandSessions()
	sessions.adoptID, sessions.adoptFor = "OLD1", "serve /dev"
	manager := testDemandManager(t, sessions, func() bool { return true })
	manager.Sync([]meshserve.Service{demandService(60 * time.Millisecond)})
	if status := manager.Status("dev"); status.State != protocol.DemandRunning || status.SessionID != "OLD1" {
		t.Fatalf("status = %+v, want the adopted session running", status)
	}
	release, err := manager.Enter(context.Background(), "dev")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if started, _ := sessions.counts(); started != 0 {
		t.Fatal("an adopted route started a second session")
	}
	waitForDemand(t, manager, protocol.DemandStopped)
}

func TestDemandRouteNoticesACrashAndRestartsOnTheNextConnection(t *testing.T) {
	sessions := newFakeDemandSessions()
	manager := testDemandManager(t, sessions, func() bool { return true })
	manager.Sync([]meshserve.Service{demandService(time.Minute)})
	if err := manager.Start(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	sessions.crash(manager.Status("dev").SessionID, 1)
	manager.supervise()
	if status := manager.Status("dev"); status.State != protocol.DemandFailed || !strings.Contains(status.Failure, "while serving") {
		t.Fatalf("status after crash = %+v", status)
	}
	release, err := manager.Enter(context.Background(), "dev")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if started, _ := sessions.counts(); started != 2 {
		t.Fatalf("started %d sessions, want a restart", started)
	}
}

func TestDemandRouteStopsWhenItsRecipeChangesOrItIsRemoved(t *testing.T) {
	sessions := newFakeDemandSessions()
	manager := testDemandManager(t, sessions, func() bool { return true })
	service := demandService(time.Minute)
	manager.Sync([]meshserve.Service{service})
	if err := manager.Start(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	changed := demandService(time.Minute)
	changed.Demand.Command = "bun run dev --port 1"
	manager.Sync([]meshserve.Service{changed})
	waitForDemand(t, manager, protocol.DemandStopped)

	if err := manager.Start(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	manager.Sync(nil)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, stopped := sessions.counts(); stopped == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("removing the route left its session running")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if manager.Status("dev") != nil {
		t.Fatal("a removed route still reports a status")
	}
}

func TestDemandReserveNamesTheProcessHoldingAPort(t *testing.T) {
	manager := testDemandManager(t, newFakeDemandSessions(), func() bool { return true })
	manager.bind = func(uint16) (net.Listener, error) {
		return nil, &net.OpError{Op: "listen", Net: "tcp", Err: &osSyscallError{err: syscall.EADDRINUSE}}
	}
	manager.holder = func(port uint16) string { return fmt.Sprintf("pid 42 (vite --port %d)", port) }
	err := manager.Reserve(demandService(time.Minute))
	if err == nil || err.Error() != "port 5173 is held by pid 42 (vite --port 5173)" {
		t.Fatalf("Reserve = %v", err)
	}
}

func TestDemandReserveRefusesAnotherRoutesListener(t *testing.T) {
	manager := testDemandManager(t, newFakeDemandSessions(), func() bool { return true })
	first := demandService(time.Minute)
	if err := manager.Reserve(first); err != nil {
		t.Fatal(err)
	}
	manager.Sync([]meshserve.Service{first})
	if err := manager.Reserve(first); err != nil {
		t.Fatalf("re-reserving a route's own listener: %v", err)
	}
	second := demandService(time.Minute)
	second.Name = "other"
	if err := manager.Reserve(second); err == nil || !strings.Contains(err.Error(), "listener of route /dev") {
		t.Fatalf("Reserve = %v, want the owning route named", err)
	}
}

// osSyscallError stands in for *os.SyscallError so the test exercises the
// errors.Is unwrapping a real bind failure goes through.
type osSyscallError struct{ err error }

func (e *osSyscallError) Error() string { return e.err.Error() }
func (e *osSyscallError) Unwrap() error { return e.err }

func TestLastLinesKeepsTheReadableEnd(t *testing.T) {
	text := "one\r\n\r\ntwo\rTWO\n" + strings.Repeat("x", 300) + "\nthree  \n"
	got := lastLines(text, 3, 10)
	want := "TWO\nxxxxxxxxxx…\nthree"
	if got != want {
		t.Fatalf("lastLines = %q, want %q", got, want)
	}
}

func listenerAddress(t *testing.T, manager *demandManager, port uint16) string {
	t.Helper()
	manager.mu.Lock()
	defer manager.mu.Unlock()
	listener := manager.listeners[port]
	if listener == nil {
		t.Fatalf("no listener for port %d", port)
	}
	return listener.listener.Addr().String()
}

func TestDemandListenerProxiesAndCountsConnections(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/page" {
			_, _ = io.WriteString(w, "upstream saw /page")
		}
	}))
	defer upstream.Close()
	port := uint16(upstream.Listener.Addr().(*net.TCPAddr).Port) //nolint:gosec // net.TCPAddr ports are bounded to uint16

	sessions := newFakeDemandSessions()
	manager := testDemandManager(t, sessions, func() bool { return true })
	plain := meshserve.Service{Name: "4000", Kind: meshserve.Proxy, Target: "4000", LocalOnly: true,
		Listens: []meshserve.Listen{{Public: 4000, Upstream: port}}}
	onDemand := demandService(time.Minute)
	onDemand.Listens = []meshserve.Listen{{Public: 5173, Upstream: port}}
	manager.Sync([]meshserve.Service{plain, onDemand})

	for _, public := range []uint16{4000, 5173} {
		connection, err := net.Dial("tcp", listenerAddress(t, manager, public))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprint(connection, "GET /page HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		response, err := http.ReadResponse(bufio.NewReader(connection), nil)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if string(body) != "upstream saw /page" {
			t.Fatalf("listener %d answered %q", public, body)
		}
		if public == 5173 {
			if status := manager.Status("dev"); status.Connections != 1 || status.State != protocol.DemandRunning {
				t.Fatalf("an open keep-alive is not counted: %+v", status)
			}
		}
		_ = connection.Close()
	}
	if started, _ := sessions.counts(); started != 1 {
		t.Fatalf("started %d sessions, want 1 for the on-demand listener only", started)
	}
	deadline := time.Now().Add(2 * time.Second)
	for manager.Status("dev").Connections != 0 {
		if time.Now().After(deadline) {
			t.Fatal("a closed connection is still counted")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestDemandRouteStopsWhenItsLabelChanges(t *testing.T) {
	sessions := newFakeDemandSessions()
	manager := testDemandManager(t, sessions, func() bool { return true })
	manager.Sync([]meshserve.Service{demandService(time.Minute)})
	if err := manager.Start(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	local := demandService(time.Minute)
	local.LocalOnly = true
	manager.Sync([]meshserve.Service{local})
	waitForDemand(t, manager, protocol.DemandStopped)
}

func TestDemandRouteKeepsRunningWhenOnlyItsTimingChanges(t *testing.T) {
	sessions := newFakeDemandSessions()
	manager := testDemandManager(t, sessions, func() bool { return true })
	manager.Sync([]meshserve.Service{demandService(time.Minute)})
	if err := manager.Start(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	shorter := demandService(30 * time.Millisecond)
	shorter.Demand.ReadyTimeout = 5 * time.Second
	manager.Sync([]meshserve.Service{shorter})
	if _, stopped := sessions.counts(); stopped != 0 || manager.Status("dev").State != protocol.DemandRunning {
		t.Fatal("changing only the idle window restarted the session")
	}
	// The running idle clock picked up the shorter window.
	waitForDemand(t, manager, protocol.DemandStopped)
}

func TestDemandListenersCloseWhileConnectionsArrive(t *testing.T) {
	manager := testDemandManager(t, newFakeDemandSessions(), func() bool { return true })
	manager.Sync([]meshserve.Service{demandService(time.Minute)})
	address := listenerAddress(t, manager, 5173)
	stop := make(chan struct{})
	var dialers sync.WaitGroup
	for range 8 {
		dialers.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				if connection, err := net.DialTimeout("tcp", address, 50*time.Millisecond); err == nil {
					_ = connection.Close()
				}
			}
		})
	}
	time.Sleep(20 * time.Millisecond)
	done := make(chan struct{})
	go func() {
		manager.Sync(nil)
		manager.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("closing a listener with connections arriving deadlocked")
	}
	close(stop)
	dialers.Wait()
}

var errWorkerSilent = errors.New("worker did not answer")

func TestDemandFailedStopMustNotSpawnDuplicate(t *testing.T) {
	sessions := newFakeDemandSessions()
	manager := testDemandManager(t, sessions, func() bool { return true })
	manager.Sync([]meshserve.Service{demandService(time.Minute)})
	if err := manager.Start(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	sessions.failStops(errWorkerSilent)
	if err := manager.Stop(context.Background(), "dev"); !errors.Is(err, errWorkerSilent) {
		t.Fatalf("Stop = %v, want the stop failure", err)
	}
	if status := manager.Status("dev"); status.State != protocol.DemandFailed || status.SessionID != "S001" {
		t.Fatalf("status after a failed stop = %+v, want failed and still naming S001", status)
	}

	if err := manager.Start(context.Background(), "dev"); err == nil {
		t.Fatal("Start succeeded while the route's first session may still run")
	}
	release, err := manager.Enter(context.Background(), "dev")
	if err == nil {
		release()
		t.Fatal("Enter succeeded while the route's first session may still run")
	}
	if started, _ := sessions.counts(); started != 1 || sessions.liveCount() != 1 {
		t.Fatalf("failed stop retained live original but started %d workers", started)
	}

	sessions.failStops(nil)
	if err := manager.Start(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	if started, _ := sessions.counts(); started != 2 || sessions.liveCount() != 1 {
		t.Fatalf("after the original stopped, started %d with %d live; want one replacement", started, sessions.liveCount())
	}
}

func TestDemandSecondStopRetriesTheOwnedSession(t *testing.T) {
	sessions := newFakeDemandSessions()
	manager := testDemandManager(t, sessions, func() bool { return true })
	manager.Sync([]meshserve.Service{demandService(time.Minute)})
	if err := manager.Start(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	sessions.failStops(errWorkerSilent)
	if err := manager.Stop(context.Background(), "dev"); err == nil {
		t.Fatal("first Stop succeeded although the stop failed")
	}
	if err := manager.Stop(context.Background(), "dev"); !errors.Is(err, errWorkerSilent) {
		t.Fatalf("second Stop = %v, want a retried stop that fails again", err)
	}
	if _, stopped := sessions.counts(); stopped != 2 {
		t.Fatalf("stop attempts = %d, want the second Stop to retry", stopped)
	}
	sessions.failStops(nil)
	if err := manager.Stop(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	if sessions.liveCount() != 0 || manager.Status("dev").State != protocol.DemandStopped {
		t.Fatalf("status = %+v with %d live, want stopped", manager.Status("dev"), sessions.liveCount())
	}
}

func TestDemandReadyTimeoutWithFailedCleanupDoesNotDuplicate(t *testing.T) {
	sessions := newFakeDemandSessions()
	manager := testDemandManager(t, sessions, func() bool { return false })
	service := demandService(time.Minute)
	service.Demand.ReadyTimeout = 30 * time.Millisecond
	manager.Sync([]meshserve.Service{service})
	sessions.failStops(errWorkerSilent)

	if err := manager.Start(context.Background(), "dev"); err == nil || !strings.Contains(err.Error(), "stopping it failed") {
		t.Fatalf("Start = %v, want a ready timeout whose cleanup failed", err)
	}
	if err := manager.Start(context.Background(), "dev"); err == nil {
		t.Fatal("Start succeeded although the upstream never accepts")
	}
	if started, _ := sessions.counts(); started != 1 {
		t.Fatalf("a timed-out session that could not be stopped was followed by %d starts, want 1", started)
	}
}

func TestDemandRecipeChangeAfterAFailedStopKeepsOwnership(t *testing.T) {
	sessions := newFakeDemandSessions()
	manager := testDemandManager(t, sessions, func() bool { return true })
	manager.Sync([]meshserve.Service{demandService(time.Minute)})
	if err := manager.Start(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	sessions.failStops(errWorkerSilent)
	if err := manager.Stop(context.Background(), "dev"); err == nil {
		t.Fatal("Stop succeeded although the stop failed")
	}
	changed := demandService(time.Minute)
	changed.Demand.Command = "bun run dev --port 1"
	manager.Sync([]meshserve.Service{changed})
	if err := manager.Start(context.Background(), "dev"); err == nil {
		t.Fatal("Start succeeded while the old recipe's session may still run")
	}
	if started, _ := sessions.counts(); started != 1 {
		t.Fatalf("a recipe change after a failed stop started %d sessions, want 1", started)
	}
}

func TestDemandRemovalRetriesAFailedStop(t *testing.T) {
	sessions := newFakeDemandSessions()
	manager := testDemandManager(t, sessions, func() bool { return true })
	manager.Sync([]meshserve.Service{demandService(time.Minute)})
	if err := manager.Start(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	sessions.failStops(errWorkerSilent)
	manager.Sync(nil)
	waitForSettledRoute(t, manager)

	sessions.failStops(nil)
	manager.Sync(nil)
	waitForStops(t, sessions, 2)
	deadline := time.Now().Add(2 * time.Second)
	for sessions.liveCount() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("a removed route forgot the session its failed stop left running")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestDemandRouteReaddedAfterAFailedRemovalKeepsOwnership(t *testing.T) {
	sessions := newFakeDemandSessions()
	manager := testDemandManager(t, sessions, func() bool { return true })
	manager.Sync([]meshserve.Service{demandService(time.Minute)})
	if err := manager.Start(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	sessions.failStops(errWorkerSilent)
	manager.Sync(nil)
	waitForSettledRoute(t, manager)

	manager.Sync([]meshserve.Service{demandService(time.Minute)})
	if err := manager.Start(context.Background(), "dev"); err == nil {
		t.Fatal("a re-added route started while its old session may still run")
	}
	if started, _ := sessions.counts(); started != 1 {
		t.Fatalf("re-adding a route whose stop failed started %d sessions, want 1", started)
	}
}

func TestDemandManagerCloseLeavesTheSessionRunning(t *testing.T) {
	sessions := newFakeDemandSessions()
	manager := testDemandManager(t, sessions, func() bool { return true })
	manager.Sync([]meshserve.Service{demandService(time.Minute)})
	if err := manager.Start(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	manager.Close()
	if _, stopped := sessions.counts(); stopped != 0 || sessions.liveCount() != 1 {
		t.Fatal("closing the manager stopped the route's session")
	}
}

// waitForSettledRoute waits until route dev, removed or not, has no start or
// stop in progress, so the next Sync sees how its last transition ended.
func waitForSettledRoute(t *testing.T, manager *demandManager) {
	t.Helper()
	manager.mu.Lock()
	route := manager.routes["dev"]
	if route == nil {
		route = manager.retiring["dev"]
	}
	manager.mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for {
		route.mu.Lock()
		settled := route.pending == nil
		route.mu.Unlock()
		if settled {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("route dev is still starting or stopping")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func waitForStops(t *testing.T, sessions *fakeDemandSessions, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, stopped := sessions.counts(); stopped >= want {
			return
		}
		if time.Now().After(deadline) {
			_, stopped := sessions.counts()
			t.Fatalf("stop attempts = %d, want %d", stopped, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestDemandPublicationFailureDoesNotLaunchAgain(t *testing.T) {
	sessions := newFakeDemandSessions()
	sessions.publishErr = errors.New("catalog is read-only")
	var reported []error
	var reportedMu sync.Mutex
	manager := testDemandManager(t, sessions, func() bool { return true })
	manager.report = func(err error) { reportedMu.Lock(); reported = append(reported, err); reportedMu.Unlock() }
	manager.Sync([]meshserve.Service{demandService(time.Minute)})

	for range 3 {
		_ = manager.Start(context.Background(), "dev")
	}
	if started, _ := sessions.counts(); started != 1 || sessions.liveCount() != 1 {
		t.Fatalf("a start whose publication failed was followed by %d launches with %d live, want 1", started, sessions.liveCount())
	}
	if status := manager.Status("dev"); status.SessionID != "S001" {
		t.Fatalf("status = %+v, want the route to own S001", status)
	}
	if err := manager.Stop(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	if sessions.liveCount() != 0 {
		t.Fatal("stopping the route left the session whose publication failed running")
	}
	reportedMu.Lock()
	defer reportedMu.Unlock()
	if len(reported) == 0 || !strings.Contains(reported[0].Error(), "S001") {
		t.Fatalf("reported %v, want the publication failure naming S001", reported)
	}
}

// upgradingUpstream answers every request by switching protocols and then
// holding the raw connection open until the test ends.
func upgradingUpstream(t *testing.T) uint16 {
	t.Helper()
	var held sync.Mutex
	var connections []net.Conn
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		connection, buffered, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		held.Lock()
		connections = append(connections, connection)
		held.Unlock()
		_, _ = buffered.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: tunnel\r\n\r\n")
		_ = buffered.Flush()
	}))
	t.Cleanup(func() {
		upstream.Close()
		held.Lock()
		defer held.Unlock()
		for _, connection := range connections {
			_ = connection.Close()
		}
	})
	return uint16(upstream.Listener.Addr().(*net.TCPAddr).Port) //nolint:gosec // net.TCPAddr ports are bounded to uint16
}

func upgradeThrough(t *testing.T, address string) net.Conn {
	t.Helper()
	connection, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	if _, err := fmt.Fprint(connection, "GET /socket HTTP/1.1\r\nHost: localhost\r\nConnection: Upgrade\r\nUpgrade: tunnel\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade answered %s", response.Status)
	}
	return connection
}

func expectClosed(t *testing.T, connection net.Conn, what string) {
	t.Helper()
	_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := connection.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("%s left its hijacked upstream tunnel open (read: %v)", what, err)
	}
}

func TestDemandRemovalClosesHijackedConnections(t *testing.T) {
	port := upgradingUpstream(t)
	manager := testDemandManager(t, newFakeDemandSessions(), func() bool { return true })
	tunnel := meshserve.Service{Name: "4000", Kind: meshserve.Proxy, Target: "4000", LocalOnly: true,
		Listens: []meshserve.Listen{{Public: 4000, Upstream: port}}}
	manager.Sync([]meshserve.Service{tunnel})
	connection := upgradeThrough(t, listenerAddress(t, manager, 4000))

	manager.Sync(nil)
	expectClosed(t, connection, "unpublished route")
}

func TestDemandManagerCloseClosesHijackedConnections(t *testing.T) {
	port := upgradingUpstream(t)
	sessions := newFakeDemandSessions()
	manager := testDemandManager(t, sessions, func() bool { return true })
	service := demandService(time.Minute)
	service.Listens = []meshserve.Listen{{Public: 5173, Upstream: port}}
	manager.Sync([]meshserve.Service{service})
	connection := upgradeThrough(t, listenerAddress(t, manager, 5173))
	if status := manager.Status("dev"); status.Connections != 1 {
		t.Fatalf("status = %+v, want the upgraded connection counted", status)
	}

	manager.Close()
	expectClosed(t, connection, "closing the manager")
	deadline := time.Now().Add(2 * time.Second)
	for manager.Status("dev").Connections != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("connections = %d after close, want 0", manager.Status("dev").Connections)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if _, stopped := sessions.counts(); stopped != 0 || sessions.liveCount() != 1 {
		t.Fatal("closing the manager stopped the route's session")
	}
}

func TestDemandLaunchFailureAfterStartKeepsOwnership(t *testing.T) {
	sessions := newFakeDemandSessions()
	sessions.startErr = errors.New("readiness: worker did not answer")
	manager := testDemandManager(t, sessions, func() bool { return true })
	manager.Sync([]meshserve.Service{demandService(time.Minute)})

	if err := manager.Start(context.Background(), "dev"); err == nil {
		t.Fatal("Start succeeded although the worker's launch failed after it started")
	}
	if status := manager.Status("dev"); status.State != protocol.DemandFailed || status.SessionID != "S001" {
		t.Fatalf("status = %+v, want failed and still owning S001", status)
	}
	sessions.failStops(errWorkerSilent)
	if err := manager.Start(context.Background(), "dev"); err == nil {
		t.Fatal("Start succeeded while the half-launched worker may still run")
	}
	if started, _ := sessions.counts(); started != 1 {
		t.Fatalf("a launch that failed after start was followed by %d launches, want 1", started)
	}
}

func TestDemandRetirementRecoversWithoutAnotherSync(t *testing.T) {
	sessions := newFakeDemandSessions()
	manager := testDemandManager(t, sessions, func() bool { return true })
	manager.Sync([]meshserve.Service{demandService(time.Minute)})
	if err := manager.Start(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	sessions.failStops(errWorkerSilent)
	manager.Sync(nil)
	waitForSettledRoute(t, manager)

	sessions.failStops(nil)
	deadline := time.Now().Add(2 * time.Second)
	for sessions.liveCount() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("a removed route's worker kept running after its stop could succeed")
		}
		manager.supervise()
		time.Sleep(5 * time.Millisecond)
	}
	waitForSettledRoute(t, manager)
	manager.supervise()
	if retained := manager.Retiring(); len(retained) != 0 {
		t.Fatalf("a finished retirement is still reported: %+v", retained)
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if len(manager.retiring) != 0 {
		t.Fatal("a finished retirement is still retained")
	}
}

func TestDemandPersistentRetirementFailureIsBounded(t *testing.T) {
	sessions := newFakeDemandSessions()
	manager := testDemandManager(t, sessions, func() bool { return true })
	manager.cleanupRetry = time.Hour
	manager.Sync([]meshserve.Service{demandService(time.Minute)})
	if err := manager.Start(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	sessions.failStops(errWorkerSilent)
	manager.Sync(nil)
	for range 5 {
		waitForSettledRoute(t, manager)
		manager.supervise()
		manager.Sync(nil)
	}
	waitForSettledRoute(t, manager)
	if _, stopped := sessions.counts(); stopped != 1 {
		t.Fatalf("a stop that keeps failing was retried %d times at once, want 1 attempt until the retry is due", stopped)
	}
	if sessions.liveCount() != 1 {
		t.Fatal("the retained worker is no longer accounted for")
	}
	retained := manager.Retiring()
	if len(retained) != 1 || retained[0].Name != "dev" || retained[0].Healthy || retained[0].Demand == nil ||
		retained[0].Demand.SessionID != "S001" || !strings.Contains(retained[0].Problem, "S001") {
		t.Fatalf("removed route still owning S001 is reported as %+v", retained)
	}
}

func TestWithRetiringKeepsCanonicalOrderAndRegisteredServices(t *testing.T) {
	got := withRetiring(
		[]protocol.ServiceInfo{{Name: "api"}, {Name: "web"}},
		[]protocol.ServiceInfo{{Name: "dev"}, {Name: "zed"}},
	)
	var names []string
	for _, info := range got {
		names = append(names, info.Name)
	}
	if strings.Join(names, ",") != "api,dev,web,zed" {
		t.Fatalf("listed %v, want canonical order", names)
	}

	full := make([]protocol.ServiceInfo, meshserve.MaximumServices)
	for index := range full {
		full[index].Name = fmt.Sprintf("s%04d", index)
	}
	if got := withRetiring(full, []protocol.ServiceInfo{{Name: "a"}}); len(got) != meshserve.MaximumServices || got[0].Name != "s0000" {
		t.Fatalf("a full list grew to %d or lost a registered service", len(got))
	}
}
