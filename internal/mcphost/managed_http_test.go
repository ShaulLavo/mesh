package mcphost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/mcpwire"
)

// managedHTTPLauncher simulates a plain pipe worker that spawns an in-process
// loopback HTTP MCP server at the configured address, with optional startup
// delay, legacy mode, or crash injection.
type managedHTTPLauncher struct {
	mu       sync.Mutex
	addr     string
	delay    time.Duration
	legacy   bool
	exitCode *int
	launches int
	servers  map[string]*managedFakeServer
	stderr   map[string]string
}

type managedFakeServer struct {
	srv      *http.Server
	ln       net.Listener
	ended    chan struct{}
	exitCode *int
	once     sync.Once
}

func (m *managedFakeServer) stop(code *int) {
	m.once.Do(func() {
		m.exitCode = code
		if m.srv != nil {
			_ = m.srv.Close()
		}
		if m.ln != nil {
			_ = m.ln.Close()
		}
		close(m.ended)
	})
}

func (l *managedHTTPLauncher) Launch(_ context.Context, spec LaunchSpec) (WorkerRef, error) {
	if !spec.PlainPipe {
		return WorkerRef{}, errors.New("expected plain pipe worker for managed HTTP")
	}
	l.mu.Lock()
	l.launches++
	n := l.launches
	addr, delay, legacy, earlyExit := l.addr, l.delay, l.legacy, l.exitCode
	l.mu.Unlock()

	key := spec.Key.Opaque()
	ms := &managedFakeServer{ended: make(chan struct{})}
	l.mu.Lock()
	if l.servers == nil {
		l.servers = map[string]*managedFakeServer{}
		l.stderr = map[string]string{}
	}
	l.servers[key] = ms
	l.stderr[key] = fmt.Sprintf("managed-http generation=%d cwd=%s\n", n, spec.Cwd)
	l.mu.Unlock()

	if earlyExit != nil {
		code := *earlyExit
		go func() {
			time.Sleep(20 * time.Millisecond)
			ms.stop(&code)
		}()
		return WorkerRef{SessionID: key, Instance: key, LaunchDigest: key}, nil
	}

	go func() {
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-ms.ended:
				return
			}
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			code := 1
			ms.stop(&code)
			return
		}
		srv := &http.Server{Handler: managedHTTPHandler(n, legacy), ReadHeaderTimeout: 5 * time.Second}
		l.mu.Lock()
		select {
		case <-ms.ended:
			l.mu.Unlock()
			_ = ln.Close()
			return
		default:
			ms.ln, ms.srv = ln, srv
		}
		l.mu.Unlock()
		_ = srv.Serve(ln)
	}()
	return WorkerRef{SessionID: key, Instance: key, LaunchDigest: key}, nil
}

