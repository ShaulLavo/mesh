package daemon

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/hostmetrics"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/transport"
)

func TestLargeWatchResyncUsesSingleReaderAndReleasesSubscriber(t *testing.T) {
	broker := newStateBroker(1, time.Now)
	for i := range 168 {
		id := fmt.Sprintf("%04d", i)
		broker.sessions[id] = protocol.SessionInfo{ID: id, Command: []string{strings.Repeat("x", 32<<10)}}
	}
	server := &clientServer{state: broker, metrics: hostmetrics.New()}
	client, peer := catalogPipe(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.serveState(ctx, peer, protocol.Control{Type: protocol.TypeStateWatch, RequestID: "large", Watch: &protocol.StateWatch{Topics: []string{protocol.TopicSessions}}})
	}()
	t.Cleanup(func() { _ = client.Close(); _ = peer.Close(); <-done })
	first, err := client.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if !decodeServerControl(t, first).More {
		t.Fatal("initial snapshot did not cross a frame boundary")
	}
	broker.mu.Lock()
	for sub := range broker.subscribers {
		fillCatalogResync(broker, sub)
	}
	broker.mu.Unlock()
	read := func() (protocol.Frame, error) {
		if first.Payload != nil {
			frame := first
			first.Payload = nil
			return frame, nil
		}
		return client.ReadFrame()
	}
	initial, err := protocol.ReadControl(read, client.WriteFrame)
	if err != nil || initial.StateSnapshot == nil || len(initial.StateSnapshot.Sessions) != 168 {
		t.Fatal("initial snapshot lost rows", err)
	}
	resync, err := protocol.ReadControl(client.ReadFrame, client.WriteFrame)
	if err != nil || resync.Type != protocol.TypeStateResync {
		t.Fatal("queue overflow did not request resync", err)
	}
	next, err := protocol.ReadControl(client.ReadFrame, client.WriteFrame)
	if err != nil || next.StateSnapshot == nil || len(next.StateSnapshot.Sessions) != 168 {
		t.Fatal("large resync snapshot lost rows", err)
	}
	_ = client.Close()
	select {
	case <-done:
	case <-time.After(runtimeTestTimeout):
		t.Fatal("resync reader retained subscriber after disconnect")
	}
	sub, _, err := broker.subscribe(protocol.StateWatch{Topics: []string{protocol.TopicServices}})
	if err != nil {
		t.Fatal("large resync leaked its subscriber reservation", err)
	}
	broker.unsubscribe(sub)
}

func fillCatalogResync(broker *stateBroker, sub *stateSubscriber) {
	for i := range stateQueueCapacity + 1 {
		broker.enqueueLocked(sub, fmt.Sprint(i), protocol.StateEvent{Kind: "session.removed", Payload: protocol.StatePayload{SessionID: "absent"}})
	}
}

func catalogPipe(t *testing.T) (transport.Conn, transport.Conn) {
	t.Helper()
	a, b := net.Pipe()
	client, err := transport.NewStreamConn(a)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := transport.NewStreamConn(b)
	if err != nil {
		t.Fatal(err)
	}
	return client, peer
}

func TestOversizedCatalogEntryReturnsStructuredError(t *testing.T) {
	catalog := &lifecycleTestCatalog{sessions: []storage.Session{{ID: "HUGE", HostID: "host-a", Command: []string{strings.Repeat("x", protocol.MaxPayload)}, State: storage.StateExited}}}
	lifecycle := mustServerTestLifecycle(t, catalog, failingServerTestConnector())
	server, err := newClientServer(lifecycle, failingServerTestConnector(), noServiceControl{}, disabledCertificateController{})
	if err != nil {
		t.Fatal(err)
	}
	client, peer := catalogPipe(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Handle(ctx, peer) }()
	t.Cleanup(func() { _ = client.Close(); _ = peer.Close(); <-done })
	if err := client.WriteFrame(serverControlFrame(t, protocol.Control{Type: protocol.TypeList, RequestID: "huge", Lean: true})); err != nil {
		t.Fatal(err)
	}
	response, err := protocol.ReadControl(client.ReadFrame, client.WriteFrame)
	if err != nil || response.Type != protocol.TypeError || response.RequestID != "huge" || !strings.Contains(response.Message, "HUGE") || !strings.Contains(response.Message, "frame limit") {
		t.Fatal("oversized entry did not produce an actionable error", err, response.Type)
	}
}
