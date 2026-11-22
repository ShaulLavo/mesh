package mcpnative

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/mcpwire"
)

// rotationHangCap only catches a stuck phase; it is not a speed judgement.
const rotationHangCap = 10 * time.Second

// rotationGate sits between net/http and a real loopback socket. It holds the
// first server write that contains marker until the test releases it or a
// write deadline in the past interrupts it, and it records every byte handed
// to the socket, which is everything the client can read.
type rotationGate struct {
	marker []byte
	held   atomic.Bool

	entered     chan struct{}
	release     chan struct{}
	releaseOnce sync.Once
	aborted     chan struct{}
	abortOnce   sync.Once
	closed      chan struct{}
	closeOnce   sync.Once
	firstSent   chan struct{}
	firstOnce   sync.Once

	mu   sync.Mutex
	sent bytes.Buffer
}

func newRotationGate(marker string) *rotationGate {
	return &rotationGate{
		marker:    []byte(marker),
		entered:   make(chan struct{}),
		release:   make(chan struct{}),
		aborted:   make(chan struct{}),
		closed:    make(chan struct{}),
		firstSent: make(chan struct{}),
	}
}

func (g *rotationGate) record(p []byte) {
	g.mu.Lock()
	g.sent.Write(p)
	g.mu.Unlock()
	g.firstOnce.Do(func() { close(g.firstSent) })
}

func (g *rotationGate) sentBytes() []byte {
	g.mu.Lock()
	defer g.mu.Unlock()
	return bytes.Clone(g.sent.Bytes())
}

func (g *rotationGate) isAborted() bool {
	select {
	case <-g.aborted:
		return true
	default:
		return false
	}
}

func (g *rotationGate) releaseHeld() { g.releaseOnce.Do(func() { close(g.release) }) }

// holdWrite blocks like a write to a client that stopped reading.
func (g *rotationGate) holdWrite(conn net.Conn, p []byte) (int, error) {
	close(g.entered)
	select {
	case <-g.release:
		g.record(p)
		return writeSocket(conn, p)
	case <-g.aborted:
	}
	// A deadline that fires mid-write does not recall what the kernel already
	// accepted: half the frame still reaches the peer, a moment after the
	// deadline fired.
	time.Sleep(5 * time.Millisecond)
	half := p[:len(p)/2]
	g.record(half)
	_, _ = conn.Write(half)
	return len(half), os.ErrDeadlineExceeded
}

func writeSocket(conn net.Conn, p []byte) (int, error) {
	n, err := conn.Write(p)
	if err != nil {
		return n, fmt.Errorf("gated socket write: %w", err)
	}
	return n, nil
}

type gatedConn struct {
	net.Conn
	gate *rotationGate
}

func (c *gatedConn) Write(p []byte) (int, error) {
	g := c.gate
	if bytes.Contains(p, g.marker) && g.held.CompareAndSwap(false, true) {
		return g.holdWrite(c.Conn, p)
	}
	if g.isAborted() {
		return 0, os.ErrDeadlineExceeded
	}
	g.record(p)
	return writeSocket(c.Conn, p)
}

func (c *gatedConn) SetWriteDeadline(t time.Time) error {
	if !t.IsZero() && !t.After(time.Now()) {
		c.gate.abortOnce.Do(func() { close(c.gate.aborted) })
	}
	if err := c.Conn.SetWriteDeadline(t); err != nil {
		return fmt.Errorf("gated socket deadline: %w", err)
	}
	return nil
}

func (c *gatedConn) Close() error {
	c.gate.closeOnce.Do(func() { close(c.gate.closed) })
	if err := c.Conn.Close(); err != nil {
		return fmt.Errorf("gated socket close: %w", err)
	}
	return nil
}

type gatedListener struct {
	net.Listener
	gate *rotationGate
}

func (l *gatedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, fmt.Errorf("gated listener accept: %w", err)
	}
	return &gatedConn{Conn: conn, gate: l.gate}, nil
}

