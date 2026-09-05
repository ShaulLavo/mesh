package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

func TestClientRelayForwardsAttachmentFrames(t *testing.T) {
	client := newRelayTestConn()
	worker := newRelayTestConn()
	connector := newRelayTestConnector(connectResult{conn: worker})
	relay := newClientRelay(client, connector)
	t.Cleanup(func() { _ = relay.Close() })

	id := mustRelaySessionID(t, "7K3D")
	attach := controlFrame(t, protocol.Control{
		Type:      protocol.TypeAttach,
		SessionID: id.String(),
		Cols:      120,
		Rows:      40,
	})
	attached := controlFrame(t, protocol.Control{
		Type:      protocol.TypeAttached,
		SessionID: id.String(),
		Seq:       9,
	})
	worker.pushRead(attached)
	handled, err := relay.HandleFrame(context.Background(), attach)
	if err != nil || !handled {
		t.Fatalf("attach handled = %v, error = %v", handled, err)
	}
	assertRelayFrame(t, worker.nextWrite(t), attach)
	assertRelayFrame(t, client.nextWrite(t), attached)

	input := protocol.Frame{Kind: protocol.KindInput, Session: id, Payload: []byte("hello")}
	handled, err = relay.HandleFrame(context.Background(), input)
	if err != nil || !handled {
		t.Fatalf("input handled = %v, error = %v", handled, err)
	}
	assertRelayFrame(t, worker.nextWrite(t), input)

	resize := controlFrame(t, protocol.Control{
		Type:      protocol.TypeResize,
		SessionID: id.String(),
		Cols:      132,
		Rows:      43,
	})
	handled, err = relay.HandleFrame(context.Background(), resize)
	if err != nil || !handled {
		t.Fatalf("resize handled = %v, error = %v", handled, err)
	}
	assertRelayFrame(t, worker.nextWrite(t), resize)

	signal := controlFrame(t, protocol.Control{
		Type:      protocol.TypeSignal,
		SessionID: id.String(),
		Signal:    "int",
	})
	handled, err = relay.HandleFrame(context.Background(), signal)
	if err != nil || handled {
		t.Fatalf("signal handled = %v, error = %v, want lifecycle dispatcher", handled, err)
	}

	data := protocol.Frame{Kind: protocol.KindData, Session: id, Seq: 9, Payload: []byte("out")}
	snapshot := protocol.Frame{Kind: protocol.KindSnapshot, Session: id, Payload: []byte("screen")}
	worker.pushRead(data)
	worker.pushRead(snapshot)
	assertRelayFrame(t, client.nextWrite(t), data)
	assertRelayFrame(t, client.nextWrite(t), snapshot)

	detach := controlFrame(t, protocol.Control{Type: protocol.TypeDetach, SessionID: id.String()})
	handled, err = relay.HandleFrame(context.Background(), detach)
	if err != nil || !handled {
		t.Fatalf("detach handled = %v, error = %v", handled, err)
	}
	assertRelayFrame(t, worker.nextWrite(t), detach)
	worker.waitClosed(t)
}

func TestClientRelayBlockedSessionDoesNotBlockAnother(t *testing.T) {
	client := newRelayTestConn()
	workerA := newRelayTestConn()
	workerB := newRelayTestConn()
	connector := newRelayTestConnector(
		connectResult{conn: workerA},
		connectResult{conn: workerB},
	)
	relay := newClientRelay(client, connector)
	t.Cleanup(func() { _ = relay.Close() })

	idA := mustRelaySessionID(t, "AAAA")
	idB := mustRelaySessionID(t, "BBBB")
	attachRelaySession(t, relay, client, workerA, idA)
	attachRelaySession(t, relay, client, workerB, idB)

	entered, release := workerA.blockWrites()
	inputA := protocol.Frame{Kind: protocol.KindInput, Session: idA, Payload: []byte("a")}
	if handled, err := relay.HandleFrame(context.Background(), inputA); err != nil || !handled {
		t.Fatalf("blocked input handled = %v, error = %v", handled, err)
	}
	waitRelaySignal(t, entered, "blocked session writer")
	inputA.Payload[0] = 'x'

	inputB := protocol.Frame{Kind: protocol.KindInput, Session: idB, Payload: []byte("b")}
	if handled, err := relay.HandleFrame(context.Background(), inputB); err != nil || !handled {
		t.Fatalf("independent input handled = %v, error = %v", handled, err)
	}
	assertRelayFrame(t, workerB.nextWrite(t), inputB)

	release()
	assertRelayFrame(t, workerA.nextWrite(t), protocol.Frame{
		Kind:    protocol.KindInput,
		Session: idA,
		Payload: []byte("a"),
	})
}

