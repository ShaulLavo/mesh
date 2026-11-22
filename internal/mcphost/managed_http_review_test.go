package mcphost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// TestManagedHTTPOccupiedPortNeverLaunchesOrLeaksSecret covers review finding 1:
// an already-listening compatible server on the target loopback port must fail
// before Mesh spawns a worker or sends configured header credentials.
func TestManagedHTTPOccupiedPortNeverLaunchesOrLeaksSecret(t *testing.T) {
	var hits atomic.Int32
	var leakedAuth atomic.Value
	incumbent := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if auth := r.Header.Get("Authorization"); auth != "" {
			leakedAuth.Store(auth)
		}
		managedHTTPHandler(99).ServeHTTP(w, r)
	}))
	addr := strings.TrimPrefix(strings.TrimSuffix(incumbent, "/mcp"), "http://")
	secretFile := filepath.Join(t.TempDir(), "auth")
	if err := os.WriteFile(secretFile, []byte("Bearer top-secret-cred"), 0o600); err != nil {
		t.Fatal(err)
	}

	launcher := &managedHTTPLauncher{addr: addr}
	sup, err := NewSupervisor(Config{Store: newMemStore(), Launcher: launcher, HostDefaultCwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sup.Close)

	admin := Caller{Account: "local", Admin: true}
	_, err = sup.Publish(t.Context(), Definition{
		Name:           "occupied-svc",
		URL:            incumbent,
		RunArgv:        []string{"/bin/sh", "-c", "sleep 1"},
		InstancePolicy: PolicyShared,
		AccountMode:    AccountHostAccount,
		AccountLabel:   "bot",
		HeaderSecrets:  map[string]string{"Authorization": secretFile},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sup.StartInstance(t.Context(), admin, Target{Service: "occupied-svc"}); err == nil {
		t.Fatal("managed HTTP started on an already-occupied port")
	}
	if launcher.launches != 0 {
		t.Fatalf("launcher spawned %d workers on an occupied port, want 0", launcher.launches)
	}
	if hits.Load() != 0 || leakedAuth.Load() != nil {
		t.Fatalf("incumbent received %d requests (auth=%v), want 0", hits.Load(), leakedAuth.Load())
	}
}

// TestManagedHTTPCompetingDefinitionsAndTakeover covers competing managed HTTP
// definitions targeting the same loopback port and a rogue listener taking over
// after the owned worker exits.
func TestManagedHTTPCompetingDefinitionsAndTakeover(t *testing.T) {
	addr := freeLoopbackAddr(t)
	secretA := filepath.Join(t.TempDir(), "secret-a")
	secretB := filepath.Join(t.TempDir(), "secret-b")
	if err := os.WriteFile(secretA, []byte("Bearer secret-A"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secretB, []byte("Bearer secret-B"), 0o600); err != nil {
		t.Fatal(err)
	}

	launcher := &managedHTTPLauncher{addr: addr}
	store := newMemStore()
	sup, err := NewSupervisor(Config{Store: store, Launcher: launcher, HostDefaultCwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sup.Close)

	admin := Caller{Account: "local", Admin: true}
	for _, item := range []struct {
		name, secret string
	}{
		{"svc-a", secretA},
		{"svc-b", secretB},
	} {
		_, err := sup.Publish(t.Context(), Definition{
			Name:           item.name,
			URL:            "http://" + addr + "/mcp",
			RunArgv:        []string{"/bin/sh", "-c", "sleep 1"},
			InstancePolicy: PolicyShared,
			AccountMode:    AccountHostAccount,
			AccountLabel:   item.name,
			HeaderSecrets:  map[string]string{"Authorization": item.secret},
		}, 0)
		if err != nil {
			t.Fatal(err)
		}
	}
	if view, err := sup.StartInstance(t.Context(), admin, Target{Service: "svc-a"}); err != nil || view.State != StateReady {
		t.Fatalf("start svc-a: %+v %v", view, err)
	}
	// Competing definition on the same port must fail before launching a worker.
	if _, err := sup.StartInstance(t.Context(), admin, Target{Service: "svc-b"}); err == nil {
		t.Fatal("competing managed HTTP service started on svc-a's port")
	}
	if launcher.launches != 1 {
		t.Fatalf("competing service caused launcher.launches=%d, want 1", launcher.launches)
	}

	// Kill svc-a's worker and bind a rogue compatible listener on addr.
	// Neither a live call nor daemon-restart adoption may send secret-A to it.
	launcher.mu.Lock()
	for _, ms := range launcher.servers {
		code := 1
		ms.stop(&code)
	}
	launcher.mu.Unlock()

	var rogueHits atomic.Int32
	var rogueAuth atomic.Value
	rogueLn, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("bind rogue listener on %s: %v", addr, err)
	}
	rogueSrv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rogueHits.Add(1)
			if auth := r.Header.Get("Authorization"); auth != "" {
				rogueAuth.Store(auth)
			}
			managedHTTPHandler(666).ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = rogueSrv.Serve(rogueLn) }()
	t.Cleanup(func() { _ = rogueSrv.Close() })

	out := &collected{}
	if err := sup.Forward(t.Context(), caller("a"), Target{Service: "svc-a"}, callRequest(t, `"takeover"`, toolParams("gen", `{}`, "")), out.emit); err == nil {
		t.Fatalf("call after worker exit succeeded against rogue listener: %s", out.last())
	}
	if rogueHits.Load() != 0 || rogueAuth.Load() != nil {
		t.Fatalf("rogue listener received %d requests (auth=%v) after worker exit", rogueHits.Load(), rogueAuth.Load())
	}

	// Daemon-restart adoption must also refuse the rogue listener without sending credentials.
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
		for _, svc := range next.Catalog(admin) {
			if svc.Name == "svc-a" && len(svc.Instances) == 1 {
				return svc.Instances[0].State == StateFailed || svc.Instances[0].State == StateStopped
			}
		}
		return false
	})
	if rogueHits.Load() != 0 || rogueAuth.Load() != nil {
		t.Fatalf("rogue listener received %d requests (auth=%v) during adoption", rogueHits.Load(), rogueAuth.Load())
	}
}