func waitPhase(t *testing.T, ch <-chan struct{}, phase string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(rotationHangCap):
		t.Fatalf("stuck waiting for %s", phase)
	}
}

// awaitCapped is waitPhase for stub workers, which run off the test goroutine
// and cannot fail the test; the test's own phase wait reports the stall.
func awaitCapped(ch <-chan struct{}) {
	select {
	case <-ch:
	case <-time.After(rotationHangCap):
	}
}

type rotationCase struct {
	marker        string
	request       []byte
	controls      *stubControls
	beforeRotate  func(srv *HTTPServer)
	gateRef       *atomic.Pointer[rotationGate] // filled before the request is sent, for stub workers
	wantRetired   int
	wantDelivered []string
}

// runRotationCase serves one raw request through the real net/http writer,
// rotates the token while the marked write is held, and fails if the server
// hands any byte to the socket after RotateToken returns.
func runRotationCase(t *testing.T, tc rotationCase) []byte {
	t.Helper()
	gate := newRotationGate(tc.marker)
	if tc.gateRef != nil {
		tc.gateRef.Store(gate)
	}
	srv, err := NewHTTPServer(HTTPServerOptions{
		ServerOptions: ServerOptions{Version: "0.1.0-rot", Controls: tc.controls},
		Authority:     "127.0.0.1:9000",
		Token:         testHTTPToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(srv)
	ts.Listener = &gatedListener{Listener: ts.Listener, gate: gate}
	ts.Start()
	t.Cleanup(func() {
		gate.releaseHeld()
		ts.Close()
		_ = srv.Shutdown(context.Background())
	})

	client, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	if _, err := client.Write(tc.request); err != nil {
		t.Fatal(err)
	}

	waitPhase(t, gate.entered, "the held write")
	if tc.beforeRotate != nil {
		tc.beforeRotate(srv)
	}
	gen, retired, err := srv.RotateToken(testHTTPTokenRotated)
	atReturn := len(gate.sentBytes())
	if err != nil || gen != 2 || retired != tc.wantRetired {
		t.Fatalf("RotateToken = gen=%d retired=%d err=%v, want gen=2 retired=%d", gen, retired, err, tc.wantRetired)
	}

	gate.releaseHeld()
	_ = client.Close()
	waitPhase(t, gate.closed, "the server to close the connection")

	sent := gate.sentBytes()
	if len(sent) > atReturn {
		t.Fatalf("server wrote %d bytes after RotateToken returned: %q", len(sent)-atReturn, sent[atReturn:])
	}
	for _, want := range tc.wantDelivered {
		if !bytes.Contains(sent, []byte(want)) {
			t.Fatalf("client never received %q before rotation: %q", want, sent)
		}
	}
	return sent
}

func rotationWaitBody(extraMeta string) string {
	return fmt.Sprintf(
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"_meta":{"%s":"%s","%s":{}%s},"name":"mesh_job_wait","arguments":{"job":%q,"wait_ms":5000}}}`,
		mcpwire.VersionKey, mcpwire.Version, mcpwire.ClientCapsKey, extraMeta, testJob,
	)
}

func rawRotationRequest(body, methodHeader string, contentLength int) []byte {
	var b strings.Builder
	b.WriteString("POST /mcp HTTP/1.1\r\nHost: 127.0.0.1:9000\r\n")
	fmt.Fprintf(&b, "Authorization: Bearer %s\r\n", testHTTPToken)
	b.WriteString("Content-Type: application/json\r\nAccept: application/json, text/event-stream\r\n")
	fmt.Fprintf(&b, "%s: %s\r\n%s: %s\r\n%s: mesh_job_wait\r\n", mcpwire.ProtocolHeader, mcpwire.Version, mcpwire.MethodHeader, methodHeader, mcpwire.NameHeader)
	fmt.Fprintf(&b, "Content-Length: %d\r\n\r\n%s", contentLength, body)
	return []byte(b.String())
}

func rotationCallRequest(body string) []byte {
	return rawRotationRequest(body, mcpwire.MethodCallTool, len(body))
}

func completedWait(req JobWaitRequest) CallOutcome {
	return CallOutcome{
		Envelope: okEnvelope("sessions.wait", JobStateData{Job: req.Job.String(), State: "exited", Wait: "completed"}),
	}
}

func TestHTTPRotationStopsHeldJSONResult(t *testing.T) {
	runRotationCase(t, rotationCase{
		marker:  `"id":7`,
		request: rotationCallRequest(rotationWaitBody("")),
		controls: &stubControls{waitFn: func(_ context.Context, req JobWaitRequest, _ func(ProgressUpdate)) (CallOutcome, error) {
			return completedWait(req), nil
		}},
		wantRetired: 1,
	})
}

// The SSE headers and a progress event are already on the wire when the
// terminal event's write is held.
func TestHTTPRotationStopsHeldSSEResultAfterHeadersSent(t *testing.T) {
	var gate atomic.Pointer[rotationGate]
	tc := rotationCase{
		marker:  `"id":7`,
		request: rotationCallRequest(rotationWaitBody(`,"progressToken":"p7"`)),
		controls: &stubControls{waitFn: func(_ context.Context, req JobWaitRequest, notify func(ProgressUpdate)) (CallOutcome, error) {
			total := float64(req.WaitMS)
			notify(ProgressUpdate{Progress: 1, Total: &total, Message: "waiting"})
			awaitCapped(gate.Load().firstSent)
			return completedWait(req), nil
		}},
		gateRef:       &gate,
		wantRetired:   1,
		wantDelivered: []string{"text/event-stream", "notifications/progress"},
	}
	runRotationCase(t, tc)
}

// The progress write is held while the finished result waits in the queue;
// rotation must suppress the queued result entirely.
func TestHTTPRotationSuppressesQueuedResultBehindHeldProgress(t *testing.T) {
	var gate atomic.Pointer[rotationGate]
	tc := rotationCase{
		marker:  "notifications/progress",
		request: rotationCallRequest(rotationWaitBody(`,"progressToken":"p7"`)),
		controls: &stubControls{waitFn: func(_ context.Context, req JobWaitRequest, notify func(ProgressUpdate)) (CallOutcome, error) {
			total := float64(req.WaitMS)
			notify(ProgressUpdate{Progress: 1, Total: &total, Message: "waiting"})
			awaitCapped(gate.Load().entered)
			return completedWait(req), nil
		}},
		// The worker returning only marks the stub's return; waiting for the
		// worker goroutine guarantees the result is enqueued before rotation.
		beforeRotate: func(srv *HTTPServer) { srv.workers.Wait() },
		gateRef:      &gate,
		wantRetired:  1,
	}
	sent := runRotationCase(t, tc)
	if bytes.Contains(sent, []byte(`"id":7`)) {
		t.Fatalf("retired stream delivered the queued result: %q", sent)
	}
}

// A validation fault is small enough to sit in net/http's buffer; it must not
// escape with the flush net/http performs after the handler returns.
func TestHTTPRotationStopsValidationReply(t *testing.T) {
	body := rotationWaitBody("")
	runRotationCase(t, rotationCase{
		marker:      `"jsonrpc"`,
		request:     rawRotationRequest(body, mcpwire.MethodListTools, len(body)),
		controls:    &stubControls{},
		wantRetired: 1,
	})
}

// A request rejected before admission still answers with a fault for the old
// token, so its lease is retired too (it is not counted as an exchange).
func TestHTTPRotationStopsPreAdmissionFault(t *testing.T) {
	runRotationCase(t, rotationCase{
		marker:      `"jsonrpc"`,
		request:     rawRotationRequest("", mcpwire.MethodCallTool, mcpwire.MaxMessage+1),
		controls:    &stubControls{},
		wantRetired: 0,
	})
}
