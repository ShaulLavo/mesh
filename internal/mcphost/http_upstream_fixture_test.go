package mcphost

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/mcpwire"
)

// hangCap is the only time limit in these tests: it catches a stuck call.
const hangCap = 30 * time.Second

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), hangCap)
	t.Cleanup(cancel)
	return ctx
}

// request builds a modern request. meta adds members to params._meta and
// params adds members to params; both are raw JSON object bodies without braces.
func request(t *testing.T, id, method, meta, params string) mcpwire.Message {
	t.Helper()
	body := fmt.Sprintf(`"%s":"%s","%s":{}`, mcpwire.VersionKey, mcpwire.Version, mcpwire.ClientCapsKey)
	if meta != "" {
		body += "," + meta
	}
	raw := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":%q,"params":{"_meta":{%s}`, id, method, body)
	if params != "" {
		raw += "," + params
	}
	m, fault := mcpwire.Parse([]byte(raw + "}}"))
	if fault != nil {
		t.Fatalf("fixture request: %v", fault)
	}
	return m
}

func toolCall(t *testing.T, id, name, arguments, meta string) mcpwire.Message {
	t.Helper()
	return request(t, id, mcpwire.MethodCallTool, meta, fmt.Sprintf(`"name":%q,"arguments":%s`, name, arguments))
}

func okResult(id string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"resultType":"complete","content":[{"type":"text","text":"ok"}]}}`, id)
}

func progressFrame(token string, n int) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":%q,"progress":%d}}`, token, n)
}

func errorFrame(id string, code int, message string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":%d,"message":%q}}`, id, code, message)
}

// requestID returns the raw JSON ID of a POSTed request.
func requestID(t *testing.T, body []byte) string {
	t.Helper()
	m, fault := mcpwire.Parse(body)
	if fault != nil {
		t.Errorf("fixture received a malformed request: %v", fault)
		return "null"
	}
	raw, err := m.ID().MarshalJSON()
	if err != nil {
		t.Errorf("fixture request ID: %v", err)
	}
	return string(raw)
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func startSSE(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	w.(http.Flusher).Flush()
}

func writeFrames(w http.ResponseWriter, frames ...string) {
	for _, frame := range frames {
		_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", frame)
	}
	w.(http.Flusher).Flush()
}

type recordedRequest struct {
	Header http.Header
	Body   []byte
	Host   string
	Method string
	Path   string
}

type recorder struct {
	mu       sync.Mutex
	requests []recordedRequest
}

func (r *recorder) handler(next func(w http.ResponseWriter, req *http.Request, body []byte)) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.requests = append(r.requests, recordedRequest{Header: req.Header.Clone(), Body: body, Host: req.Host, Method: req.Method, Path: req.URL.Path})
		r.mu.Unlock()
		next(w, req, body)
	}
}

func (r *recorder) all() []recordedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedRequest(nil), r.requests...)
}

func startServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server.URL + "/mcp"
}

func dialUpstream(t *testing.T, endpoint string, headers http.Header, tools func(string) (mcpwire.ParameterHeaders, bool)) Upstream {
	t.Helper()
	upstream, err := DialHTTP(HTTPUpstreamConfig{URL: endpoint, Headers: headers, ToolHeaders: tools})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = upstream.Close() })
	return upstream
}

func bearer() http.Header { return http.Header{"Authorization": {"Bearer fixture-secret"}} }

func callAll(t *testing.T, upstream Upstream, m mcpwire.Message) ([]mcpwire.Event, error) {
	t.Helper()
	var events []mcpwire.Event
	err := upstream.Call(testContext(t), m, func(event mcpwire.Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		return events, fmt.Errorf("call %s: %w", m.Method(), err)
	}
	return events, nil
}

func terminals(events []mcpwire.Event) int {
	count := 0
	for _, event := range events {
		if event.Terminal {
			count++
		}
	}
	return count
}

func headerSet(h http.Header, name string) bool { return len(h.Values(name)) > 0 }

func containsAll(value string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(value, part) {
			return false
		}
	}
	return true
}
