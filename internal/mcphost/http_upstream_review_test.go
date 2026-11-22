package mcphost

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/shaul/mesh/internal/mcpwire"
)

// await waits for a signal under the hang cap.
func await(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-signal:
	case <-testContext(t).Done():
		t.Fatalf("timed out waiting for %s", what)
	}
}

func awaitErr(t *testing.T, errs <-chan error) error {
	t.Helper()
	select {
	case err := <-errs:
		return err
	case <-testContext(t).Done():
		t.Fatal("timed out waiting for the call to return")
		return nil
	}
}

// releaser is a blocked sink's gate. It opens on cleanup too, so a failing
// test never leaves the call goroutine stuck.
type releaser struct {
	once sync.Once
	gate chan struct{}
}

func newReleaser(t *testing.T) *releaser {
	t.Helper()
	r := &releaser{gate: make(chan struct{})}
	t.Cleanup(r.open)
	return r
}

func (r *releaser) open() { r.once.Do(func() { close(r.gate) }) }

func bigProgress(token string, n, size int) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":%q,"progress":%d,"message":%q}}`,
		token, n, strings.Repeat("A", size))
}

func bigResult(id string, size int) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"resultType":"complete","content":[{"type":"text","text":%q}]}}`,
		id, strings.Repeat("B", size))
}

// Blocker 1: only positive evidence about parameter headers may trigger a
// schema refresh. A standard-header or unexplained -32020 is the caller's result.
func TestHeaderMismatchWithoutParameterEvidenceIsDelivered(t *testing.T) {
	messages := map[string]string{
		"standard header named": "Header mismatch: Mcp-Name header value 'foo' does not match body value 'bar'",
		"method header named":   "Mcp-Method header is missing",
		"generic":               "Header mismatch",
		"both named":            "Mcp-Name and Mcp-Param-Region do not match",
	}
	for name, message := range messages {
		t.Run(name, func(t *testing.T) {
			endpoint := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				writeJSON(w, http.StatusBadRequest, errorFrame(requestID(t, body), mcpwire.HeaderMismatch, message))
			}))
			upstream := dialUpstream(t, endpoint, nil, nil)
			events, err := callAll(t, upstream, toolCall(t, "1", "t", `{}`, ""))
			if err != nil || len(events) != 1 || mcpErrorCode(t, events[0]) != mcpwire.HeaderMismatch {
				t.Fatalf("events %d, err %v", len(events), err)
			}
		})
	}
}

// Blocker 2: session and resume metadata from a modern endpoint behind
// middleware is inert. Delivery is unchanged and nothing is echoed back.
func TestInertSessionAndResumeMetadataOnReplies(t *testing.T) {
	rec := &recorder{}
	endpoint := startServer(t, rec.handler(func(w http.ResponseWriter, r *http.Request, body []byte) {
		id := requestID(t, body)
		w.Header().Set("Mcp-Session-Id", "middleware-added")
		w.Header().Set("Last-Event-Id", "9")
		if !strings.Contains(string(body), `"sse"`) {
			writeJSON(w, http.StatusOK, okResult(id))
			return
		}
		startSSE(w)
		_, _ = io.WriteString(w, "id: 4\nretry: 100\nevent: message\ndata: "+progressFrame("p1", 1)+"\n\nid: 5\nevent: message\ndata: "+okResult(id)+"\n\n")
	}))
	upstream := dialUpstream(t, endpoint, nil, nil)
	for _, arguments := range []string{`{}`, `{"mode":"sse"}`} {
		events, err := callAll(t, upstream, toolCall(t, "1", "echo", arguments, `"progressToken":"p1"`))
		if err != nil || terminals(events) != 1 {
			t.Fatalf("%s: %d events, %v", arguments, len(events), err)
		}
	}
	for i, got := range rec.all() {
		if headerSet(got.Header, "Mcp-Session-Id") || headerSet(got.Header, "Last-Event-Id") {
			t.Errorf("request %d carried session or resume metadata: %v", i, got.Header)
		}
	}
}

// Blocker 3: queued plus in-delivery bytes stay within the budget, with the
// terminal result held to it too.
func TestByteBudgetCoversQueuedInDeliveryAndTerminal(t *testing.T) {
	const size = 3 << 20
	stopped := make(chan struct{})
	endpoint := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		startSSE(w)
		writeFrames(w, bigProgress("p1", 1, size), bigProgress("p1", 2, size), bigResult(requestID(t, body), size))
		<-r.Context().Done()
		close(stopped)
	}))
	upstream := dialUpstream(t, endpoint, nil, nil)
	gate, blocked := newReleaser(t), make(chan struct{})
	var once sync.Once
	errs := make(chan error, 1)
	go func() {
		errs <- upstream.Call(testContext(t), toolCall(t, "1", "slow", `{}`, `"progressToken":"p1"`), func(mcpwire.Event) error {
			once.Do(func() { close(blocked) })
			<-gate.gate
			return nil
		})
	}()
	await(t, blocked, "the first event")
	await(t, stopped, "the server to see the call fail")
	gate.open()
	if err := awaitErr(t, errs); !errors.Is(err, ErrSlowConsumer) {
		t.Fatalf("err %v", err)
	}
}

func TestQueueReleasesDequeuedPayloads(t *testing.T) {
	q := newEventQueue()
	m, fault := mcpwire.Parse([]byte(progressFrame("p1", 1)))
	if fault != nil {
		t.Fatal(fault)
	}
	if !q.push(mcpwire.Event{Message: m}) {
		t.Fatal("push refused")
	}
	if _, err := q.take(testContext(t)); err != nil {
		t.Fatal(err)
	}
	for i, slot := range q.events[:cap(q.events)] {
		if slot.Message.Raw() != nil {
			t.Errorf("slot %d still references a delivered payload", i)
		}
	}
	if q.bytes == 0 {
		t.Error("an event in delivery must still count against the budget")
	}
	q.release(mcpwire.Event{Message: m})
	if q.bytes != 0 {
		t.Errorf("budget not released: %d", q.bytes)
	}
}

// Blocker 4: the reader closes the stream when it is done, whatever the sink does.
func TestStreamClosesAtTerminalWhileSinkIsBlocked(t *testing.T) {
	closed := make(chan struct{})
	endpoint := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		startSSE(w)
		writeFrames(w, okResult(requestID(t, body)))
		<-r.Context().Done()
		close(closed)
	}))
	upstream := dialUpstream(t, endpoint, nil, nil)
	gate, blocked := newReleaser(t), make(chan struct{})
	errs := make(chan error, 1)
	go func() {
		errs <- upstream.Call(testContext(t), toolCall(t, "1", "echo", `{}`, ""), func(mcpwire.Event) error {
			close(blocked)
			<-gate.gate
			return nil
		})
	}()
	await(t, blocked, "the terminal event")
	await(t, closed, "the server to see the stream close")
	gate.open()
	if err := awaitErr(t, errs); err != nil {
		t.Fatalf("err %v", err)
	}
}

// Blocker 5: peer-controlled text in malformed HTTP never reaches a diagnostic.
func TestMalformedResponsesDoNotLeakCredentials(t *testing.T) {
	const secret = "fixture-secret"
	replies := map[string]string{
		"status line": "HTTP/1.1 " + secret + "\r\n\r\n",
		"status code": "HTTP/1.1 2" + secret + " OK\r\n\r\n",
		"header line": "HTTP/1.1 200 OK\r\nBad Header Bearer " + secret + "\r\n\r\n",
		"version":     "HTTP/9 200 " + secret + "\r\n\r\n",
	}
	for name, reply := range replies {
		t.Run(name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener.Close() })
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer func() { _ = conn.Close() }()
				buf := make([]byte, 1<<16)
				_, _ = conn.Read(buf)
				_, _ = io.WriteString(conn, reply)
			}()
			upstream := dialUpstream(t, "http://"+listener.Addr().String()+"/mcp", bearer(), nil)
			_, err = callAll(t, upstream, toolCall(t, "1", "echo", `{}`, ""))
			if err == nil || !errors.Is(err, ErrOutcomeUnknown) || !errors.Is(err, ErrMalformedResponse) {
				t.Fatalf("err %v", err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("diagnostic leaks peer text: %v", err)
			}
		})
	}
}
