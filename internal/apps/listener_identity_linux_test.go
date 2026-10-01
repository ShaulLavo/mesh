package apps

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

const squatterPortEnv = "MESH_APPS_TEST_SQUAT_PORT"

// TestListenerSquatterProcess only runs as a helper: the identity tests start
// it as a separate process holding a port, as another program on the host would.
func TestListenerSquatterProcess(t *testing.T) {
	port := os.Getenv(squatterPortEnv)
	if port == "" {
		t.Skip("helper process for the listener identity tests")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:"+port)
	if err != nil {
		fmt.Println("listen:", err)
		os.Exit(1)
	}
	fmt.Println("ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
	_ = listener.Close()
	os.Exit(0)
}

// startSquatter holds 127.0.0.1:port from another process until the test ends.
func startSquatter(t *testing.T, port int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestListenerSquatterProcess$") //nolint:gosec // re-runs this test binary as a helper
	cmd.Env = append(os.Environ(), squatterPortEnv+"="+strconv.Itoa(port))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "ready" {
		t.Fatalf("squatter on port %d did not start: %q %v", port, line, err)
	}
}

// squattedWorkers starts app workers that never listen, while another process
// takes the port between the origin's preflight and the app's own bind.
type squattedWorkers struct {
	*fakeWorkers
	t    *testing.T
	port int
}

func (w *squattedWorkers) Start(ctx context.Context, label, command, root string, env []string) (string, error) {
	if strings.HasPrefix(label, "app ") {
		startSquatter(w.t, w.port)
	}
	return w.fakeWorkers.Start(ctx, label, command, root, env)
}

func workerListener(t *testing.T, w *serverWorkers, app Record) net.Listener {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	listener, ok := w.listeners["worker-app "+app.ID]
	if !ok {
		t.Fatalf("app %s has no listener", app.ID)
	}
	return listener
}

func edgeRecord(t *testing.T, f *appFixture, id string) Record {
	t.Helper()
	record, _, err := f.edge.lookup(context.Background(), id, false)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func serveText(t *testing.T, listener net.Listener, text string) *http.Server {
	t.Helper()
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, text)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return server
}

func TestReadinessRejectsAListenerOutsideTheAppSession(t *testing.T) {
	f := newAppFixture(t)
	port := freePort(t)
	f.origin.config.Workers = &squattedWorkers{fakeWorkers: f.workers, t: t, port: port}
	upload, digest := uploadSource(t, f, sourceFixture(t))
	started := time.Now()
	_, err := f.origin.Handle(context.Background(), Request{Action: "create", Kind: "server", Command: "serve", Port: port, UploadID: upload, Digest: digest})
	if err == nil {
		t.Fatalf("app became ready on port %d, which another process holds", port)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("readiness waited %s on a foreign listener instead of rejecting it: %v", elapsed, err)
	}
	if !strings.Contains(err.Error(), "outside the app") {
		t.Fatalf("readiness failed for another reason: %v", err)
	}
}

func TestSyncWithdrawsAnAppThatListensBeyondLoopback(t *testing.T) {
	f := newAppFixture(t)
	workers := newServerWorkers(t, f)
	app := createServerApp(t, f)
	wildcard, err := net.Listen("tcp4", "0.0.0.0:0") //nolint:gosec // the app exposing a second port is the fixture
	if err != nil {
		t.Fatal(err)
	}
	_ = f.origin.Sync(context.Background())
	if edgeRecord(t, f, app.ID).Ready {
		t.Fatalf("edge still routes app %s after it opened %s", app.ID, wildcard.Addr())
	}
	if code := serveStatus(t, f, app); code != http.StatusServiceUnavailable {
		t.Fatalf("origin served app %s with status %d after it opened %s", app.ID, code, wildcard.Addr())
	}
	if workers.wasStopped(app.ID) {
		t.Fatalf("app %s worker was stopped; withdrawing the route is the response", app.ID)
	}
	inspected, err := f.origin.Handle(context.Background(), Request{Action: "inspect", ID: app.ID})
	if err != nil || inspected.Runtime == nil || !strings.Contains(inspected.Runtime.Problem, wildcard.Addr().String()) {
		t.Fatalf("owner is not told why app %s stopped serving: %+v %v", app.ID, inspected.Runtime, err)
	}
	if err := wildcard.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.origin.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	resumed := edgeRecord(t, f, app.ID)
	if !resumed.Ready || resumed.Generation != app.Generation {
		t.Fatalf("app %s after its wildcard listener closed: ready %t, generation %d; want ready at generation %d", app.ID, resumed.Ready, resumed.Generation, app.Generation)
	}
	if !(*f.origin.routes.Load())[app.ID].Upstream.IsValid() {
		t.Fatalf("resumed app %s is still refused at the origin", app.ID)
	}
}

func TestSyncWithdrawsAnAppWhosePortAnotherProcessHolds(t *testing.T) {
	f := newAppFixture(t)
	workers := newServerWorkers(t, f)
	port := freePort(t)
	app := createServerAppOn(t, f, port)
	if err := workerListener(t, workers, app).Close(); err != nil {
		t.Fatal(err)
	}
	startSquatter(t, port)
	_ = f.origin.Sync(context.Background())
	if edgeRecord(t, f, app.ID).Ready {
		t.Fatalf("edge still routes app %s to port %d, which another process holds", app.ID, port)
	}
	if code := serveStatus(t, f, app); code != http.StatusServiceUnavailable {
		t.Fatalf("origin served app %s with status %d from a squatted port", app.ID, code)
	}
	if workers.wasStopped(app.ID) {
		t.Fatalf("app %s worker was stopped for a squatter it may be the victim of", app.ID)
	}
}

func TestProxyNeverFallsBackToIPv6Loopback(t *testing.T) {
	f := newAppFixture(t)
	workers := newServerWorkers(t, f)
	port := freePort(t)
	app := createServerAppOn(t, f, port)
	server := serveText(t, workerListener(t, workers, app), "app")
	if code := serveStatus(t, f, app); code != http.StatusOK {
		t.Fatalf("verified app %s answered %d", app.ID, code)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	squatter, err := net.Listen("tcp6", net.JoinHostPort("::1", strconv.Itoa(port)))
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	serveText(t, squatter, "squatter")
	if code := serveStatus(t, f, app); code == http.StatusOK {
		t.Fatalf("proxy for app %s reached [::1]:%d after its verified 127.0.0.1 listener closed", app.ID, port)
	}
}

func TestUpdatingASuspendedAppStartsANewGeneration(t *testing.T) {
	f := newAppFixture(t)
	workers := newServerWorkers(t, f)
	port := freePort(t)
	app := createServerAppOn(t, f, port)
	f.now = app.ExpiresAt.Add(-time.Minute)
	if err := workerListener(t, workers, app).Close(); err != nil {
		t.Fatal(err)
	}
	startSquatter(t, port)
	_ = f.origin.Sync(context.Background())
	if edgeRecord(t, f, app.ID).Ready {
		t.Fatalf("app %s was not suspended", app.ID)
	}
	upload, digest := uploadSource(t, f, sourceFixture(t))
	if _, err := f.origin.Handle(context.Background(), Request{Action: "update", ID: app.ID, Kind: "static", UploadID: upload, Digest: digest}); err != nil {
		t.Fatal(err)
	}
	updated := edgeRecord(t, f, app.ID)
	if !updated.Ready || updated.Generation != app.Generation+1 || !updated.ExpiresAt.Equal(f.now.Add(IdleTTL)) {
		t.Fatalf("update of suspended app %s: ready %t, generation %d, expires %s; want ready, generation %d, expires %s",
			app.ID, updated.Ready, updated.Generation, updated.ExpiresAt, app.Generation+1, f.now.Add(IdleTTL))
	}
	f.now = app.ExpiresAt.Add(time.Minute)
	_ = f.origin.Sync(context.Background())
	if record := edgeRecord(t, f, app.ID); record.Status != "active" {
		t.Fatalf("updated app %s was %s at its old deadline", app.ID, record.Status)
	}
}

func TestCachedRouteFollowsTheVerifiedUpstream(t *testing.T) {
	app := localApp{Record: Record{ID: "7k3d", Kind: "server", Status: "active", Revision: "r1"}, Root: "/apps/7k3d", Port: 3000, Phase: "ready"}
	first := cachedAppRoute(app, appRoute{}, netip.MustParseAddrPort("127.0.0.1:3000"))
	if first.Handler == nil {
		t.Fatal("verified server app has no handler")
	}
	if again := cachedAppRoute(app, first, first.Upstream); again.Handler != first.Handler {
		t.Fatal("unchanged route rebuilt its handler")
	}
	if moved := cachedAppRoute(app, first, netip.MustParseAddrPort("[::1]:3000")); moved.Handler == first.Handler || moved.Handler == nil {
		t.Fatal("handler for 127.0.0.1 reused for [::1]")
	}
	if withdrawn := cachedAppRoute(app, first, netip.AddrPort{}); withdrawn.Handler != nil {
		t.Fatal("server app without a verified upstream kept a handler")
	}
}
