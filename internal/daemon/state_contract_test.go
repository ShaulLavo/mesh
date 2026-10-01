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

	"github.com/shaul/mesh/internal/hostmetrics"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/transport"
)

func TestWatchConcurrentSnapshotCommitHasNoLostChange(t *testing.T) {
	for range 100 {
		broker := newStateBroker(2, time.Now)
		broker.observeSessions(nil)
		var wg sync.WaitGroup
		wg.Go(func() {
			broker.sessionsChanged(SessionDiff{Added: []storage.Session{{ID: "one", Command: []string{"shell"}}}})
		})
		sub, snapshot, err := broker.subscribe(protocol.StateWatch{Topics: []string{protocol.TopicSessions}})
		if err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		rows := len(snapshot.Sessions)
		for _, message := range broker.take(sub) {
			if message.StateEvent != nil && message.StateEvent.Payload.Session != nil {
				rows++
			}
		}
		if rows != 1 {
			t.Fatalf("snapshot/event handoff delivered %d rows", rows)
		}
		broker.unsubscribe(sub)
	}
}

func TestWatchStalledUnixWriterReleasesSubscriberAndReader(t *testing.T) {
	rawClient, rawServer := net.Pipe()
	client, err := transport.NewStreamConn(rawClient)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	peer, err := transport.NewStreamConn(rawServer)
	if err != nil {
		t.Fatal(err)
	}
	broker := newStateBroker(1, time.Now)
	server := &clientServer{state: broker, metrics: hostmetrics.New()}
	done := make(chan error, 1)
	started := time.Now()
	go func() {
		done <- server.serveState(t.Context(), peer, protocol.Control{Type: protocol.TypeStateWatch, RequestID: "stall", Watch: &protocol.StateWatch{Topics: []string{protocol.TopicSessions}}})
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stalled write succeeded")
		}
	case <-time.After(7 * time.Second):
		_ = peer.Close()
		t.Fatal("stalled writer or reader retained its reservation")
	}
	if time.Since(started) < 4*time.Second {
		t.Fatal("write did not exercise the transport deadline")
	}
	sub, _, err := broker.subscribe(protocol.StateWatch{Topics: []string{protocol.TopicServices}})
	if err != nil {
		t.Fatal("subscriber leaked", err)
	}
	broker.unsubscribe(sub)
}

type watchFailureStore struct {
	*catalogStoreStub
	fail bool
}

func (s *watchFailureStore) ApplyHostChanges(ctx context.Context, id storage.HostID, changes storage.HostChanges) error {
	if s.fail {
		return errors.New("fixture commit failed")
	}
	return s.catalogStoreStub.ApplyHostChanges(ctx, id, changes)
}
func TestWatchCatalogFailureRetainsRowsAndRecoveryClearsImmediately(t *testing.T) {
	now := time.Now()
	root := t.TempDir()
	meta := catalogTestMeta("7K3D", "running", "boot-a")
	writeCatalogMeta(t, root, meta.ID, meta)
	broker := newStateBroker(1, func() time.Time { return now })
	store := &watchFailureStore{catalogStoreStub: &catalogStoreStub{}}
	catalog, err := NewCatalog(CatalogConfig{SessionsDir: root, Host: catalogTestHost(now), Store: store, Probe: probeFunc(func(context.Context, string) error { return nil }), BootID: func() string { return "boot-a" }, Now: func() time.Time { return now }, OnChange: broker.sessionsChanged, OnObservation: broker.observeSessions})
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	sub, initial, err := broker.subscribe(protocol.StateWatch{Topics: []string{protocol.TopicSessions}})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.unsubscribe(sub)
	if len(initial.Sessions) != 1 {
		t.Fatal(initial)
	}
	now = now.Add(time.Minute)
	store.fail = true
	if err := catalog.Reconcile(t.Context()); err == nil {
		t.Fatal("injected commit succeeded")
	}
	if !broker.current(sub).Sections[protocol.TopicSessions].Failing {
		t.Fatal("failed reconcile remained fresh")
	}
	store.fail = false
	if err := catalog.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	messages := broker.take(sub)
	if len(messages) != 1 || messages[0].StateCurrent.Sections[protocol.TopicSessions].Failing {
		t.Fatalf("recovery waited for heartbeat: %+v", messages)
	}
	if len(broker.snapshotLockedForTest(sub).Sessions) != 1 {
		t.Fatal("retained catalog lost rows")
	}
}
func (b *stateBroker) snapshotLockedForTest(sub *stateSubscriber) *protocol.StateSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.snapshotLocked(sub)
}

