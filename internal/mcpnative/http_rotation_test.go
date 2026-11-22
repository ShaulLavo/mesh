package mcpnative

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/mcpwire"
)

// rotationHangCap only catches a stuck phase; it is not a speed judgement.
const rotationHangCap = 10 * time.Second

// rotationGate sits between net/http and real loopback sockets. It holds the
// first server write that contains marker, like a write to a client that
// stopped reading, until the test releases it or the socket is interrupted.
// It records every byte handed to each socket, which is what its client can
// read.
type rotationGate struct {
	marker []byte
	held   atomic.Bool
	// holdThroughClose keeps the held write blocked until release even when
	// rotation closes or interrupts the socket, as a slow kernel copy would.
	holdThroughClose bool
	// holdDeadline pauses the first past write deadline until the test
	// releases it.
	holdDeadline bool

	entered        chan struct{}
	release        chan struct{}
	releaseOnce    sync.Once
	firstSent      chan struct{}
	firstOnce      sync.Once
	touched        chan struct{}
	touchOnce      sync.Once
	deadlineHeld   chan struct{}
	deadlineFree   chan struct{}
	deadlineOnce   sync.Once
	deadlineHolder atomic.Bool

	mu    sync.Mutex
	conns []*gatedConn
}

func newRotationGate(marker string) *rotationGate {
	return &rotationGate{
		marker:       []byte(marker),
		entered:      make(chan struct{}),
		release:      make(chan struct{}),
		firstSent:    make(chan struct{}),
		touched:      make(chan struct{}),
		deadlineHeld: make(chan struct{}),
		deadlineFree: make(chan struct{}),
	}
}

func (g *rotationGate) releaseHeld() { g.releaseOnce.Do(func() { close(g.release) }) }

func (g *rotationGate) releaseDeadline() { g.deadlineOnce.Do(func() { close(g.deadlineFree) }) }

// touch reports that rotation reached the socket: it closed it or set a
// past deadline on it.
func (g *rotationGate) touch() { g.touchOnce.Do(func() { close(g.touched) }) }

func (g *rotationGate) conn(i int) *gatedConn {
	g.mu.Lock()
	defer g.mu.Unlock()
	if i >= len(g.conns) {
		return nil
	}
	return g.conns[i]
}

type gatedConn struct {
	net.Conn
	gate *rotationGate

	// ioMu makes Close wait for an in-flight write, as closing a Go socket
	// waits for the write holding it to return.
	ioMu       sync.RWMutex
	isClosed   bool
	closing    chan struct{}
	closeOnce  sync.Once
	closed     chan struct{}
	closedOnce sync.Once
	aborted    chan struct{}
	abortOnce  sync.Once

	sentMu sync.Mutex
	sent   bytes.Buffer
}

func (c *gatedConn) record(p []byte) {
	c.sentMu.Lock()
	c.sent.Write(p)
	c.sentMu.Unlock()
	c.gate.firstOnce.Do(func() { close(c.gate.firstSent) })
}

func (c *gatedConn) sentBytes() []byte {
	c.sentMu.Lock()
	defer c.sentMu.Unlock()
	return bytes.Clone(c.sent.Bytes())
}

func (c *gatedConn) isAborted() bool {
	select {
	case <-c.aborted:
		return true
	default:
		return false
	}
}

func (c *gatedConn) Write(p []byte) (int, error) {
	c.ioMu.RLock()
	defer c.ioMu.RUnlock()
	if c.isClosed {
		return 0, net.ErrClosed
	}
	if c.isAborted() {
		return 0, os.ErrDeadlineExceeded
	}
	g := c.gate
	if len(g.marker) > 0 && bytes.Contains(p, g.marker) && g.held.CompareAndSwap(false, true) {
		return c.holdWrite(p)
	}
	c.record(p)
	return writeSocket(c.Conn, p)
}

func (c *gatedConn) holdWrite(p []byte) (int, error) {
	g := c.gate
	close(g.entered)
	closing, aborted := c.closing, c.aborted
	if g.holdThroughClose {
		closing, aborted = nil, nil
	}
	select {
	case <-g.release:
		c.record(p)
		return writeSocket(c.Conn, p)
	case <-closing:
	case <-aborted:
	}
	// An interrupted write does not recall what the kernel already
	// accepted: half the bytes still reach the peer, a moment later.
	time.Sleep(5 * time.Millisecond)
	half := p[:len(p)/2]
	c.record(half)
	_, _ = c.Conn.Write(half)
	return len(half), net.ErrClosed
}