func TestClientRelaySlowClientDoesNotStopWorkerReaders(t *testing.T) {
	client := newRelayTestConn()
	workerA := newRelayTestConn()
	workerB := newRelayTestConn()
	connector := newRelayTestConnector(
		connectResult{conn: workerA},
		connectResult{conn: workerB},
	)
	relay := newClientRelay(client, connector)
	t.Cleanup(func() { _ = relay.Close() })

	idA := mustRelaySessionID(t, "AAAA")
	idB := mustRelaySessionID(t, "BBBB")
	attachRelaySession(t, relay, client, workerA, idA)
	attachRelaySession(t, relay, client, workerB, idB)

	entered, release := client.blockWrites()
	defer release()
	workerA.pushRead(protocol.Frame{Kind: protocol.KindData, Session: idA, Payload: []byte("blocked")})
	waitRelaySignal(t, entered, "blocked client writer")

	first := workerB.pushReadObserved(protocol.Frame{Kind: protocol.KindData, Session: idB, Payload: []byte("one")})
	waitRelaySignal(t, first, "first independent worker frame")
	second := workerB.pushReadObserved(protocol.Frame{Kind: protocol.KindData, Session: idB, Payload: []byte("two")})
	waitRelaySignal(t, second, "second independent worker frame")
}

func TestClientRelayOutputQueueIsBoundedAndReservesControls(t *testing.T) {
	lifetime, cancel := context.WithCancel(context.Background())
	defer cancel()
	relay := &clientRelay{
		lifetime: lifetime,
		output:   make(chan protocol.Frame, relayOutputQueueFrameLimit+relayOutputControlReserve),
	}

	payload := []byte{0}
	for i := range relayOutputQueueFrameLimit {
		payload[0] = byte(i)
		if err := relay.enqueueOutput(protocol.Frame{Kind: protocol.KindData, Payload: payload}); err != nil {
			t.Fatalf("queue data frame %d: %v", i, err)
		}
	}
	payload[0] = 0xff
	if err := relay.enqueueOutput(protocol.Frame{Kind: protocol.KindData, Payload: payload}); !errors.Is(err, errRelayOutputQueueFull) {
		t.Fatalf("data overflow error = %v, want %v", err, errRelayOutputQueueFull)
	}
	first := <-relay.output
	if len(first.Payload) != 1 || first.Payload[0] != 0 {
		t.Fatalf("queued data was not copied: %v", first.Payload)
	}
	relay.output <- first

	control := controlFrame(t, protocol.Control{Type: protocol.TypeExit, SessionID: "FULL"})
	for i := range relayOutputControlReserve {
		if err := relay.enqueueOutput(control); err != nil {
			t.Fatalf("queue reserved control %d: %v", i, err)
		}
	}
	if err := relay.enqueueOutput(control); !errors.Is(err, errRelayOutputQueueFull) {
		t.Fatalf("control overflow error = %v, want %v", err, errRelayOutputQueueFull)
	}
}

func TestClientRelayBoundsQueuedControlResponseBytes(t *testing.T) {
	lifetime, cancel := context.WithCancel(context.Background())
	defer cancel()
	relay := &clientRelay{lifetime: lifetime, output: make(chan protocol.Frame, 4)}
	large := protocol.Frame{Kind: protocol.KindControl, Payload: make([]byte, protocol.MaxPayload)}
	for range 2 {
		if err := relay.enqueueOutput(large); err != nil {
			t.Fatal(err)
		}
	}
	reserved := protocol.Frame{Kind: protocol.KindControl, Payload: make([]byte, relayOutputControlByteReserve)}
	if err := relay.enqueueOutput(reserved); err != nil {
		t.Fatal(err)
	}
	if err := relay.enqueueOutput(protocol.Frame{Kind: protocol.KindControl, Payload: []byte{1}}); !errors.Is(err, errRelayOutputQueueFull) {
		t.Fatalf("control bytes overflow = %v", err)
	}
	relay.releaseOutput(<-relay.output)
	if err := relay.enqueueOutput(large); err != nil {
		t.Fatalf("completed control response did not release byte budget: %v", err)
	}
}

