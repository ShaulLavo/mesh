package daemon

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	mu       sync.Mutex
	next     int
	live     map[string]string // id → label
	exits    map[string]int
	started  []string
	stopped  []string
	exitNow  *int // a started session exits at once with this code
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
	return id, nil
}

func (f *fakeDemandSessions) stopSession(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, id)
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
		_, _ = fmt.Fprintf(w, "upstream saw %s", request.URL.Path)
	}))
	defer upstream.Close()
	_, upstreamPort, _ := net.SplitHostPort(upstream.Listener.Addr().String())
	port, _ := strconv.Atoi(upstreamPort)

	sessions := newFakeDemandSessions()
	manager := testDemandManager(t, sessions, func() bool { return true })
	plain := meshserve.Service{Name: "4000", Kind: meshserve.Proxy, Target: "4000", LocalOnly: true,
		Listens: []meshserve.Listen{{Public: 4000, Upstream: uint16(port)}}}
	onDemand := demandService(time.Minute)
	onDemand.Listens = []meshserve.Listen{{Public: 5173, Upstream: uint16(port)}}
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