func TestWatchLeanProjectionRejectsSupersededAndRemovedRows(t *testing.T) {
	broker := newStateBroker(1, time.Now)
	original := storage.Session{ID: "one", Command: []string{"old"}}
	broker.sessionsChanged(SessionDiff{Added: []storage.Session{original}})
	if !broker.sessions["one"].RecoveryPending {
		t.Fatal("base row claimed metadata was complete")
	}
	broker.sessionsChanged(SessionDiff{Changed: []storage.Session{{ID: "one", Command: []string{"new"}}}})
	broker.commitProjection(original, protocol.SessionInfo{ID: "one", Command: []string{"stale"}})
	if broker.sessions["one"].Command[0] != "new" {
		t.Fatal("stale projection replaced committed row")
	}
	broker.sessionsChanged(SessionDiff{Removed: []storage.SessionID{"one"}})
	broker.commitProjection(original, protocol.SessionInfo{ID: "one"})
	if _, exists := broker.sessions["one"]; exists {
		t.Fatal("projection restored removed row")
	}
	if len(broker.dirty) != 0 {
		t.Fatal("removed projection retained work")
	}
}

func TestWatchProjectionCompletesPendingSnapshot(t *testing.T) {
	broker := newStateBroker(1, time.Now)
	row := storage.Session{ID: "one", Command: []string{"shell"}}
	broker.sessionsChanged(SessionDiff{Added: []storage.Session{row}})
	sub, snapshot, err := broker.subscribe(protocol.StateWatch{Topics: []string{protocol.TopicSessions}})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.unsubscribe(sub)
	if !snapshot.Sessions[0].RecoveryPending {
		t.Fatal("initial base row claimed complete recognition")
	}
	broker.commitProjection(row, sessionInfo(row))
	messages := broker.take(sub)
	if len(messages) != 1 || messages[0].StateEvent.Payload.Session.RecoveryPending {
		t.Fatalf("projection did not finish pending row: %+v", messages)
	}
}