func TestClientRelayFailedReplacementPreservesIncumbent(t *testing.T) {
	client := newRelayTestConn()
	incumbent := newRelayTestConn()
	connector := newRelayTestConnector(
		connectResult{conn: incumbent},
		connectResult{err: errors.New("dial failed")},
	)
	relay := newClientRelay(client, connector)
	t.Cleanup(func() { _ = relay.Close() })

	id := mustRelaySessionID(t, "KEEP")
	attachRelaySession(t, relay, client, incumbent, id)
	replacement := controlFrame(t, protocol.Control{Type: protocol.TypeAttach, SessionID: id.String()})
	handled, err := relay.HandleFrame(context.Background(), replacement)
	if !handled || err == nil {
		t.Fatalf("replacement handled = %v, error = %v, want candidate failure", handled, err)
	}
	select {
	case <-incumbent.closed:
		t.Fatal("failed replacement closed incumbent")
	default:
	}

	input := protocol.Frame{Kind: protocol.KindInput, Session: id, Payload: []byte("still here")}
	if handled, err := relay.HandleFrame(context.Background(), input); err != nil || !handled {
		t.Fatalf("incumbent input handled = %v, error = %v", handled, err)
	}
	assertRelayFrame(t, incumbent.nextWrite(t), input)
}

func TestClientRelayRejectedReplacementPreservesIncumbent(t *testing.T) {
	client := newRelayTestConn()
	incumbent := newRelayTestConn()
	candidate := newRelayTestConn()
	connector := newRelayTestConnector(
		connectResult{conn: incumbent},
		connectResult{conn: candidate},
	)
	relay := newClientRelay(client, connector)
	t.Cleanup(func() { _ = relay.Close() })

	id := mustRelaySessionID(t, "KEEP")
	attachRelaySession(t, relay, client, incumbent, id)
	replacement := controlFrame(t, protocol.Control{Type: protocol.TypeAttach, SessionID: id.String()})
	rejection := controlFrame(t, protocol.Control{
		Type:      protocol.TypeError,
		SessionID: id.String(),
		Message:   "retry attachment",
	})
	candidate.pushRead(rejection)
	if handled, err := relay.HandleFrame(context.Background(), replacement); err != nil || !handled {
		t.Fatalf("rejected replacement handled = %v, error = %v", handled, err)
	}
	assertRelayFrame(t, candidate.nextWrite(t), replacement)
	assertRelayFrame(t, client.nextWrite(t), rejection)
	candidate.waitClosed(t)
	assertRelayConnOpen(t, incumbent, "rejected replacement closed incumbent")

	input := protocol.Frame{Kind: protocol.KindInput, Session: id, Payload: []byte("still incumbent")}
	if handled, err := relay.HandleFrame(context.Background(), input); err != nil || !handled {
		t.Fatalf("incumbent input handled = %v, error = %v", handled, err)
	}
	assertRelayFrame(t, incumbent.nextWrite(t), input)
}

func TestClientRelayHandshakeEOFPreservesIncumbent(t *testing.T) {
	client := newRelayTestConn()
	incumbent := newRelayTestConn()
	candidate := newRelayTestConn()
	connector := newRelayTestConnector(
		connectResult{conn: incumbent},
		connectResult{conn: candidate},
	)
	relay := newClientRelay(client, connector)
	t.Cleanup(func() { _ = relay.Close() })

	id := mustRelaySessionID(t, "KEEP")
	attachRelaySession(t, relay, client, incumbent, id)
	candidate.pushReadError(io.EOF)
	replacement := controlFrame(t, protocol.Control{Type: protocol.TypeAttach, SessionID: id.String()})
	if handled, err := relay.HandleFrame(context.Background(), replacement); !handled || err == nil {
		t.Fatalf("EOF replacement handled = %v, error = %v", handled, err)
	}
	assertRelayFrame(t, candidate.nextWrite(t), replacement)
	candidate.waitClosed(t)
	assertRelayConnOpen(t, incumbent, "EOF replacement closed incumbent")
}