func writeSocket(conn net.Conn, p []byte) (int, error) {
	n, err := conn.Write(p)
	if err != nil {
		return n, fmt.Errorf("gated socket write: %w", err)
	}
	return n, nil
}

// holdPastDeadline sees only write deadlines: interrupting a socket from the
// frontend starts with its write deadline, while past read deadlines also come
// from net/http aborting its own reads and from refusing a request body.
func (c *gatedConn) holdPastDeadline(t time.Time) {
	if t.IsZero() || t.After(time.Now()) {
		return
	}
	c.gate.touch()
	g := c.gate
	if g.holdDeadline && g.deadlineHolder.CompareAndSwap(false, true) {
		close(g.deadlineHeld)
		<-g.deadlineFree
	}
}

func (c *gatedConn) SetWriteDeadline(t time.Time) error {
	c.holdPastDeadline(t)
	if !t.IsZero() && !t.After(time.Now()) {
		c.abortOnce.Do(func() { close(c.aborted) })
	}
	if err := c.Conn.SetWriteDeadline(t); err != nil {
		return fmt.Errorf("gated socket write deadline: %w", err)
	}
	return nil
}

func (c *gatedConn) SetReadDeadline(t time.Time) error {
	if err := c.Conn.SetReadDeadline(t); err != nil {
		return fmt.Errorf("gated socket read deadline: %w", err)
	}
	return nil
}