// TestManagedHTTPDrainWaitsForPredecessorAndRecoveryHonoursStopIntent covers
// review finding 2.
func TestManagedHTTPDrainWaitsForPredecessorAndRecoveryHonoursStopIntent(t *testing.T) {
	t.Run("same-port republish waits for active predecessor call to drain and exit", func(t *testing.T) {
		addr := freeLoopbackAddr(t)
		callEntered := make(chan struct{})
		releaseCall := make(chan struct{})
		launcher := &managedHTTPLauncher{
			addr: addr,
			onCall: func(gen int) {
				if gen == 1 {
					select {
					case <-callEntered:
					default:
						close(callEntered)
					}
					<-releaseCall
				}
			},
		}
		sup, err := NewSupervisor(Config{Store: newMemStore(), Launcher: launcher, HostDefaultCwd: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		if err := sup.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(sup.Close)

		def := Definition{
			Name: "drain-web", URL: "http://" + addr + "/mcp",
			RunArgv: []string{"/bin/sh", "-c", "sleep 1"}, InstancePolicy: PolicyShared,
		}
		if _, err := sup.Publish(t.Context(), def, 0); err != nil {
			t.Fatal(err)
		}
		call1Done := make(chan error, 1)
		out1 := &collected{}
		go func() {
			call1Done <- sup.Forward(t.Context(), caller("a"), Target{Service: "drain-web"}, callRequest(t, `"c1"`, toolParams("gen", `{}`, "")), out1.emit)
		}()
		<-callEntered

		// Republish revision 2 on the exact same loopback port while gen 1 is active.
		def.RunArgv = []string{"/bin/sh", "-c", "sleep 2"}
		if _, err := sup.Publish(t.Context(), def, 1); err != nil {
			t.Fatal(err)
		}
		call2Done := make(chan error, 1)
		out2 := &collected{}
		go func() {
			call2Done <- sup.Forward(t.Context(), caller("a"), Target{Service: "drain-web"}, callRequest(t, `"c2"`, toolParams("gen", `{}`, "")), out2.emit)
		}()

		// Give rev 2 time to enter launch and confirm gen 2 has NOT spawned yet.
		time.Sleep(150 * time.Millisecond)
		launcher.mu.Lock()
		launchesWhileActive := launcher.launches
		launcher.mu.Unlock()
		if launchesWhileActive != 1 {
			close(releaseCall)
			t.Fatalf("launches while predecessor active = %d, want 1", launchesWhileActive)
		}

		// Release gen 1's active call: gen 1 finishes and stops, then gen 2 launches.
		close(releaseCall)
		if err := <-call1Done; err != nil || !strings.Contains(out1.last(), "managed-gen=1") {
			t.Fatalf("predecessor call = %q, %v", out1.last(), err)
		}
		if err := <-call2Done; err != nil || !strings.Contains(out2.last(), "managed-gen=2") {
			t.Fatalf("replacement call = %q, %v", out2.last(), err)
		}
	})

	t.Run("failed predecessor stop blocks replacement launch", func(t *testing.T) {
		addr := freeLoopbackAddr(t)
		base := &managedHTTPLauncher{addr: addr}
		var failStop atomic.Bool
		failStop.Store(true)
		launcher := &stopFailLauncher{managedHTTPLauncher: base, failStop: &failStop}
		sup, err := NewSupervisor(Config{Store: newMemStore(), Launcher: launcher, HostDefaultCwd: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		if err := sup.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(sup.Close)

		admin := Caller{Account: "local", Admin: true}
		def := Definition{
			Name: "repub-web", URL: "http://" + addr + "/mcp",
			RunArgv: []string{"/bin/sh", "-c", "sleep 1"}, InstancePolicy: PolicyShared,
		}
		if _, err := sup.Publish(t.Context(), def, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := sup.StartInstance(t.Context(), admin, Target{Service: "repub-web"}); err != nil {
			t.Fatal(err)
		}
		// Republish rev 2 while gen 1's stop fails. Gen 2 MUST NOT launch.
		def.RunArgv = []string{"/bin/sh", "-c", "sleep 2"}
		if _, err := sup.Publish(t.Context(), def, 1); err != nil {
			t.Fatal(err)
		}
		if _, err := sup.StartInstance(t.Context(), admin, Target{Service: "repub-web"}); err == nil {
			t.Fatal("replacement launched despite failed predecessor stop")
		}
		if base.launches != 1 {
			t.Fatalf("launches = %d, want 1 (predecessor stop was unresolved)", base.launches)
		}
	})

	t.Run("daemon recovery finishes persisted stop intent", func(t *testing.T) {
		addr := freeLoopbackAddr(t)
		launcher := &managedHTTPLauncher{addr: addr}
		store := newMemStore()
		sup, err := NewSupervisor(Config{Store: store, Launcher: launcher, HostDefaultCwd: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		if err := sup.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		admin := Caller{Account: "local", Admin: true}
		def := Definition{
			Name: "stop-recover", URL: "http://" + addr + "/mcp",
			RunArgv: []string{"/bin/sh", "-c", "sleep 1"}, InstancePolicy: PolicyShared,
		}
		saved, err := sup.Publish(t.Context(), def, 0)
		if err != nil {
			t.Fatal(err)
		}
		view, err := sup.StartInstance(t.Context(), admin, Target{Service: "stop-recover"})
		if err != nil {
			t.Fatal(err)
		}
		// Simulate crash right after persisting intentStop and before Stop() is
		// delivered: record intentStop directly in store while worker stays live.
		sup.mu.Lock()
		inst := sup.instances[view.ID]
		rec := inst.rec
		sup.mu.Unlock()
		rec.Intent = intentStop
		rec.State = StateStopped
		rec.Reason = "stopped on request"
		row, err := instanceRow(rec)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.PutMCPInstance(t.Context(), row); err != nil {
			t.Fatal(err)
		}
		sup.Close()

		// Worker is still live before recovery.
		key := LaunchKey{Service: saved.ID, Instance: rec.ID, Generation: rec.Generation}
		if _, status, _, _ := launcher.Locate(t.Context(), key); status != WorkerLive {
			t.Fatalf("precondition: worker status = %v, want live", status)
		}

		// Recovery on new supervisor MUST finish stopping that worker.
		next, err := NewSupervisor(Config{Store: store, Launcher: launcher, HostDefaultCwd: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		if err := next.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(next.Close)
		eventually(t, func() bool {
			_, status, _, _ := launcher.Locate(t.Context(), key)
			return status == WorkerEnded
		})
	})
}

type stopFailLauncher struct {
	*managedHTTPLauncher
	failStop *atomic.Bool
}

func (l *stopFailLauncher) Stop(ctx context.Context, ref WorkerRef) error {
	if l.failStop.Load() {
		return errors.New("injected stop failure")
	}
	return l.managedHTTPLauncher.Stop(ctx, ref)
}

// TestDisableRejectsQueuedAcquireOnBothTransports covers review finding 3:
// a request that passed pick before SetDisabled(true) must not open or reuse
// an upstream when it enters acquire after SetDisabled returns.
func TestDisableRejectsQueuedAcquireOnBothTransports(t *testing.T) {
	admin := Caller{Account: "local", Admin: true}
	t.Run("external_http", func(t *testing.T) {
		f := newSupervisorFixture(t)
		var hits atomic.Int32
		endpoint := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			managedHTTPHandler(1).ServeHTTP(w, r)
		}))
		f.publish(t, Definition{Name: "ext-race", Argv: []string{}, URL: endpoint, InstancePolicy: PolicyShared})

		inst, _, err := f.sup.pick(t.Context(), caller("a"), Target{Service: "ext-race"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.sup.SetDisabled(t.Context(), "ext-race", true); err != nil {
			t.Fatal(err)
		}
		if _, err := f.sup.acquire(t.Context(), inst); Classify(err).Code != codeUnavailable {
			t.Fatalf("acquire after disable succeeded or wrong code: %v", err)
		}
		if hits.Load() != 0 {
			t.Fatalf("external HTTP upstream received %d requests after disable", hits.Load())
		}
	})

	t.Run("stdio", func(t *testing.T) {
		f := newSupervisorFixture(t)
		f.publish(t, Definition{Name: "stdio-race", InstancePolicy: PolicyShared})

		inst, _, err := f.sup.pick(t.Context(), caller("a"), Target{Service: "stdio-race"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.sup.SetDisabled(t.Context(), "stdio-race", true); err != nil {
			t.Fatal(err)
		}
		if _, err := f.sup.acquire(t.Context(), inst); Classify(err).Code != codeUnavailable {
			t.Fatalf("stdio acquire after disable succeeded or wrong code: %v", err)
		}
		if f.launcher.launches != 0 {
			t.Fatalf("stdio launcher spawned %d workers after disable", f.launcher.launches)
		}
		_ = admin
	})
}

// TestWarmProbeDoesNotCancelHeldCallOrRaceForwarding covers review finding 4.
func TestWarmProbeDoesNotCancelHeldCallOrRaceForwarding(t *testing.T) {
	callStarted := make(chan struct{})
	releaseCall := make(chan struct{})
	var failDiscover atomic.Bool
	var discoverVersion atomic.Int64
	discoverVersion.Store(1)

	endpoint := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case mcpwire.MethodDiscover:
			if failDiscover.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			v := discoverVersion.Load()
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"resultType":"complete","supportedVersions":["2026-07-28"],"capabilities":{"tools":{}},"ttlMs":60000,"cacheScope":"private","_meta":{"io.modelcontextprotocol/serverInfo":{"name":"warm-srv","version":"%d"}}}}`, req.ID, v)
		case mcpwire.MethodListTools:
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"resultType":"complete","tools":[{"name":"hold","inputSchema":{"type":"object"}}],"ttlMs":60000,"cacheScope":"private"}}`, req.ID)
		case mcpwire.MethodCallTool:
			select {
			case <-callStarted:
			default:
				close(callStarted)
			}
			select {
			case <-releaseCall:
			case <-r.Context().Done():
				return
			}
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"resultType":"complete","content":[{"type":"text","text":"held-ok"}]}}`, req.ID)
		}
	}))

	f := newSupervisorFixture(t)
	f.publish(t, Definition{Name: "warm-ext", Argv: []string{}, URL: endpoint, InstancePolicy: PolicyShared})
	admin := Caller{Account: "local", Admin: true}
	if view, err := f.sup.Probe(t.Context(), admin, Target{Service: "warm-ext"}); err != nil || view.State != StateReady {
		t.Fatalf("initial probe: %+v %v", view, err)
	}

	// Hold a tools/call open while a warm probe gets 503 AND another warm probe
	// is canceled. The held call MUST NOT be canceled.
	callErr := make(chan error, 1)
	out := &collected{}
	go func() {
		callErr <- f.sup.Forward(t.Context(), caller("a"), Target{Service: "warm-ext"}, callRequest(t, `"held"`, toolParams("hold", `{}`, "")), out.emit)
	}()
	<-callStarted

	failDiscover.Store(true)
	if _, err := f.sup.Probe(t.Context(), admin, Target{Service: "warm-ext"}); err == nil {
		t.Fatal("warm probe against 503 unexpectedly succeeded")
	}

	// Canceled probe while call is still held.
	canceledCtx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _ = f.sup.Probe(canceledCtx, admin, Target{Service: "warm-ext"})

	// Release the held call: it must succeed with "held-ok".
	close(releaseCall)
	if err := <-callErr; err != nil {
		t.Fatalf("held call interrupted by failed/canceled warm probe: %v", err)
	}
	if !strings.Contains(out.last(), "held-ok") {
		t.Fatalf("held call result = %s", out.last())
	}

	// Successful concurrent probes and forwards must not race under -race.
	failDiscover.Store(false)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(2)
		go func(n int) {
			defer wg.Done()
			discoverVersion.Store(int64(n + 2))
			_, _ = f.sup.Probe(t.Context(), admin, Target{Service: "warm-ext"})
		}(i)
		go func(n int) {
			defer wg.Done()
			c := &collected{}
			_ = f.sup.Forward(t.Context(), caller("a"), Target{Service: "warm-ext"}, callRequest(t, fmt.Sprintf(`"r%d"`, n), toolParams("hold", `{}`, "")), c.emit)
		}(i)
	}
	wg.Wait()
}