func TestClientRelayCanceledHandshakePreservesIncumbent(t *testing.T) {
	client := newRelayTestConn()
	incumbent := newRelayTestConn()
	candidate := newRelayTestConn()
	connector := newRelayTestConnector(
		connectResult{conn: incumbent},
		connectResult{conn: candidate},
	)
	relay := newClientRelay(client, connector)
	t.Cleanup(func() { _ = relay.Close() })

	id := mustRelaySessionID(t, "KEEP")
	attachRelaySession(t, relay, client, incumbent, id)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	replacement := controlFrame(t, protocol.Control{Type: protocol.TypeAttach, SessionID: id.String()})
	go func() {
		handled, err := relay.HandleFrame(ctx, replacement)
		if err == nil && !handled {
			err = errors.New("replacement was not handled")
		}
		result <- err
	}()
	assertRelayFrame(t, candidate.nextWrite(t), replacement)
	cancel()
	if err := waitRelayError(t, result, "canceled handshake"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled handshake error = %v, want context cancellation", err)
	}
	candidate.waitClosed(t)
	assertRelayConnOpen(t, incumbent, "canceled replacement closed incumbent")
}

func TestClientRelayCrossSessionHandshakePreservesIncumbent(t *testing.T) {
	client := newRelayTestConn()
	incumbent := newRelayTestConn()
	candidate := newRelayTestConn()
	connector := newRelayTestConnector(
		connectResult{conn: incumbent},
		connectResult{conn: candidate},
	)
	relay := newClientRelay(client, connector)
	t.Cleanup(func() { _ = relay.Close() })

	id := mustRelaySessionID(t, "KEEP")
	other := mustRelaySessionID(t, "EVIL")
	attachRelaySession(t, relay, client, incumbent, id)
	candidate.pushRead(controlFrame(t, protocol.Control{
		Type:      protocol.TypeAttached,
		SessionID: other.String(),
	}))
	replacement := controlFrame(t, protocol.Control{Type: protocol.TypeAttach, SessionID: id.String()})
	if handled, err := relay.HandleFrame(context.Background(), replacement); !handled || err == nil {
		t.Fatalf("cross-session replacement handled = %v, error = %v", handled, err)
	}
	assertRelayFrame(t, candidate.nextWrite(t), replacement)
	candidate.waitClosed(t)
	assertRelayConnOpen(t, incumbent, "cross-session replacement closed incumbent")
}

func TestClientRelayOrdersGenerationReplacementWithOutboundFrames(t *testing.T) {
	client := newRelayTestConn()
	oldWorker := newRelayTestConn()
	newWorker := newRelayTestConn()
	connector := newRelayTestConnector(
		connectResult{conn: oldWorker},
		connectResult{conn: newWorker},
	)
	relay := newClientRelay(client, connector)
	t.Cleanup(func() { _ = relay.Close() })

	id := mustRelaySessionID(t, "SWAP")
	attachRelaySession(t, relay, client, oldWorker, id)
	entered, release := client.blockWrites()
	oldData := protocol.Frame{Kind: protocol.KindData, Session: id, Seq: 1, Payload: []byte("old")}
	oldWorker.pushRead(oldData)
	waitRelaySignal(t, entered, "old generation client write")

	replacement := controlFrame(t, protocol.Control{Type: protocol.TypeAttach, SessionID: id.String()})
	replaced := make(chan error, 1)
	go func() {
		handled, err := relay.HandleFrame(context.Background(), replacement)
		if err == nil && !handled {
			err = errors.New("replacement was not handled")
		}
		replaced <- err
	}()
	assertRelayFrame(t, newWorker.nextWrite(t), replacement)
	newAttached := controlFrame(t, protocol.Control{Type: protocol.TypeAttached, SessionID: id.String(), Seq: 4})
	newWorker.pushRead(newAttached)
	if err := waitRelayError(t, replaced, "queued generation replacement"); err != nil {
		t.Fatal(err)
	}

	release()
	assertRelayFrame(t, client.nextWrite(t), oldData)
	assertRelayFrame(t, client.nextWrite(t), newAttached)
	oldWorker.waitClosed(t)

	newData := protocol.Frame{Kind: protocol.KindData, Session: id, Seq: 4, Payload: []byte("new")}
	newWorker.pushRead(newData)
	assertRelayFrame(t, client.nextWrite(t), newData)
	input := protocol.Frame{Kind: protocol.KindInput, Session: id, Payload: []byte("owner")}
	if handled, err := relay.HandleFrame(context.Background(), input); err != nil || !handled {
		t.Fatalf("new generation input handled = %v, error = %v", handled, err)
	}
	assertRelayFrame(t, newWorker.nextWrite(t), input)
}

