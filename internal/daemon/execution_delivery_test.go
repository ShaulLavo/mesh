package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session/execution"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/worker"
)

func TestQueuedExecutionReplyUsesLatchedFullGrant(t *testing.T) {
	t.Run("receipt", func(t *testing.T) { testQueuedExecutionGrant(t, false) })
	t.Run("partial-error", func(t *testing.T) { testQueuedExecutionGrant(t, true) })
}

func testQueuedExecutionGrant(t *testing.T, partial bool) {
	t.Helper()
	state := compactSocketTempDir(t)
	host, _, err := identity.LoadOrCreate(state)
	if err != nil {
		t.Fatal(err)
	}
	device, grantCtx := bindExecutionFixtureGrant(t, state, host.ID)
	l := mustLifecycle(t, lifecycleConfig{Catalog: &lifecycleTestCatalog{}, Connector: failingLifecycleConnector(),
		Host: storage.Host{ID: storage.HostID(host.ID), MeshIdentity: host.ID}, SessionsDir: filepath.Join(state, "s")})
	t.Cleanup(l.closeExecution)
	key := prepareQueuedExecution(t, l, host.ID, device, state, partial)

	conn := newRelayTestConn()
	relay := newClientRelay(conn, nil, defaultWorkerOperationTimeout)
	t.Cleanup(func() { _ = relay.Close() })
	entered, release := conn.blockWrites()
	defer release()
	prior := protocol.Frame{Kind: protocol.KindControl, Payload: []byte("previous reply")}
	if err := relay.enqueueResponse(prior); err != nil {
		t.Fatal(err)
	}
	waitRelaySignal(t, entered, "first reply held at writer")
	s := &clientServer{lifecycle: l}
	if err := s.replyExecution(grantCtx, relay, nil, protocol.Control{Type: protocol.TypeExecutionRequest, RequestID: "queued", RequestKey: key.Opaque}); err != nil {
		t.Fatal(err)
	}
	assertQueuedReceipt(t, relay, partial)
	if err := identity.RevokeDevice(t.Context(), state, device); err != nil {
		t.Fatal(err)
	}
	if err := identity.ApproveDevice(t.Context(), state, device); err != nil {
		t.Fatal(err)
	}
	release()
	conn.waitClosed(t)
	if sent := conn.nextWrite(t); string(sent.Payload) != string(prior.Payload) {
		t.Fatalf("unexpected first write: %q", sent.Payload)
	}
	select {
	case leaked := <-conn.writes:
		t.Fatalf("revoked grant received a queued execution reply: %s", leaked.Payload)
	default:
	}
}

func assertQueuedReceipt(t *testing.T, relay *clientRelay, partial bool) {
	t.Helper()
	var queued relayOutput
	select {
	case queued = <-relay.output:
	default:
		t.Fatal("fixture has no queued execution reply")
	}
	reply, err := protocol.DecodeControl(queued.frame.Payload)
	if err != nil || reply.ExecutionReceipt == nil || (reply.Type == protocol.TypeError) != partial {
		t.Fatalf("fixture did not queue the expected receipt: %+v %v", reply, err)
	}
	relay.output <- queued
}

func bindExecutionFixtureGrant(t *testing.T, state, host string) (string, context.Context) {
	t.Helper()
	actor, key, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := identity.ApproveDevice(t.Context(), state, actor.ID); err != nil {
		t.Fatal(err)
	}
	auth, err := controlAuthentication(state)
	if err != nil {
		t.Fatal(err)
	}
	grant := make(chan context.Context, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = transport.ServeWithOptions(w, r, transport.ServeOptions{Auth: auth}, func(ctx context.Context, conn transport.Conn) error {
			// Exercise the grant at delivery even when socket closure has not
			// propagated through the authorization watcher.
			grant <- context.WithoutCancel(ctx)
			_, _ = conn.ReadFrame()
			return nil
		})
	}))
	t.Cleanup(server.Close)
	conn, err := transport.DialOnce(t.Context(), server.URL, transport.DialOptions{Auth: &transport.Authentication{Key: key, ExpectedIdentity: host}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	select {
	case granted := <-grant:
		return actor.ID, granted
	case <-time.After(10 * time.Second):
		t.Fatal("fixture full-device grant was not bound")
		return "", nil
	}
}

func prepareQueuedExecution(t *testing.T, l *lifecycle, host, device, cwd string, partial bool) execution.Key {
	t.Helper()
	j, err := l.executionJournal()
	if err != nil {
		t.Fatal(err)
	}
	key := execution.Key{Host: host, Class: "device", Principal: device, Opaque: "queued-job"}
	intent, err := execution.Normalize(execution.Intent{Kind: execution.KindPipe, Cwd: cwd, Command: []string{"/bin/true"}})
	if err != nil {
		t.Fatal(err)
	}
	r, created, _, err := l.reserveExecution(t.Context(), j, key, intent)
	if err != nil || !created {
		t.Fatalf("reserve reply fixture: %v", err)
	}
	if r.Binding.Reservation.BootID != worker.BootID() {
		t.Fatal("unexpected boot in receipt fixture")
	}
	// The reservation is already dispatched, which a lookup reports as a
	// partial receipt with an unknown outcome.
	if r.Phase != "dispatched" {
		t.Fatalf("reserve reply fixture phase = %s", r.Phase)
	}
	if partial {
		return key
	}
	if _, err := j.NotStarted(t.Context(), r.Binding, "fixture"); err != nil {
		t.Fatal(err)
	}
	return key
}