func TestWatchSubscriberCapIndependentAcrossActualTransports(t *testing.T) {
	for _, firstNetwork := range []string{"Unix", "Tailnet"} {
		t.Run(firstNetwork, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), runtimeTestTimeout)
			defer cancel()
			lifecycle := mustServerTestLifecycle(t, &serverTestCatalog{}, failingServerTestConnector())
			server, err := newClientServer(lifecycle, failingServerTestConnector(), disabledEdgeController{}, noServiceControl{}, disabledCertificateController{})
			if err != nil {
				t.Fatal(err)
			}
			server.state = newStateBroker(1, time.Now)
			server.metrics = hostmetrics.New()
			listener, port := newTCPListener(t, "127.0.0.1:0")
			cfg := ListenerConfig{StateDir: compactSocketTempDir(t), UnixConnectionLimit: 3, TailnetConnectionLimit: 3, TailnetAddrs: []string{"127.0.0.1"}, TailnetPort: port, WebSocketPath: "/mesh"}
			done := runRuntime(t, ctx, cfg, server.Handle, listener)
			dial := func(network string) transport.Conn {
				if network == "Unix" {
					return dialUnixRuntime(t, filepath.Join(cfg.StateDir, daemonSocketName))
				}
				conn, err := transport.DialOnce(ctx, fmt.Sprintf("ws://%s/mesh", listener.Addr()), transport.DialOptions{})
				if err != nil {
					t.Fatal(err)
				}
				return conn
			}
			first := dial(firstNetwork)
			defer func() { _ = first.Close() }()
			response := watchContractRequest(t, first, protocol.Control{Type: protocol.TypeStateWatch, RequestID: "first", Watch: &protocol.StateWatch{Topics: []string{protocol.TopicSessions}}})
			if response.Type != protocol.TypeStateSnapshot {
				t.Fatal(response)
			}
			otherNetwork := "Tailnet"
			if firstNetwork == "Tailnet" {
				otherNetwork = "Unix"
			}
			refused := dial(otherNetwork)
			response = watchContractRequest(t, refused, protocol.Control{Type: protocol.TypeStateWatch, RequestID: "refused", Watch: &protocol.StateWatch{Topics: []string{protocol.TopicSessions}}})
			_ = refused.Close()
			if response.ErrorCode != protocol.ErrorCodeWatchLimit {
				t.Fatal("subscriber cap did not span transports", response)
			}
			for _, network := range []string{"Unix", "Tailnet"} {
				ordinary := dial(network)
				response = watchContractRequest(t, ordinary, protocol.Control{Type: protocol.TypeHostInfo, RequestID: "ordinary"})
				_ = ordinary.Close()
				if response.Type != protocol.TypeHostInfoResult {
					t.Fatal("watch cap blocked ordinary control", response)
				}
			}
			cancel()
			if err := waitRuntime(t, done); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func watchContractRequest(t *testing.T, conn transport.Conn, request protocol.Control) protocol.Control {
	t.Helper()
	if err := conn.WriteFrame(serverControlFrame(t, request)); err != nil {
		t.Fatal(err)
	}
	frame, err := conn.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	return decodeServerControl(t, frame)
}

type watchMetricsCollector struct{ reads int }

func (c *watchMetricsCollector) CPU(context.Context) (hostmetrics.Counters, error) {
	c.reads++
	return hostmetrics.Counters{User: float64(c.reads * 10), Idle: float64(c.reads * 10)}, nil
}
func (*watchMetricsCollector) Memory(context.Context) (hostmetrics.Memory, error) {
	return hostmetrics.Memory{TotalBytes: 100, AvailableBytes: 40, Estimate: "fixture available pages"}, nil
}
func (*watchMetricsCollector) Temperature(context.Context) (hostmetrics.Temperature, error) {
	return hostmetrics.Temperature{}, hostmetrics.ErrUnsupported
}
func (*watchMetricsCollector) Uptime(context.Context) (uint64, error) { return 100, nil }

func TestHostMetricsOneShotWarmsAcrossDemandStopStart(t *testing.T) {
	now := time.Now()
	collector := &watchMetricsCollector{}
	sampler := hostmetrics.NewWithCollector(collector, func() time.Time { return now })
	server := &clientServer{metrics: sampler}
	read := func() hostmetrics.Snapshot {
		t.Helper()
		frame, err := server.readMetrics(t.Context(), protocol.Control{Type: protocol.TypeHostMetrics, RequestID: "one-shot"})
		if err != nil {
			t.Fatal(err)
		}
		response := decodeServerControl(t, frame)
		if response.Type != protocol.TypeHostMetricsResult || response.RequestID != "one-shot" || response.Metrics == nil {
			t.Fatal(response)
		}
		if sampler.Interval() != 0 {
			t.Fatal("one-shot retained sampling demand", sampler.Interval())
		}
		return *response.Metrics
	}
	first := read()
	if first.CPU.Availability != hostmetrics.Unavailable || collector.reads != 1 {
		t.Fatal(first, collector.reads)
	}
	now = now.Add(hostmetrics.MinimumInterval)
	warm := read()
	if warm.CPU.Availability != hostmetrics.Available || warm.CPU.Value != 50 || collector.reads != 2 {
		t.Fatal(warm, collector.reads)
	}
	release := sampler.Demand(hostmetrics.MaximumInterval)
	release()
	now = now.Add(hostmetrics.MinimumInterval)
	resumed := read()
	if resumed.CPU.Failing || resumed.CPU.Segment != warm.CPU.Segment {
		t.Fatal("demand release reset logical collector", resumed.CPU)
	}
	now = now.Add(hostmetrics.BaselineGap + time.Second)
	gap := read()
	if !gap.CPU.Failing || gap.CPU.Sample != resumed.CPU.Sample || gap.CPU.AgeMillis <= hostmetrics.BaselineGap.Milliseconds() {
		t.Fatal("long gap lost independently aged last-good sample", gap.CPU)
	}
	now = now.Add(hostmetrics.MinimumInterval)
	restarted := read()
	if restarted.CPU.Failing || restarted.CPU.Segment == resumed.CPU.Segment {
		t.Fatal("long gap did not begin a new graph segment", restarted.CPU)
	}
}

func TestWatchRejectsTerminalUsedConnectionAndPreservesAttachment(t *testing.T) {
	client, worker := newServerTestConn(), newServerTestConn()
	lifecycle := mustServerTestLifecycle(t, &serverTestCatalog{}, failingServerTestConnector())
	server, err := newClientServer(lifecycle, newServerTestConnector(worker), disabledEdgeController{}, noServiceControl{}, disabledCertificateController{})
	if err != nil {
		t.Fatal(err)
	}
	server.state = newStateBroker(1, time.Now)
	server.metrics = hostmetrics.New()
	done := make(chan error, 1)
	go func() { done <- server.Handle(t.Context(), client) }()
	defer func() { _ = client.Close(); <-done }()
	attached := serverControlFrame(t, protocol.Control{Type: protocol.TypeAttached, SessionID: "7K3D"})
	worker.pushRead(attached)
	client.pushRead(serverControlFrame(t, protocol.Control{Type: protocol.TypeAttach, SessionID: "7K3D"}))
	_ = worker.nextWrite(t)
	assertServerFrame(t, client.nextWrite(t), attached)
	client.pushRead(serverControlFrame(t, protocol.Control{Type: protocol.TypeStateWatch, RequestID: "watch-after-attach", Watch: &protocol.StateWatch{Topics: []string{protocol.TopicSessions, protocol.TopicMetrics}}}))
	refused := decodeServerControl(t, client.nextWrite(t))
	if refused.Type != protocol.TypeError || refused.RequestID != "watch-after-attach" {
		t.Fatalf("terminal connection became a watch: %+v", refused)
	}
	if server.metrics.Interval() != 0 {
		t.Fatal("rejected terminal watch acquired sampling demand")
	}
	id := mustServerSessionID(t, "7K3D")
	input := protocol.Frame{Kind: protocol.KindInput, Session: id, Payload: []byte("still attached")}
	client.pushRead(input)
	assertServerFrame(t, worker.nextWrite(t), input)
	output := protocol.Frame{Kind: protocol.KindData, Session: id, Payload: []byte("still running")}
	worker.pushRead(output)
	assertServerFrame(t, client.nextWrite(t), output)
	sub, _, err := server.state.subscribe(protocol.StateWatch{Topics: []string{protocol.TopicSessions}})
	if err != nil {
		t.Fatal("rejected terminal watch consumed subscriber reservation", err)
	}
	server.state.unsubscribe(sub)
}

func TestWatchRejectsTerminalModeChangeAndKeepsStream(t *testing.T) {
	client := newServerTestConn()
	lifecycle := mustServerTestLifecycle(t, &serverTestCatalog{}, failingServerTestConnector())
	server, err := newClientServer(lifecycle, failingServerTestConnector(), disabledEdgeController{}, noServiceControl{}, disabledCertificateController{})
	if err != nil {
		t.Fatal(err)
	}
	server.state = newStateBroker(1, time.Now)
	server.metrics = hostmetrics.New()
	done := make(chan error, 1)
	go func() { done <- server.Handle(t.Context(), client) }()
	defer func() { _ = client.Close(); <-done }()
	client.pushRead(serverControlFrame(t, protocol.Control{Type: protocol.TypeStateWatch, RequestID: "watch-first", Watch: &protocol.StateWatch{Topics: []string{protocol.TopicSessions, protocol.TopicMetrics}}}))
	initial := decodeServerControl(t, client.nextWrite(t))
	if initial.Type != protocol.TypeStateSnapshot {
		t.Fatal(initial)
	}
	for _, kind := range []string{protocol.TypeAttach, protocol.TypeTunnelRecover} {
		client.pushRead(serverControlFrame(t, protocol.Control{Type: kind, SessionID: "7K3D", RequestID: "incompatible"}))
		refused := decodeServerControl(t, client.nextWrite(t))
		if refused.Type != protocol.TypeError || refused.RequestID != "incompatible" {
			t.Fatal("mode change did not receive correlated refusal", refused)
		}
	}
	if server.metrics.Interval() != hostmetrics.MinimumInterval {
		t.Fatal("mode refusal released active watch demand")
	}
	id := mustServerSessionID(t, "7K3D")
	client.pushRead(protocol.Frame{Kind: protocol.KindInput, Session: id, Payload: []byte("terminal input")})
	refused := decodeServerControl(t, client.nextWrite(t))
	if refused.Type != protocol.TypeError || refused.SessionID != "7K3D" {
		t.Fatal("watch accepted terminal bytes", refused)
	}
	client.pushRead(serverControlFrame(t, protocol.Control{Type: protocol.TypeHostInfo, RequestID: "read-only"}))
	info := decodeServerControl(t, client.nextWrite(t))
	if info.Type != protocol.TypeHostInfoResult || info.RequestID != "read-only" {
		t.Fatal("watch lost ordinary read control", info)
	}
	server.state.sessionsChanged(SessionDiff{Added: []storage.Session{{ID: "7K3D", Command: []string{"shell"}}}})
	changed := decodeServerControl(t, client.nextWrite(t))
	if changed.Type != protocol.TypeStateEvent || changed.StateEvent.Seq != initial.StateSnapshot.Seq+1 {
		t.Fatal("watch mode change stopped state stream", changed)
	}
}