func TestClientRelayCloseInterruptsBlockedWorkWithoutLifecycleFrames(t *testing.T) {
	client := newRelayTestConn()
	worker := newRelayTestConn()
	connector := newRelayTestConnector(connectResult{conn: worker})
	relay := newClientRelay(client, connector)
	id := mustRelaySessionID(t, "LIVE")
	attachRelaySession(t, relay, client, worker, id)

	entered, _ := client.blockWrites()
	worker.pushRead(protocol.Frame{Kind: protocol.KindData, Session: id, Payload: []byte("blocked")})
	waitRelaySignal(t, entered, "blocked client output")

	closed := make(chan error, 1)
	go func() { closed <- relay.Close() }()
	if err := waitRelayError(t, closed, "relay close"); err != nil {
		t.Fatal(err)
	}
	client.waitClosed(t)
	worker.waitClosed(t)
	select {
	case frame := <-worker.writes:
		t.Fatalf("relay cleanup sent worker frame %+v", frame)
	default:
	}
}

func TestClientRelayCloseInterruptsPendingHandshake(t *testing.T) {
	client := newRelayTestConn()
	candidate := newRelayTestConn()
	connector := newRelayTestConnector(connectResult{conn: candidate})
	relay := newClientRelay(client, connector)
	id := mustRelaySessionID(t, "WAIT")
	attach := controlFrame(t, protocol.Control{Type: protocol.TypeAttach, SessionID: id.String()})
	handled := make(chan error, 1)
	go func() {
		wasHandled, err := relay.HandleFrame(context.Background(), attach)
		if err == nil && !wasHandled {
			err = errors.New("attach was not handled")
		}
		handled <- err
	}()
	assertRelayFrame(t, candidate.nextWrite(t), attach)

	closed := make(chan error, 1)
	go func() { closed <- relay.Close() }()
	if err := waitRelayError(t, closed, "close during handshake"); err != nil {
		t.Fatal(err)
	}
	if err := waitRelayError(t, handled, "pending handshake"); err == nil {
		t.Fatal("pending handshake returned without an error")
	}
	client.waitClosed(t)
	candidate.waitClosed(t)
}

func TestClientRelayDetachFlushesItsOrderedQueue(t *testing.T) {
	client := newRelayTestConn()
	worker := newRelayTestConn()
	connector := newRelayTestConnector(connectResult{conn: worker})
	relay := newClientRelay(client, connector)
	t.Cleanup(func() { _ = relay.Close() })

	id := mustRelaySessionID(t, "LEAVE")
	attachRelaySession(t, relay, client, worker, id)
	entered, release := worker.blockWrites()
	input := protocol.Frame{Kind: protocol.KindInput, Session: id, Payload: []byte("before")}
	if handled, err := relay.HandleFrame(context.Background(), input); err != nil || !handled {
		t.Fatalf("input handled = %v, error = %v", handled, err)
	}
	waitRelaySignal(t, entered, "blocked detach lane")
	detach := controlFrame(t, protocol.Control{Type: protocol.TypeDetach, SessionID: id.String()})
	if handled, err := relay.HandleFrame(context.Background(), detach); err != nil || !handled {
		t.Fatalf("detach handled = %v, error = %v", handled, err)
	}
	observed := worker.pushReadObserved(protocol.Frame{
		Kind:    protocol.KindData,
		Session: id,
		Payload: []byte("after detach"),
	})
	waitRelaySignal(t, observed, "detached lane output read")
	select {
	case <-worker.closed:
		t.Fatal("stale output closed the worker before queued detach was written")
	case <-time.After(25 * time.Millisecond):
	}

	release()
	assertRelayFrame(t, worker.nextWrite(t), input)
	assertRelayFrame(t, worker.nextWrite(t), detach)
	worker.waitClosed(t)
	select {
	case frame := <-client.writes:
		t.Fatalf("output crossed after detach: %+v", frame)
	default:
	}
}