func managedHTTPHandler(generation int, legacy bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		req, fault := mcpwire.Parse(body)
		if fault != nil {
			http.Error(w, fault.Message, http.StatusBadRequest)
			return
		}
		id, _ := req.ID().MarshalJSON()
		w.Header().Set("Content-Type", "application/json")
		switch req.Method() {
		case mcpwire.MethodDiscover:
			if legacy {
				_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"not found"}}`, id)
				return
			}
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"resultType":"complete","supportedVersions":["2026-07-28"],"capabilities":{"tools":{}},"ttlMs":60000,"cacheScope":"private","_meta":{"io.modelcontextprotocol/serverInfo":{"name":"managed-http-fixture","version":"%d"}}}}`, id, generation)
		case mcpwire.MethodListTools:
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"resultType":"complete","tools":[{"name":"gen","inputSchema":{"type":"object"}}],"ttlMs":60000,"cacheScope":"private"}}`, id)
		case mcpwire.MethodCallTool:
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"resultType":"complete","content":[{"type":"text","text":"managed-gen=%d"}]}}`, id, generation)
		default:
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"unknown"}}`, id)
		}
	})
}

func (l *managedHTTPLauncher) Locate(_ context.Context, key LaunchKey) (WorkerRef, WorkerStatus, *int, error) {
	l.mu.Lock()
	ms := l.servers[key.Opaque()]
	l.mu.Unlock()
	if ms == nil {
		return WorkerRef{}, WorkerAbsent, nil, nil
	}
	select {
	case <-ms.ended:
		return WorkerRef{SessionID: key.Opaque(), Instance: key.Opaque(), LaunchDigest: key.Opaque()}, WorkerEnded, ms.exitCode, nil
	default:
		return WorkerRef{SessionID: key.Opaque(), Instance: key.Opaque(), LaunchDigest: key.Opaque()}, WorkerLive, nil, nil
	}
}

func (l *managedHTTPLauncher) Stop(_ context.Context, ref WorkerRef) error {
	l.mu.Lock()
	ms := l.servers[ref.SessionID]
	l.mu.Unlock()
	if ms != nil {
		code := 0
		ms.stop(&code)
	}
	return nil
}

func (l *managedHTTPLauncher) Stderr(_ context.Context, ref WorkerRef, _ int) ([]byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return []byte(l.stderr[ref.SessionID]), nil
}

func (l *managedHTTPLauncher) Dial(context.Context, WorkerRef) (LineConn, error) {
	return nil, errors.New("managed HTTP uses plain pipe workers, not line attach")
}

func (l *managedHTTPLauncher) Wait(ctx context.Context, ref WorkerRef) error {
	l.mu.Lock()
	ms := l.servers[ref.SessionID]
	l.mu.Unlock()
	if ms == nil {
		return nil
	}
	select {
	case <-ms.ended:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for managed fixture exit: %w", ctx.Err())
	}
}

func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func TestManagedHTTPFullLifecycle(t *testing.T) {
	addr := freeLoopbackAddr(t)
	store := newMemStore()
	launcher := &managedHTTPLauncher{addr: addr, delay: 80 * time.Millisecond}
	sup, err := NewSupervisor(Config{Store: store, Launcher: launcher, HostDefaultCwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sup.Close)

	admin := Caller{Account: "local", Admin: true}
	def := Definition{
		Name:           "managed-web",
		URL:            "http://" + addr + "/mcp",
		RunArgv:        []string{"/bin/sh", "-c", "echo managed"},
		RunCwd:         t.TempDir(),
		InstancePolicy: PolicyShared,
		AccountMode:    AccountHostAccount,
		AccountLabel:   "web bot",
		Idle:           120 * time.Millisecond,
	}
	if _, err := sup.Publish(t.Context(), def, 0); err != nil {
		t.Fatal(err)
	}

	// Probe starts the plain pipe worker, retries across the 80ms port-bind
	// delay, verifies 2026-07-28, and returns Ready.
	probed, err := sup.Probe(t.Context(), admin, Target{Service: "managed-web"})
	if err != nil || probed.State != StateReady || probed.Generation != 1 {
		t.Fatalf("probe managed HTTP: %+v %v", probed, err)
	}

	out := &collected{}
	if err := sup.Forward(t.Context(), caller("a"), Target{Service: "managed-web"}, callRequest(t, `"c1"`, toolParams("gen", `{}`, "")), out.emit); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.last(), "managed-gen=1") {
		t.Fatalf("managed HTTP call: %s", out.last())
	}

	logs, err := sup.Logs(t.Context(), admin, Target{Service: "managed-web"}, 0)
	if err != nil || !strings.Contains(string(logs), "managed-http generation=1") {
		t.Fatalf("managed HTTP logs: %q %v", logs, err)
	}

	// Restart replaces the worker generation and binds the same loopback port.
	restarted, err := sup.Restart(t.Context(), admin, Target{Service: "managed-web"})
	if err != nil || restarted.State != StateReady || restarted.Generation != 2 {
		t.Fatalf("restart managed HTTP: %+v %v", restarted, err)
	}
	out2 := &collected{}
	if err := sup.Forward(t.Context(), caller("a"), Target{Service: "managed-web"}, callRequest(t, `"c2"`, toolParams("gen", `{}`, "")), out2.emit); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out2.last(), "managed-gen=2") {
		t.Fatalf("after restart: %s", out2.last())
	}

	// Idle expiry stops the owned process automatically.
	eventually(t, func() bool {
		return sup.Catalog(admin)[0].Instances[0].State == StateStopped
	})

	// Explicit start brings it back as generation 3; daemon restart adopts it
	// without launching a fourth process.
	started, err := sup.StartInstance(t.Context(), admin, Target{Service: "managed-web"})
	if err != nil || started.State != StateReady || started.Generation != 3 {
		t.Fatalf("start after idle: %+v %v", started, err)
	}
	// Cancel idle by disabling idle before restart adoption check.
	sup.Close()
	next, err := NewSupervisor(Config{Store: store, Launcher: launcher, HostDefaultCwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := next.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(next.Close)
	eventually(t, func() bool {
		return next.Catalog(admin)[0].Instances[0].State == StateReady
	})
	if launcher.launches != 3 {
		t.Fatalf("daemon restart launched a new managed HTTP worker: launches=%d, want 3", launcher.launches)
	}

	// Explicit stop stops the worker cleanly.
	if _, err := next.StopInstance(t.Context(), admin, Target{Service: "managed-web"}); err != nil {
		t.Fatal(err)
	}
	if state := next.Catalog(admin)[0].Instances[0].State; state != StateStopped {
		t.Fatalf("state after stop = %s, want stopped", state)
	}
}

func TestManagedHTTPKeptRestartsAfterCrash(t *testing.T) {
	addr := freeLoopbackAddr(t)
	store := newMemStore()
	launcher := &managedHTTPLauncher{addr: addr}
	sup, err := NewSupervisor(Config{Store: store, Launcher: launcher, HostDefaultCwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sup.Close)

	admin := Caller{Account: "local", Admin: true}
	_, err = sup.Publish(t.Context(), Definition{
		Name:           "kept-web",
		URL:            "http://" + addr + "/mcp",
		RunArgv:        []string{"/bin/sh", "-c", "echo kept"},
		InstancePolicy: PolicyShared,
		Lifecycle:      LifecycleKept,
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		v := sup.Catalog(admin)[0]
		return len(v.Instances) == 1 && v.Instances[0].State == StateReady && v.Instances[0].Generation == 1
	})
	// Crash the managed HTTP worker unexpectedly; watchManagedExit detects it
	// and kept restart backoff starts generation 2.
	launcher.mu.Lock()
	for _, ms := range launcher.servers {
		code := 1
		ms.stop(&code)
	}
	launcher.mu.Unlock()
	eventually(t, func() bool {
		v := sup.Catalog(admin)[0]
		return len(v.Instances) == 1 && v.Instances[0].State == StateReady && v.Instances[0].Generation == 2
	})
}

func TestManagedHTTPIncompatibleAndEarlyExit(t *testing.T) {
	admin := Caller{Account: "local", Admin: true}
	t.Run("incompatible server is stopped and not retried", func(t *testing.T) {
		addr := freeLoopbackAddr(t)
		launcher := &managedHTTPLauncher{addr: addr, legacy: true}
		sup, err := NewSupervisor(Config{Store: newMemStore(), Launcher: launcher, HostDefaultCwd: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		if err := sup.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(sup.Close)
		_, err = sup.Publish(t.Context(), Definition{
			Name: "legacy-web", URL: "http://" + addr + "/mcp", RunArgv: []string{"/bin/sh", "-c", "x"}, InstancePolicy: PolicyShared,
		}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sup.StartInstance(t.Context(), admin, Target{Service: "legacy-web"}); Classify(err).Code != codeIncompatible {
			t.Fatalf("legacy managed HTTP start: %v", err)
		}
	})
	t.Run("early exit classifies exit code", func(t *testing.T) {
		addr := freeLoopbackAddr(t)
		code := 42
		launcher := &managedHTTPLauncher{addr: addr, exitCode: &code}
		sup, err := NewSupervisor(Config{Store: newMemStore(), Launcher: launcher, HostDefaultCwd: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		if err := sup.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(sup.Close)
		_, err = sup.Publish(t.Context(), Definition{
			Name: "crash-web", URL: "http://" + addr + "/mcp", RunArgv: []string{"/bin/sh", "-c", "exit 42"}, InstancePolicy: PolicyShared,
		}, 0)
		if err != nil {
			t.Fatal(err)
		}
		view, err := sup.StartInstance(t.Context(), admin, Target{Service: "crash-web"})
		if Classify(err).Code != codeFailed || view.ExitCode == nil || *view.ExitCode != 42 {
			t.Fatalf("early exit managed HTTP: view=%+v err=%v", view, err)
		}
	})
}

// TestOversizedStdioReplyFailsOnlyThatRequest verifies PLAN §2.5: an oversized
// stdio server line whose ID and delimiter are recovered fails only that
// request with mcp.message_too_large while keeping the instance healthy.
func TestOversizedStdioReplyFailsOnlyThatRequest(t *testing.T) {
	conn := &oversizeLineConn{
		incoming: make(chan []byte, 4),
		outgoing: make(chan oversizeItem, 4),
		done:     make(chan struct{}),
	}
	router, err := NewRouter()
	if err != nil {
		t.Fatal(err)
	}
	upstream := newStdioUpstream(t.Context(), conn, router)
	defer func() { _ = upstream.Close() }()

	// Fake server: request 1 gets an attributable ErrOversizedLine; request 2
	// succeeds normally on the very same StdioUpstream.
	var count atomic.Int32
	go func() {
		for raw := range conn.incoming {
			req, _ := mcpwire.Parse(raw)
			if count.Add(1) == 1 {
				conn.outgoing <- oversizeItem{err: &ErrOversizedLine{ID: req.ID()}}
				continue
			}
			id, _ := req.ID().MarshalJSON()
			conn.outgoing <- oversizeItem{line: []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"resultType":"complete","content":[{"type":"text","text":"still-alive"}]}}`, id))}
		}
	}()

	route := Route{Caller: caller("a"), Service: "svc", Revision: 1, Instance: "inst", Generation: "1", Upstream: upstream}
	err = router.Forward(t.Context(), route, callRequest(t, `"req-big"`, toolParams("big", `{}`, "")), (&collected{}).emit)
	if Classify(err).Code != codeMessageTooLarge {
		t.Fatalf("oversized reply error = %v (code %s), want %s", err, Classify(err).Code, codeMessageTooLarge)
	}
	if upstream.Err() != nil {
		t.Fatalf("attributable oversized reply faulted the upstream: %v", upstream.Err())
	}

	out := &collected{}
	if err := router.Forward(t.Context(), route, callRequest(t, `"req-ok"`, toolParams("echo", `{}`, "")), out.emit); err != nil {
		t.Fatalf("subsequent call after oversized reply failed: %v", err)
	}
	if !strings.Contains(out.last(), "still-alive") {
		t.Fatalf("subsequent call result = %s", out.last())
	}
}

