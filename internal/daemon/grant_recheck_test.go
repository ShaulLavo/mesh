package daemon

import (
	"errors"
	"testing"

	"github.com/shaul/mesh/internal/access"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	meshserve "github.com/shaul/mesh/internal/serve"
)

// A service mutation that waited behind the gate after admission runs only
// if its device grant is still current when it gets the gate.
func TestQueuedServiceMutationRechecksGrant(t *testing.T) {
	state := compactSocketTempDir(t)
	host, _, err := identity.LoadOrCreate(state)
	if err != nil {
		t.Fatal(err)
	}
	device, grantCtx := bindExecutionFixtureGrant(t, state, host.ID)
	store := &scopedServiceStore{rows: []meshserve.Service{{Name: "site", Kind: meshserve.Proxy, Target: "3000"}}}
	registry, err := meshserve.NewRegistry(nil)
	if err != nil {
		t.Fatal(err)
	}
	controller, err := newServiceController(t.Context(), t.TempDir(), store, registry)
	if err != nil {
		t.Fatal(err)
	}
	controller.gate <- struct{}{}
	done := make(chan error, 1)
	go func() {
		_, _, err := controller.HandleControl(grantCtx, protocol.Control{Type: protocol.TypeServiceLabel, RequestID: "label", ServiceName: "site", ServiceDisplayName: "Site"})
		done <- err
	}()
	if err := identity.RevokeDevice(t.Context(), state, device); err != nil {
		t.Fatal(err)
	}
	<-controller.gate
	if err := <-done; !errors.Is(err, access.ErrDenied) {
		t.Fatalf("queued label after revocation = %v, want access denied", err)
	}
	if store.writes.Load() != 0 {
		t.Fatal("revoked grant changed a service")
	}
}

// The reply to a state-changing control is delivered only while the grant
// that admitted it is current, as execution replies are.
func TestQueuedMutationReplyUsesLatchedGrant(t *testing.T) {
	state := compactSocketTempDir(t)
	host, _, err := identity.LoadOrCreate(state)
	if err != nil {
		t.Fatal(err)
	}
	device, grantCtx := bindExecutionFixtureGrant(t, state, host.ID)
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
	reply := protocol.Control{Type: protocol.TypeOK, RequestID: "start", ServiceName: "site"}
	frames, err := protocol.ControlFrames(reply)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeClientResponse(grantCtx, conn, relay, frames, reply, replyAdmission(grantCtx, protocol.TypeServiceStart)); err != nil {
		t.Fatal(err)
	}
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
		t.Fatalf("revoked grant received a queued service reply: %s", leaked.Payload)
	default:
	}
	if replyAdmission(grantCtx, protocol.TypeServiceList) != nil {
		t.Fatal("a read reply should not need the mutation admission")
	}
}
