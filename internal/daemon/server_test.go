package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/transport"
)

func TestClientServerDispatchesRelayAndLifecycleFrames(t *testing.T) {
	client := newServerTestConn()
	worker := newServerTestConn()
	controlWorker := newServerTestConn()
	workers := newServerTestConnector(worker)
	lifecycle := mustServerTestLifecycle(t, &serverTestCatalog{}, newServerTestConnector(controlWorker))
	server, err := newClientServer(lifecycle, workers)
	if err != nil {
		t.Fatal(err)
	}

	id := mustServerSessionID(t, "7K3D")
	attached := serverControlFrame(t, protocol.Control{
		Type:      protocol.TypeAttached,
		SessionID: id.String(),
		Seq:       12,
	})
	worker.pushRead(attached)
	done := make(chan error, 1)
	go func() { done <- server.Handle(context.Background(), client) }()

	attach := serverControlFrame(t, protocol.Control{
		Type:      protocol.TypeAttach,
		SessionID: id.String(),
	})
	client.pushRead(attach)
	assertServerFrame(t, worker.nextWrite(t), attach)
	assertServerFrame(t, client.nextWrite(t), attached)

	input := protocol.Frame{Kind: protocol.KindInput, Session: id, Payload: []byte("hello")}
	client.pushRead(input)
	assertServerFrame(t, worker.nextWrite(t), input)

	list := serverControlFrame(t, protocol.Control{Type: protocol.TypeList, RequestID: "list-1"})
	client.pushRead(list)
	listed := decodeServerControl(t, client.nextWrite(t))
	if listed.Type != protocol.TypeListed || listed.RequestID != "list-1" {
		t.Fatalf("list response = %+v", listed)
	}

	signal := serverControlFrame(t, protocol.Control{
		Type:      protocol.TypeSignal,
		RequestID: "signal-1",
		SessionID: id.String(),
		Signal:    "term",
	})
	client.pushRead(signal)
	forwardedSignal := decodeServerControl(t, controlWorker.nextWrite(t))
	if forwardedSignal.Type != protocol.TypeSignal || forwardedSignal.RequestID != "signal-1" || forwardedSignal.SessionID != id.String() || forwardedSignal.Signal != "term" {
		t.Fatalf("forwarded signal = %+v", forwardedSignal)
	}
	controlWorker.waitClosed(t)
	ok := decodeServerControl(t, client.nextWrite(t))
	if ok.Type != protocol.TypeOK || ok.RequestID != "signal-1" || ok.SessionID != id.String() {
		t.Fatalf("signal response = %+v", ok)
	}
	select {
	case <-worker.closed:
		t.Fatal("one-shot signal closed the attached relay worker")
	default:
	}

	client.pushReadError(io.EOF)
	if err := waitServerResult(t, done, "client server"); err != nil {
		t.Fatal(err)
	}
	client.waitClosed(t)
	worker.waitClosed(t)
	select {
	case frame := <-worker.writes:
		t.Fatalf("disconnect sent lifecycle frame to worker: %+v", frame)
	default:
	}
}