func (c *gatedConn) Close() error {
	c.gate.touch()
	c.closeOnce.Do(func() { close(c.closing) })
	c.ioMu.Lock()
	c.isClosed = true
	c.ioMu.Unlock()
	err := c.Conn.Close()
	c.closedOnce.Do(func() { close(c.closed) })
	if err != nil {
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
	gc := &gatedConn{
		Conn:    conn,
		gate:    l.gate,
		closing: make(chan struct{}),
		closed:  make(chan struct{}),
		aborted: make(chan struct{}),
	}
	l.gate.mu.Lock()
	l.gate.conns = append(l.gate.conns, gc)
	l.gate.mu.Unlock()
	return gc, nil
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

type rotationResult struct {
	gen     uint64
	retired int
	err     error
	// sentAtReturn is how many bytes the first connection had carried when
	// RotateToken returned.
	sentAtReturn int
}

type rotationHarness struct {
	gate *rotationGate
	srv  *HTTPServer
	ts   *httptest.Server
}

func startRotationHarness(t *testing.T, gate *rotationGate, opts ServerOptions) *rotationHarness {
	t.Helper()
	opts.Version = "0.1.0-rot"
	srv, err := NewHTTPServer(HTTPServerOptions{
		ServerOptions: opts,
		Authority:     "127.0.0.1:9000",
		Token:         testHTTPToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(srv)
	srv.Attach(ts.Config)
	ts.Listener = &gatedListener{Listener: ts.Listener, gate: gate}
	ts.Start()
	t.Cleanup(func() {
		gate.releaseHeld()
		gate.releaseDeadline()
		_ = srv.Shutdown(context.Background())
		ts.Close()
	})
	return &rotationHarness{gate: gate, srv: srv, ts: ts}
}

func (h *rotationHarness) dial(t *testing.T, request []byte) net.Conn {
	t.Helper()
	client, err := net.Dial("tcp", h.ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	_ = client.SetDeadline(time.Now().Add(rotationHangCap))
	if _, err := client.Write(request); err != nil {
		t.Fatal(err)
	}
	return client
}

// rotate runs RotateToken off the test goroutine so a stuck rotation fails
// the test instead of hanging it.
func (h *rotationHarness) rotate() <-chan rotationResult {
	out := make(chan rotationResult, 1)
	go func() {
		gen, retired, err := h.srv.RotateToken(testHTTPTokenRotated)
		res := rotationResult{gen: gen, retired: retired, err: err}
		if c := h.gate.conn(0); c != nil {
			res.sentAtReturn = len(c.sentBytes())
		}
		out <- res
	}()
	return out
}

// anyRetired skips the retired count where it depends on whether the held
// write happens before or after the handler returns.
const anyRetired = -1

func waitRotation(t *testing.T, ch <-chan rotationResult, wantRetired int) rotationResult {
	t.Helper()
	select {
	case res := <-ch:
		retiredOK := wantRetired == anyRetired || res.retired == wantRetired
		if res.err != nil || res.gen != 2 || !retiredOK {
			t.Fatalf("RotateToken = gen=%d retired=%d err=%v, want gen=2 retired=%d", res.gen, res.retired, res.err, wantRetired)
		}
		return res
	case <-time.After(rotationHangCap):
		t.Fatal("stuck waiting for RotateToken to return")
		return rotationResult{}
	}
}

// assertNothingAfter fails if the first connection carried any byte after
// RotateToken returned. The connection must be closed by then, so no later
// byte can follow.
func assertNothingAfter(t *testing.T, gate *rotationGate, res rotationResult) []byte {
	t.Helper()
	c := gate.conn(0)
	gate.releaseHeld()
	waitPhase(t, c.closed, "the old connection to close")
	sent := c.sentBytes()
	if len(sent) > res.sentAtReturn {
		t.Fatalf("server wrote %d bytes after RotateToken returned: %q", len(sent)-res.sentAtReturn, sent[res.sentAtReturn:])
	}
	return sent
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
	h := startRotationHarness(t, gate, ServerOptions{Controls: tc.controls})
	client := h.dial(t, tc.request)

	waitPhase(t, gate.entered, "the held write")
	if tc.beforeRotate != nil {
		tc.beforeRotate(h.srv)
	}
	res := waitRotation(t, h.rotate(), tc.wantRetired)
	_ = client.Close()
	sent := assertNothingAfter(t, gate, res)
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

func rotationListBody() string {
	return fmt.Sprintf(
		`{"jsonrpc":"2.0","id":8,"method":"tools/list","params":{"_meta":{"%s":"%s","%s":{}}}}`,
		mcpwire.VersionKey, mcpwire.Version, mcpwire.ClientCapsKey,
	)
}

// rawRequestHead builds request headers; name is the MCP name header, left
// out when empty.
func rawRequestHead(token, method, name string, contentLength int) string {
	var b strings.Builder
	b.WriteString("POST /mcp HTTP/1.1\r\nHost: 127.0.0.1:9000\r\n")
	fmt.Fprintf(&b, "Authorization: Bearer %s\r\n", token)
	b.WriteString("Content-Type: application/json\r\nAccept: application/json, text/event-stream\r\n")
	fmt.Fprintf(&b, "%s: %s\r\n%s: %s\r\n", mcpwire.ProtocolHeader, mcpwire.Version, mcpwire.MethodHeader, method)
	if name != "" {
		fmt.Fprintf(&b, "%s: %s\r\n", mcpwire.NameHeader, name)
	}
	fmt.Fprintf(&b, "Content-Length: %d\r\n\r\n", contentLength)
	return b.String()
}

func rawRotationRequest(body, methodHeader string, contentLength int) []byte {
	return []byte(rawRequestHead(testHTTPToken, methodHeader, "mesh_job_wait", contentLength) + body)
}

func rotationCallRequest(body string) []byte {
	return rawRotationRequest(body, mcpwire.MethodCallTool, len(body))
}

func newTokenListRequest() []byte {
	body := rotationListBody()
	return []byte(rawRequestHead(testHTTPTokenRotated, mcpwire.MethodListTools, "", len(body)) + body)
}

type rawResponse struct {
	status int
	// closing reports that the server will close the connection after it.
	closing bool
	body    []byte
}

func readRawResponse(br *bufio.Reader) (rawResponse, error) {
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return rawResponse{}, fmt.Errorf("read raw HTTP response: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body bytes.Buffer
	if _, err := body.ReadFrom(resp.Body); err != nil {
		return rawResponse{}, fmt.Errorf("read raw HTTP response body: %w", err)
	}
	return rawResponse{status: resp.StatusCode, closing: resp.Close, body: body.Bytes()}, nil
}

func completedWait(req JobWaitRequest) CallOutcome {
	return CallOutcome{
		Envelope: okEnvelope("sessions.wait", JobStateData{Job: req.Job.String(), State: "exited", Wait: "completed"}),
	}
}

func completingControls() *stubControls {
	return &stubControls{waitFn: func(_ context.Context, req JobWaitRequest, _ func(ProgressUpdate)) (CallOutcome, error) {
		return completedWait(req), nil
	}}
}

// blockingControls holds every wait until its request is cancelled and
// reports each start on started.
func blockingControls(started chan<- struct{}) *stubControls {
	return &stubControls{waitFn: func(ctx context.Context, _ JobWaitRequest, _ func(ProgressUpdate)) (CallOutcome, error) {
		started <- struct{}{}
		<-ctx.Done()
		return CallOutcome{}, ctx.Err()
	}}
}

func TestHTTPRotationStopsHeldJSONResult(t *testing.T) {
	runRotationCase(t, rotationCase{
		marker:      `"id":7`,
		request:     rotationCallRequest(rotationWaitBody("")),
		controls:    completingControls(),
		wantRetired: 1,
	})
}

// The SSE headers and a progress event are already on the wire when the
// terminal event's write is held.
func TestHTTPRotationStopsHeldSSEResultAfterHeadersSent(t *testing.T) {
	var gate atomic.Pointer[rotationGate]
	runRotationCase(t, rotationCase{
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
	})
}

// The progress write is held while the finished result waits in the queue;
// rotation must suppress the queued result entirely.
func TestHTTPRotationSuppressesQueuedResultBehindHeldProgress(t *testing.T) {
	var gate atomic.Pointer[rotationGate]
	sent := runRotationCase(t, rotationCase{
		marker:  "notifications/progress",
		request: rotationCallRequest(rotationWaitBody(`,"progressToken":"p7"`)),
		controls: &stubControls{waitFn: func(_ context.Context, req JobWaitRequest, notify func(ProgressUpdate)) (CallOutcome, error) {
			total := float64(req.WaitMS)
			notify(ProgressUpdate{Progress: 1, Total: &total, Message: "waiting"})
			awaitCapped(gate.Load().entered)
			return completedWait(req), nil
		}},
		// The stub returning only marks its own return; waiting for the
		// worker goroutine guarantees the result is enqueued before rotation.
		beforeRotate: func(srv *HTTPServer) { srv.workers.Wait() },
		gateRef:      &gate,
		wantRetired:  1,
	})
	if bytes.Contains(sent, []byte(`"id":7`)) {
		t.Fatalf("retired stream delivered the queued result: %q", sent)
	}
}

// A validation fault is small enough to sit in net/http's buffer until the
// flush after the handler returns.
func TestHTTPRotationStopsValidationReply(t *testing.T) {
	body := rotationWaitBody("")
	runRotationCase(t, rotationCase{
		marker:      `"jsonrpc"`,
		request:     rawRotationRequest(body, mcpwire.MethodListTools, len(body)),
		controls:    &stubControls{},
		wantRetired: anyRetired,
	})
}

func TestHTTPRotationStopsPreAdmissionFault(t *testing.T) {
	runRotationCase(t, rotationCase{
		marker:      `"jsonrpc"`,
		request:     rawRotationRequest("", mcpwire.MethodCallTool, mcpwire.MaxMessage+1),
		controls:    &stubControls{},
		wantRetired: 0,
	})
}

// net/http writes the SSE terminating chunk after the handler has returned;
// rotation must stop that write too.
func TestHTTPRotationStopsSSETerminatorAfterHandlerReturns(t *testing.T) {
	var gate atomic.Pointer[rotationGate]
	runRotationCase(t, rotationCase{
		marker:  "0\r\n\r\n",
		request: rotationCallRequest(rotationWaitBody(`,"progressToken":"p7"`)),
		controls: &stubControls{waitFn: func(_ context.Context, req JobWaitRequest, notify func(ProgressUpdate)) (CallOutcome, error) {
			total := float64(req.WaitMS)
			notify(ProgressUpdate{Progress: 1, Total: &total, Message: "waiting"})
			awaitCapped(gate.Load().firstSent)
			return completedWait(req), nil
		}},
		gateRef:     &gate,
		wantRetired: 0,
	})
}

// A request whose body never completes ends without a status from the
// handler; net/http's implicit 200 must not follow rotation.
func TestHTTPRotationStopsImplicitStatusAfterTruncatedBody(t *testing.T) {
	gate := newRotationGate("HTTP/1.1 200 OK")
	h := startRotationHarness(t, gate, ServerOptions{Controls: &stubControls{}})
	body := rotationWaitBody("")
	client := h.dial(t, []byte(rawRequestHead(testHTTPToken, mcpwire.MethodCallTool, "mesh_job_wait", len(body))+body[:len(body)/2]))
	if tcp, ok := client.(*net.TCPConn); ok {
		_ = tcp.CloseWrite()
	}
	waitPhase(t, gate.entered, "net/http's implicit status")
	res := waitRotation(t, h.rotate(), 0)
	assertNothingAfter(t, gate, res)
}

// Rotation must not leave an old request's deadline behind on a connection
// that then carries a new-token request: the old connection is closed, and a
// new-token request succeeds.
func TestHTTPRotationNeverInterruptsNewTokenRequest(t *testing.T) {
	gate := newRotationGate("")
	gate.holdDeadline = true
	started := make(chan struct{}, 1)
	h := startRotationHarness(t, gate, ServerOptions{Controls: blockingControls(started)})
	clientA := h.dial(t, rotationCallRequest(rotationWaitBody("")))
	waitPhase(t, started, "the old wait to start")
	rotated := h.rotate()

	br := bufio.NewReader(clientA)
	resp, err := readRawResponse(br)
	if err != nil || resp.closing {
		// The old connection is gone; the new token uses a fresh one.
		waitRotation(t, rotated, 1)
		assertListSucceeds(t, h.dial(t, newTokenListRequest()))
		return
	}
	// The old connection stayed open after its reply, so a client may reuse
	// it: start a new-token request there, then let any pending old deadline
	// land before the rest of the body.
	full := newTokenListRequest()
	split := bytes.Index(full, []byte("\r\n\r\n")) + 4 + 10
	if _, err := clientA.Write(full[:split]); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, gate.deadlineHeld, "rotation's deadline")
	gate.releaseDeadline()
	waitRotation(t, rotated, 1)
	if _, err := clientA.Write(full[split:]); err != nil {
		t.Fatalf("new-token request on the reused connection failed to send: %v", err)
	}
	assertListReply(t, br)
}

// A pooled client keeps its idle keep-alive connection across rotation; Go's
// client does not retry a POST on a connection the server closed under it.
func TestHTTPRotationKeepsIdleConnectionForNewToken(t *testing.T) {
	gate := newRotationGate("")
	h := startRotationHarness(t, gate, ServerOptions{Controls: &stubControls{}})
	body := rotationListBody()
	client := h.dial(t, []byte(rawRequestHead(testHTTPToken, mcpwire.MethodListTools, "", len(body))+body))
	br := bufio.NewReader(client)
	assertListReply(t, br)

	waitRotation(t, h.rotate(), 0)
	if _, err := client.Write(newTokenListRequest()); err != nil {
		t.Fatalf("new-token request on the idle connection failed to send: %v", err)
	}
	assertListReply(t, br)
}

func assertListSucceeds(t *testing.T, client net.Conn) {
	t.Helper()
	assertListReply(t, bufio.NewReader(client))
}

func assertListReply(t *testing.T, br *bufio.Reader) {
	t.Helper()
	resp, err := readRawResponse(br)
	if err != nil {
		t.Fatalf("new-token request failed: %v", err)
	}
	if resp.status != http.StatusOK || !bytes.Contains(resp.body, []byte(`"tools"`)) {
		t.Fatalf("new-token request = %d %q, want 200 with tools", resp.status, resp.body)
	}
}

// Retired exchanges still draining must not hold the admission slots the new
// token needs.
func TestHTTPRotationFreesAdmissionForNewToken(t *testing.T) {
	gate := newRotationGate(`"id":7`)
	gate.holdThroughClose = true
	h := startRotationHarness(t, gate, ServerOptions{Controls: completingControls(), MaxRequests: 1})
	h.dial(t, rotationCallRequest(rotationWaitBody("")))
	waitPhase(t, gate.entered, "the held old result")
	rotated := h.rotate()
	waitPhase(t, gate.touched, "rotation to reach the old connection")

	assertListSucceeds(t, h.dial(t, newTokenListRequest()))
	gate.releaseHeld()
	res := waitRotation(t, rotated, 1)
	assertNothingAfter(t, gate, res)
}

// A request refused for capacity must not leave its handler waiting for a
// declared body the client never sends.
func TestHTTPCapacityRejectionDoesNotWaitForBody(t *testing.T) {
	gate := newRotationGate("")
	started := make(chan struct{}, 1)
	h := startRotationHarness(t, gate, ServerOptions{Controls: blockingControls(started), MaxRequests: 1})
	h.dial(t, rotationCallRequest(rotationWaitBody("")))
	waitPhase(t, started, "the wait holding the only slot")

	client := h.dial(t, []byte(rawRequestHead(testHTTPToken, mcpwire.MethodCallTool, "mesh_job_wait", 64)))
	resp, err := readRawResponse(bufio.NewReader(client))
	if err != nil {
		t.Fatalf("capacity rejection never answered while the body was missing: %v", err)
	}
	if !bytes.Contains(resp.body, []byte(string(agentapi.ResourceLimit))) {
		t.Fatalf("capacity rejection = %d %q, want %s", resp.status, resp.body, agentapi.ResourceLimit)
	}
	waitPhase(t, gate.conn(1).closed, "the rejected connection to close")
}