func TestClientRelayRejectsMalformedAndCrossSessionFrames(t *testing.T) {
	client := newRelayTestConn()
	worker := newRelayTestConn()
	connector := newRelayTestConnector(connectResult{conn: worker})
	relay := newClientRelay(client, connector)
	t.Cleanup(func() { _ = relay.Close() })

	if handled, err := relay.HandleFrame(context.Background(), protocol.Frame{
		Kind:    protocol.KindControl,
		Payload: []byte("{"),
	}); !handled || err == nil {
		t.Fatalf("malformed control handled = %v, error = %v", handled, err)
	}
	if handled, err := relay.HandleFrame(context.Background(), protocol.Frame{
		Kind:    protocol.KindData,
		Payload: []byte("client output"),
	}); !handled || err == nil {
		t.Fatalf("client data handled = %v, error = %v", handled, err)
	}

	id := mustRelaySessionID(t, "GOOD")
	other := mustRelaySessionID(t, "EVIL")
	attachRelaySession(t, relay, client, worker, id)
	worker.pushRead(protocol.Frame{Kind: protocol.KindData, Session: other, Payload: []byte("wrong")})
	worker.waitClosed(t)
	select {
	case frame := <-client.writes:
		t.Fatalf("cross-session worker frame reached client: %+v", frame)
	default:
	}

	if handled, err := relay.HandleFrame(context.Background(), protocol.Frame{
		Kind:    protocol.KindInput,
		Session: id,
		Payload: []byte("after retirement"),
	}); !handled || err == nil {
		t.Fatalf("retired input handled = %v, error = %v", handled, err)
	}
}

func TestClientRelayInputQueueIsBounded(t *testing.T) {
	client := newRelayTestConn()
	worker := newRelayTestConn()
	connector := newRelayTestConnector(connectResult{conn: worker})
	relay := newClientRelay(client, connector)
	t.Cleanup(func() { _ = relay.Close() })

	id := mustRelaySessionID(t, "FULL")
	attachRelaySession(t, relay, client, worker, id)
	entered, release := worker.blockWrites()
	defer release()
	frame := protocol.Frame{Kind: protocol.KindInput, Session: id, Payload: []byte("x")}
	if _, err := relay.HandleFrame(context.Background(), frame); err != nil {
		t.Fatal(err)
	}
	waitRelaySignal(t, entered, "blocked input writer")
	for i := 1; i < relayInputQueueFrameLimit; i++ {
		if _, err := relay.HandleFrame(context.Background(), frame); err != nil {
			t.Fatalf("enqueue frame %d: %v", i, err)
		}
	}
	if handled, err := relay.HandleFrame(context.Background(), frame); !handled || err == nil {
		t.Fatalf("overflow handled = %v, error = %v", handled, err)
	}
}

func attachRelaySession(t *testing.T, relay *clientRelay, client, worker *relayTestConn, id protocol.SessionID) {
	t.Helper()
	frame := controlFrame(t, protocol.Control{Type: protocol.TypeAttach, SessionID: id.String()})
	attached := controlFrame(t, protocol.Control{Type: protocol.TypeAttached, SessionID: id.String()})
	worker.pushRead(attached)
	handled, err := relay.HandleFrame(context.Background(), frame)
	if err != nil || !handled {
		t.Fatalf("attach %s handled = %v, error = %v", id.String(), handled, err)
	}
	assertRelayFrame(t, worker.nextWrite(t), frame)
	assertRelayFrame(t, client.nextWrite(t), attached)
}

func assertRelayConnOpen(t *testing.T, conn *relayTestConn, message string) {
	t.Helper()
	select {
	case <-conn.closed:
		t.Fatal(message)
	default:
	}
}

func controlFrame(t *testing.T, message protocol.Control) protocol.Frame {
	t.Helper()
	payload, err := message.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return protocol.Frame{Kind: protocol.KindControl, Payload: payload}
}