func TestClientServerReturnsCorrelatedRequestErrorsAndContinues(t *testing.T) {
	client := newServerTestConn()
	lifecycle := mustServerTestLifecycle(t, &serverTestCatalog{}, failingServerTestConnector())
	server, err := newClientServer(lifecycle, failingServerTestConnector())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Handle(context.Background(), client) }()

	client.pushRead(protocol.Frame{
		Kind:    protocol.KindControl,
		Payload: []byte(`{"requestId":"decode-1","sessionId":"7K3D"}`),
	})
	assertServerError(t, client.nextWrite(t), "decode-1", "7K3D", "without type")

	client.pushRead(serverControlFrame(t, protocol.Control{
		Type:      "session.unknown",
		RequestID: "unknown-1",
		SessionID: "ABCD",
	}))
	assertServerError(t, client.nextWrite(t), "unknown-1", "ABCD", "unknown control")

	client.pushRead(serverControlFrame(t, protocol.Control{
		Type:      protocol.TypeSignal,
		RequestID: "signal-1",
		SessionID: "../BAD",
		Signal:    "term",
	}))
	assertServerError(t, client.nextWrite(t), "signal-1", "../BAD", "session")

	client.pushRead(serverControlFrame(t, protocol.Control{
		Type:      protocol.TypeAttach,
		RequestID: "attach-1",
		SessionID: "FAIL",
	}))
	assertServerError(t, client.nextWrite(t), "attach-1", "FAIL", "connect worker")

	client.pushRead(serverControlFrame(t, protocol.Control{
		Type:      protocol.TypeHostInfo,
		RequestID: "host-1",
	}))
	host := decodeServerControl(t, client.nextWrite(t))
	if host.Type != protocol.TypeHostInfoResult || host.RequestID != "host-1" || host.Host == nil || host.Host.ID != "host-a" {
		t.Fatalf("host response = %+v", host)
	}

	client.pushReadError(io.EOF)
	if err := waitServerResult(t, done, "error response loop"); err != nil {
		t.Fatal(err)
	}
}

func TestClientServerSerializesLifecycleAndRelayWrites(t *testing.T) {
	client := newServerTestConn()
	worker := newServerTestConn()
	workers := newServerTestConnector(worker)
	listCalled := make(chan struct{}, 1)
	lifecycle := mustServerTestLifecycle(t, &serverTestCatalog{listCalled: listCalled}, failingServerTestConnector())
	server, err := newClientServer(lifecycle, workers)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Handle(context.Background(), client) }()

	id := mustServerSessionID(t, "SYNC")
	attach := serverControlFrame(t, protocol.Control{Type: protocol.TypeAttach, SessionID: id.String()})
	attached := serverControlFrame(t, protocol.Control{Type: protocol.TypeAttached, SessionID: id.String()})
	worker.pushRead(attached)
	client.pushRead(attach)
	assertServerFrame(t, worker.nextWrite(t), attach)
	assertServerFrame(t, client.nextWrite(t), attached)

	entered, release := client.blockNextWrite()
	defer release()
	data := protocol.Frame{Kind: protocol.KindData, Session: id, Payload: []byte("output")}
	worker.pushRead(data)
	waitServerSignal(t, entered, "relay output write")
	client.pushRead(serverControlFrame(t, protocol.Control{Type: protocol.TypeList, RequestID: "list-race"}))
	waitServerSignal(t, listCalled, "lifecycle list")
	select {
	case <-client.concurrentWrite:
		t.Fatal("lifecycle and relay entered the client writer concurrently")
	case <-time.After(25 * time.Millisecond):
	}

	release()
	assertServerFrame(t, client.nextWrite(t), data)
	listed := decodeServerControl(t, client.nextWrite(t))
	if listed.Type != protocol.TypeListed || listed.RequestID != "list-race" {
		t.Fatalf("racing list response = %+v", listed)
	}
	client.pushReadError(io.EOF)
	if err := waitServerResult(t, done, "serialized server"); err != nil {
		t.Fatal(err)
	}
}

func TestClientServerTreatsClientTerminationAsNormal(t *testing.T) {
	lifecycle := mustServerTestLifecycle(t, &serverTestCatalog{}, failingServerTestConnector())
	server, err := newClientServer(lifecycle, failingServerTestConnector())
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "EOF", err: io.EOF},
		{name: "closed pipe", err: io.ErrClosedPipe},
		{name: "network close", err: net.ErrClosed},
		{name: "transport close", err: transport.ErrClosed},
		{name: "context cancellation", err: context.Canceled},
		{name: "context deadline", err: context.DeadlineExceeded},
		{name: "WebSocket close", err: websocket.CloseError{Code: websocket.StatusNormalClosure}},
		{name: "WebSocket going away", err: websocket.CloseError{Code: websocket.StatusGoingAway}},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := newServerTestConn()
			client.pushReadError(fmt.Errorf("read: %w", test.err))
			if err := server.Handle(context.Background(), client); err != nil {
				t.Fatalf("Handle error = %v", err)
			}
			client.waitClosed(t)
		})
	}

	wantErr := errors.New("broken transport")
	client := newServerTestConn()
	client.pushReadError(wantErr)
	if err := server.Handle(context.Background(), client); !errors.Is(err, wantErr) {
		t.Fatalf("Handle error = %v, want %v", err, wantErr)
	}
}