type oversizeItem struct {
	line []byte
	err  error
}

type oversizeLineConn struct {
	incoming chan []byte
	outgoing chan oversizeItem
	done     chan struct{}
	once     sync.Once
}

func (c *oversizeLineConn) WriteLine(ctx context.Context, line []byte) error {
	select {
	case c.incoming <- append([]byte(nil), line...):
		return nil
	case <-c.done:
		return errors.New("closed")
	case <-ctx.Done():
		return fmt.Errorf("write test line: %w", ctx.Err())
	}
}

func (c *oversizeLineConn) ReadLine() ([]byte, error) {
	select {
	case item := <-c.outgoing:
		return item.line, item.err
	case <-c.done:
		return nil, ErrStdoutClosed
	}
}

func (c *oversizeLineConn) Close() error {
	c.once.Do(func() {
		close(c.done)
		close(c.incoming)
	})
	return nil
}

func TestExternalHTTPWarmProbeDetectsUnavailable(t *testing.T) {
	f := newSupervisorFixture(t)
	var fail atomic.Bool
	endpoint := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		managedHTTPHandler(1, false).ServeHTTP(w, r)
	}))
	f.publish(t, Definition{Name: "ext", Argv: []string{}, URL: endpoint, InstancePolicy: PolicyShared})
	admin := Caller{Account: "local", Admin: true}
	if view, err := f.sup.Probe(t.Context(), admin, Target{Service: "ext"}); err != nil || view.State != StateReady {
		t.Fatalf("initial probe: %+v %v", view, err)
	}
	fail.Store(true)
	if view, err := f.sup.Probe(t.Context(), admin, Target{Service: "ext"}); err == nil || view.State != StateFailed {
		t.Fatalf("warm probe against failing external server: view=%+v err=%v", view, err)
	}
	// Ensure no secret files leak via bad header secret before managed HTTP launch.
	badSecret := filepath.Join(t.TempDir(), "loose")
	if err := os.WriteFile(badSecret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	mode := os.FileMode(0o644)
	if err := os.Chmod(badSecret, mode); err != nil {
		t.Fatal(err)
	}
	_, err := f.sup.Publish(t.Context(), Definition{
		Name: "bad-secret-web", URL: endpoint, RunArgv: []string{"/bin/sh", "-c", "sleep 1"}, InstancePolicy: PolicyShared,
		AccountMode: AccountHostAccount, AccountLabel: "bot", HeaderSecrets: map[string]string{"Authorization": badSecret},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.sup.StartInstance(t.Context(), admin, Target{Service: "bad-secret-web"}); err == nil {
		t.Fatal("managed HTTP started despite loose header secret permissions")
	}
	if f.launcher.launches != 0 {
		t.Fatalf("managed HTTP launched worker despite invalid header secret: launches=%d", f.launcher.launches)
	}
}