func mustRelaySessionID(t *testing.T, value string) protocol.SessionID {
	t.Helper()
	id, err := protocol.NewSessionID(value)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func assertRelayFrame(t *testing.T, got, want protocol.Frame) {
	t.Helper()
	if got.Kind != want.Kind || got.Session != want.Session || got.Seq != want.Seq || !bytes.Equal(got.Payload, want.Payload) {
		t.Fatalf("frame = %+v payload %q, want %+v payload %q", got, got.Payload, want, want.Payload)
	}
}

func cloneRelayFrame(frame protocol.Frame) protocol.Frame {
	frame.Payload = bytes.Clone(frame.Payload)
	return frame
}

func waitRelaySignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func waitRelayError(t *testing.T, result <-chan error, name string) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", name)
		return nil
	}
}

type connectResult struct {
	conn transport.Conn
	err  error
}

type relayTestConnector struct {
	mu      sync.Mutex
	results []connectResult
}

func newRelayTestConnector(results ...connectResult) *relayTestConnector {
	return &relayTestConnector{results: append([]connectResult(nil), results...)}
}

func (c *relayTestConnector) ConnectWorker(ctx context.Context, _ protocol.SessionID) (transport.Conn, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.results) == 0 {
		return nil, errors.New("unexpected worker connection")
	}
	result := c.results[0]
	c.results = c.results[1:]
	return result.conn, result.err
}

type relayReadResult struct {
	frame    protocol.Frame
	err      error
	observed chan struct{}
}

type relayTestConn struct {
	reads  chan relayReadResult
	writes chan protocol.Frame
	closed chan struct{}

	closeOnce sync.Once
	blockMu   sync.Mutex
	writeGate <-chan struct{}
	entered   chan struct{}
}

func newRelayTestConn() *relayTestConn {
	return &relayTestConn{
		reads:  make(chan relayReadResult, 32),
		writes: make(chan protocol.Frame, 1024),
		closed: make(chan struct{}),
	}
}

func (c *relayTestConn) ReadFrame() (protocol.Frame, error) {
	select {
	case <-c.closed:
		return protocol.Frame{}, transport.ErrClosed
	default:
	}
	select {
	case result := <-c.reads:
		if result.observed != nil {
			close(result.observed)
		}
		return cloneRelayFrame(result.frame), result.err
	case <-c.closed:
		return protocol.Frame{}, transport.ErrClosed
	}
}

func (c *relayTestConn) WriteFrame(frame protocol.Frame) error {
	c.blockMu.Lock()
	gate := c.writeGate
	entered := c.entered
	c.blockMu.Unlock()
	if gate != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-gate:
		case <-c.closed:
			return transport.ErrClosed
		}
	}
	select {
	case c.writes <- cloneRelayFrame(frame):
		return nil
	case <-c.closed:
		return transport.ErrClosed
	}
}

func (c *relayTestConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func (c *relayTestConn) pushRead(frame protocol.Frame) {
	c.reads <- relayReadResult{frame: cloneRelayFrame(frame)}
}

func (c *relayTestConn) pushReadObserved(frame protocol.Frame) <-chan struct{} {
	observed := make(chan struct{})
	c.reads <- relayReadResult{frame: cloneRelayFrame(frame), observed: observed}
	return observed
}

func (c *relayTestConn) pushReadError(err error) {
	c.reads <- relayReadResult{err: err}
}

func (c *relayTestConn) nextWrite(t *testing.T) protocol.Frame {
	t.Helper()
	select {
	case frame := <-c.writes:
		return frame
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for frame write")
		return protocol.Frame{}
	}
}

func (c *relayTestConn) blockWrites() (<-chan struct{}, func()) {
	c.blockMu.Lock()
	defer c.blockMu.Unlock()
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	c.writeGate = gate
	c.entered = entered
	var once sync.Once
	return entered, func() {
		once.Do(func() {
			close(gate)
			c.blockMu.Lock()
			if c.writeGate == gate {
				c.writeGate = nil
				c.entered = nil
			}
			c.blockMu.Unlock()
		})
	}
}

func (c *relayTestConn) waitClosed(t *testing.T) {
	t.Helper()
	waitRelaySignal(t, c.closed, fmt.Sprintf("connection %p close", c))
}

var _ transport.Conn = (*relayTestConn)(nil)