func TestClientServerContextCancellationClosesBlockedClient(t *testing.T) {
	lifecycle := mustServerTestLifecycle(t, &serverTestCatalog{}, failingServerTestConnector())
	server, err := newClientServer(lifecycle, failingServerTestConnector())
	if err != nil {
		t.Fatal(err)
	}
	client := newServerTestConn()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Handle(ctx, client) }()
	cancel()
	if err := waitServerResult(t, done, "context cancellation"); err != nil {
		t.Fatal(err)
	}
	client.waitClosed(t)
}

func TestNewClientServerRejectsNilDependencies(t *testing.T) {
	lifecycle := mustServerTestLifecycle(t, &serverTestCatalog{}, failingServerTestConnector())
	if _, err := newClientServer(nil, failingServerTestConnector()); err == nil {
		t.Fatal("nil lifecycle accepted")
	}
	if _, err := newClientServer(lifecycle, nil); err == nil {
		t.Fatal("nil worker connector accepted")
	}
	server, err := newClientServer(lifecycle, failingServerTestConnector())
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Handle(nil, newServerTestConn()); err == nil {
		t.Fatal("nil client context accepted")
	}
	if err := server.Handle(context.Background(), nil); err == nil {
		t.Fatal("nil client connection accepted")
	}
}

func mustServerTestLifecycle(t *testing.T, catalog lifecycleCatalog, connector WorkerConnector) *lifecycle {
	t.Helper()
	got, err := newLifecycle(lifecycleConfig{
		Catalog:     catalog,
		Connector:   connector,
		Host:        storage.Host{ID: "host-a", MeshIdentity: "mesh-key", LastSeenAt: time.Now()},
		SessionsDir: "/state/s",
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

type serverTestCatalog struct {
	listCalled chan<- struct{}
}

func (*serverTestCatalog) Reconcile(context.Context) error { return nil }

func (c *serverTestCatalog) List(context.Context) ([]storage.Session, error) {
	if c.listCalled != nil {
		select {
		case c.listCalled <- struct{}{}:
		default:
		}
	}
	return nil, nil
}

func (*serverTestCatalog) Get(context.Context, storage.SessionID) (storage.Session, error) {
	return storage.Session{}, errors.New("unused")
}

type serverTestConnector struct {
	mu    sync.Mutex
	conns []transport.Conn
}

func newServerTestConnector(conns ...transport.Conn) *serverTestConnector {
	return &serverTestConnector{conns: append([]transport.Conn(nil), conns...)}
}

func failingServerTestConnector() WorkerConnector { return newServerTestConnector() }

func (c *serverTestConnector) ConnectWorker(ctx context.Context, _ protocol.SessionID) (transport.Conn, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.conns) == 0 {
		return nil, errors.New("unexpected worker connection")
	}
	conn := c.conns[0]
	c.conns = c.conns[1:]
	return conn, nil
}

var errServerConcurrentWrite = errors.New("concurrent client write")

type serverTestRead struct {
	frame protocol.Frame
	err   error
}

type serverTestConn struct {
	reads           chan serverTestRead
	writes          chan protocol.Frame
	closed          chan struct{}
	concurrentWrite chan struct{}

	closeOnce sync.Once
	active    atomic.Int32
	blockMu   sync.Mutex
	nextGate  <-chan struct{}
	entered   chan struct{}
}

func newServerTestConn() *serverTestConn {
	return &serverTestConn{
		reads:           make(chan serverTestRead, 32),
		writes:          make(chan protocol.Frame, 128),
		closed:          make(chan struct{}),
		concurrentWrite: make(chan struct{}, 1),
	}
}

func (c *serverTestConn) ReadFrame() (protocol.Frame, error) {
	select {
	case result := <-c.reads:
		return serverCloneFrame(result.frame), result.err
	case <-c.closed:
		return protocol.Frame{}, transport.ErrClosed
	}
}

func (c *serverTestConn) WriteFrame(frame protocol.Frame) error {
	if c.active.Add(1) != 1 {
		c.active.Add(-1)
		select {
		case c.concurrentWrite <- struct{}{}:
		default:
		}
		return errServerConcurrentWrite
	}
	defer c.active.Add(-1)

	c.blockMu.Lock()
	gate := c.nextGate
	entered := c.entered
	c.nextGate = nil
	c.entered = nil
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
	case c.writes <- serverCloneFrame(frame):
		return nil
	case <-c.closed:
		return transport.ErrClosed
	}
}

func (c *serverTestConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func (c *serverTestConn) pushRead(frame protocol.Frame) {
	c.reads <- serverTestRead{frame: serverCloneFrame(frame)}
}

func (c *serverTestConn) pushReadError(err error) {
	c.reads <- serverTestRead{err: err}
}

func (c *serverTestConn) nextWrite(t *testing.T) protocol.Frame {
	t.Helper()
	select {
	case frame := <-c.writes:
		return frame
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for server write")
		return protocol.Frame{}
	}
}

func (c *serverTestConn) blockNextWrite() (<-chan struct{}, func()) {
	c.blockMu.Lock()
	defer c.blockMu.Unlock()
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	c.nextGate = gate
	c.entered = entered
	var once sync.Once
	return entered, func() { once.Do(func() { close(gate) }) }
}

func (c *serverTestConn) waitClosed(t *testing.T) {
	t.Helper()
	waitServerSignal(t, c.closed, fmt.Sprintf("connection %p close", c))
}

func serverControlFrame(t *testing.T, message protocol.Control) protocol.Frame {
	t.Helper()
	payload, err := message.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return protocol.Frame{Kind: protocol.KindControl, Payload: payload}
}

func decodeServerControl(t *testing.T, frame protocol.Frame) protocol.Control {
	t.Helper()
	if frame.Kind != protocol.KindControl {
		t.Fatalf("frame kind = %d, want control", frame.Kind)
	}
	message, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func assertServerError(t *testing.T, frame protocol.Frame, requestID, sessionID, messagePart string) {
	t.Helper()
	message := decodeServerControl(t, frame)
	if message.Type != protocol.TypeError || message.RequestID != requestID || message.SessionID != sessionID || !strings.Contains(message.Message, messagePart) {
		t.Fatalf("error response = %+v, want request %q, session %q, message containing %q", message, requestID, sessionID, messagePart)
	}
}

func mustServerSessionID(t *testing.T, value string) protocol.SessionID {
	t.Helper()
	id, err := protocol.NewSessionID(value)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func assertServerFrame(t *testing.T, got, want protocol.Frame) {
	t.Helper()
	if got.Kind != want.Kind || got.Session != want.Session || got.Seq != want.Seq || !bytes.Equal(got.Payload, want.Payload) {
		t.Fatalf("frame = %+v payload %q, want %+v payload %q", got, got.Payload, want, want.Payload)
	}
}

func serverCloneFrame(frame protocol.Frame) protocol.Frame {
	frame.Payload = bytes.Clone(frame.Payload)
	return frame
}

func waitServerSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func waitServerResult(t *testing.T, result <-chan error, name string) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", name)
		return nil
	}
}

var _ transport.Conn = (*serverTestConn)(nil)
