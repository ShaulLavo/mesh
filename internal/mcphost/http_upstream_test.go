package mcphost

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/shaul/mesh/internal/mcpwire"
)

func TestJSONReplyAndRequestShape(t *testing.T) {
	rec := &recorder{}
	endpoint := startServer(t, rec.handler(func(w http.ResponseWriter, _ *http.Request, body []byte) {
		writeJSON(w, http.StatusOK, okResult(requestID(t, body)))
	}))
	upstream := dialUpstream(t, endpoint, bearer(), nil)
	m := toolCall(t, `"h-7"`, "echo", `{}`, "")

	events, err := callAll(t, upstream, m)
	if err != nil || len(events) != 1 || !events[0].Terminal {
		t.Fatalf("events %v, err %v", events, err)
	}
	got := rec.all()
	if len(got) != 1 {
		t.Fatalf("requests: %d", len(got))
	}
	h := got[0].Header
	if got[0].Method != http.MethodPost || got[0].Path != "/mcp" || string(got[0].Body) != string(m.Raw()) {
		t.Fatalf("request line or body: %+v", got[0])
	}
	if !containsAll(strings.Join(h.Values("Accept"), ","), "application/json", "text/event-stream") ||
		h.Get("Content-Type") != "application/json" || h.Get(mcpwire.ProtocolHeader) != mcpwire.Version ||
		h.Get(mcpwire.MethodHeader) != "tools/call" || h.Get(mcpwire.NameHeader) != "echo" ||
		h.Get("Authorization") != "Bearer fixture-secret" {
		t.Fatalf("headers: %v", h)
	}
	for _, name := range []string{"Mcp-Session-Id", "Last-Event-Id", "Origin", "Cookie", "Accept-Encoding"} {
		if headerSet(h, name) {
			t.Errorf("unexpected %s header: %v", name, h)
		}
	}
	if !strings.HasPrefix(got[0].Host, "127.0.0.1:") {
		t.Errorf("Host %q", got[0].Host)
	}
}

func TestSSENotificationsThenTerminal(t *testing.T) {
	endpoint := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		startSSE(w)
		writeFrames(w, progressFrame("p1", 1), progressFrame("p1", 2), okResult(requestID(t, body)))
	}))
	upstream := dialUpstream(t, endpoint, nil, nil)

	events, err := callAll(t, upstream, toolCall(t, "1", "echo", `{}`, `"progressToken":"p1"`))
	if err != nil || len(events) != 3 || terminals(events) != 1 || !events[2].Terminal {
		t.Fatalf("events %d, err %v", len(events), err)
	}
	if events[0].Message.Method() != "notifications/progress" {
		t.Fatalf("first event: %s", events[0].Message.Raw())
	}
}

func TestInterruptedStreams(t *testing.T) {
	for name, serve := range map[string]func(http.ResponseWriter, string){
		"closed after progress": func(w http.ResponseWriter, _ string) { writeFrames(w, progressFrame("p1", 1)) },
		"cut mid event":         func(w http.ResponseWriter, _ string) { _, _ = io.WriteString(w, "event: message\ndata: {\"jsonrpc\":") },
		"empty stream":          func(http.ResponseWriter, string) {},
	} {
		t.Run(name, func(t *testing.T) {
			endpoint := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				startSSE(w)
				serve(w, requestID(t, body))
			}))
			upstream := dialUpstream(t, endpoint, nil, nil)
			events, err := callAll(t, upstream, toolCall(t, "1", "echo", `{}`, `"progressToken":"p1"`))
			if !errors.Is(err, ErrOutcomeUnknown) || !errors.Is(err, mcpwire.ErrInterrupted) || terminals(events) != 0 {
				t.Fatalf("events %d, err %v", len(events), err)
			}
			if name == "closed after progress" && len(events) != 1 {
				t.Fatalf("the notification before the cut was lost: %d", len(events))
			}
		})
	}
}

func TestWrongIDsAndShapesAreRejected(t *testing.T) {
	cases := map[string]struct {
		sse   bool
		frame func(id string) string
	}{
		"JSON wrong ID": {frame: func(string) string { return okResult("99") }},
		"SSE wrong ID":  {sse: true, frame: func(string) string { return okResult("99") }},
		"string for number": {sse: true, frame: func(string) string {
			return okResult(`"1"`)
		}},
		"server request": {sse: true, frame: func(string) string {
			return `{"jsonrpc":"2.0","id":5,"method":"sampling/createMessage","params":{}}`
		}},
		"result without type": {frame: func(id string) string {
			return `{"jsonrpc":"2.0","id":` + id + `,"result":{"content":[]}}`
		}},
		"foreign progress token": {sse: true, frame: func(string) string { return progressFrame("other", 1) }},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			endpoint := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if !tc.sse {
					writeJSON(w, http.StatusOK, tc.frame(requestID(t, body)))
					return
				}
				startSSE(w)
				writeFrames(w, tc.frame(requestID(t, body)))
			}))
			upstream := dialUpstream(t, endpoint, nil, nil)
			events, err := callAll(t, upstream, toolCall(t, "1", "echo", `{}`, `"progressToken":"p1"`))
			if !errors.Is(err, mcpwire.ErrPeerMessage) || !errors.Is(err, ErrOutcomeUnknown) || len(events) != 0 {
				t.Fatalf("events %d, err %v", len(events), err)
			}
		})
	}
}

func TestExtraTerminalResult(t *testing.T) {
	endpoint := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		id := requestID(t, body)
		startSSE(w)
		writeFrames(w, okResult(id), okResult(id))
	}))
	upstream := dialUpstream(t, endpoint, nil, nil)
	events, err := callAll(t, upstream, toolCall(t, "1", "echo", `{}`, ""))
	if err != nil || terminals(events) != 1 {
		t.Fatalf("the caller must see exactly one terminal: %d events, err %v", len(events), err)
	}

	// The strict reader sees the stray frame, so a caller that wants the
	// check can have it.
	endpoint2, _ := parseHTTPEndpoint(endpoint)
	client := newHTTPClient(endpoint2, nil)
	stream, err := client.do(testContext(t), httpCall{request: toolCall(t, "1", "echo", `{}`, "")})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.close()
	if event, err := stream.next(); err != nil || !event.Terminal {
		t.Fatalf("first: %v %v", event, err)
	}
	if _, err := stream.next(); !errors.Is(err, mcpwire.ErrPeerMessage) {
		t.Fatalf("second terminal: %v", err)
	}
}

func TestLegacySessionAndResumeSignalsAreRefusedAndNeverSent(t *testing.T) {
	var calls atomic.Int32
	rec := &recorder{}
	endpoint := startServer(t, rec.handler(func(w http.ResponseWriter, r *http.Request, body []byte) {
		switch calls.Add(1) {
		case 1:
			w.Header().Set("Mcp-Session-Id", "abc")
			w.Header().Set("Last-Event-Id", "9")
			writeJSON(w, http.StatusOK, okResult(requestID(t, body)))
		case 2:
			startSSE(w)
			_, _ = io.WriteString(w, "id: 4\nevent: message\ndata: "+okResult(requestID(t, body))+"\n\n")
		default:
			writeJSON(w, http.StatusOK, okResult(requestID(t, body)))
		}
	}))
	upstream := dialUpstream(t, endpoint, nil, nil)
	for range 2 {
		_, err := callAll(t, upstream, toolCall(t, "1", "echo", `{}`, ""))
		if !errors.Is(err, mcpwire.ErrPeerMessage) {
			t.Fatalf("legacy signal accepted: %v", err)
		}
	}
	if events, err := callAll(t, upstream, toolCall(t, "1", "echo", `{}`, "")); err != nil || terminals(events) != 1 {
		t.Fatalf("plain reply: %v", err)
	}
	for i, got := range rec.all() {
		if headerSet(got.Header, "Mcp-Session-Id") || headerSet(got.Header, "Last-Event-Id") {
			t.Errorf("request %d carried a session or resume header: %v", i, got.Header)
		}
	}
}

func TestStatusAndEncodingFailures(t *testing.T) {
	for name, serve := range map[string]func(w http.ResponseWriter){
		"unauthorized": func(w http.ResponseWriter) { w.WriteHeader(http.StatusUnauthorized) },
		"not allowed":  func(w http.ResponseWriter) { w.WriteHeader(http.StatusMethodNotAllowed) },
		"server error": func(w http.ResponseWriter) { http.Error(w, "boom", http.StatusInternalServerError) },
		"accepted":     func(w http.ResponseWriter) { w.WriteHeader(http.StatusAccepted) },
		"coded body": func(w http.ResponseWriter) {
			w.Header().Set("Content-Encoding", "gzip")
			writeJSON(w, http.StatusOK, "{}")
		},
	} {
		t.Run(name, func(t *testing.T) {
			endpoint := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { serve(w) }))
			upstream := dialUpstream(t, endpoint, nil, nil)
			_, err := callAll(t, upstream, toolCall(t, "1", "echo", `{}`, ""))
			var status *HTTPStatusError
			if !errors.Is(err, ErrOutcomeUnknown) || !errors.As(err, &status) {
				t.Fatalf("err %v", err)
			}
		})
	}
}

func TestRedirectIsRefusedAndCredentialsStayHome(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		elsewhere.Add(1)
		writeJSON(w, http.StatusOK, "{}")
	}))
	t.Cleanup(other.Close)
	endpoint := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	upstream := dialUpstream(t, endpoint, bearer(), nil)
	_, err := callAll(t, upstream, toolCall(t, "1", "echo", `{}`, ""))
	var status *HTTPStatusError
	if !errors.As(err, &status) || !errors.Is(err, ErrRedirect) || status.Status != http.StatusTemporaryRedirect {
		t.Fatalf("err %v", err)
	}
	if elsewhere.Load() != 0 {
		t.Fatal("the redirect target received a request")
	}
	if strings.Contains(err.Error(), "fixture-secret") {
		t.Fatalf("error leaks the credential: %v", err)
	}
}

func TestEnvironmentProxyIsIgnored(t *testing.T) {
	// A closed port as the proxy: any use of it fails the call.
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("http_proxy", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	endpoint := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		writeJSON(w, http.StatusOK, okResult(requestID(t, body)))
	}))
	upstream := dialUpstream(t, strings.Replace(endpoint, "127.0.0.1", "localhost", 1), nil, nil)
	if upstream.(*httpUpstream).client.transport.Proxy != nil {
		t.Fatal("the transport may consult a proxy")
	}
	if events, err := callAll(t, upstream, toolCall(t, "1", "echo", `{}`, "")); err != nil || terminals(events) != 1 {
		t.Fatalf("call: %v", err)
	}
}

func TestNonLoopbackEndpointsAreRefused(t *testing.T) {
	for _, endpoint := range []string{
		"http://192.0.2.1:8080/mcp", "http://example.com/mcp", "https://127.0.0.1:8080/mcp",
		"http://127.0.0.2:8080/mcp", "http://[::ffff:127.0.0.1]:8080/mcp", "http://localhost.example.com/mcp",
		"http://user:hunter2@127.0.0.1:8080/mcp", "http://127.0.0.1:0/mcp", "http://127.0.0.1:99999/mcp",
		"http://127.0.0.1:8080/mcp#frag", "http://0.0.0.0:8080/mcp", "http://[::1%25lo]:8080/mcp", "127.0.0.1:8080/mcp", "",
	} {
		_, err := DialHTTP(HTTPUpstreamConfig{URL: endpoint})
		if err == nil {
			t.Errorf("accepted %q", endpoint)
		} else if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("error leaks userinfo: %v", err)
		}
	}
	for _, endpoint := range []string{"http://127.0.0.1:8080/mcp", "http://[::1]:8080/mcp", "http://localhost:8080/mcp", "http://LOCALHOST/mcp?x=1"} {
		upstream, err := DialHTTP(HTTPUpstreamConfig{URL: endpoint})
		if err != nil {
			t.Errorf("refused %q: %v", endpoint, err)
			continue
		}
		_ = upstream.Close()
	}
}

func TestDialIsBoundToTheApprovedAuthority(t *testing.T) {
	endpoint, err := parseHTTPEndpoint("http://127.0.0.1:8080/mcp")
	if err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{"192.0.2.1:8080", "127.0.0.1:8081", "localhost:8080", "[::1]:8080"} {
		if _, err := endpoint.dial(testContext(t), "tcp", address); !errors.Is(err, ErrNotSent) {
			t.Errorf("dial %s: %v", address, err)
		}
	}
}

func TestLocalhostReachesEitherLoopbackFamily(t *testing.T) {
	serve := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		writeJSON(w, http.StatusOK, okResult(requestID(t, body)))
	})
	for name, listen := range map[string]string{"IPv4": "127.0.0.1:0", "IPv6 only": "[::1]:0"} {
		t.Run(name, func(t *testing.T) {
			listener, err := net.Listen("tcp", listen)
			if err != nil {
				t.Skipf("cannot listen on %s: %v", listen, err)
			}
			server := httptest.NewUnstartedServer(serve)
			server.Listener = listener
			server.Start()
			t.Cleanup(server.Close)
			_, port, _ := net.SplitHostPort(listener.Addr().String())
			upstream := dialUpstream(t, "http://localhost:"+port+"/mcp", nil, nil)
			if events, err := callAll(t, upstream, toolCall(t, "1", "echo", `{}`, "")); err != nil || terminals(events) != 1 {
				t.Fatalf("call: %v", err)
			}
		})
	}
}

func TestCredentialHeadersAreChecked(t *testing.T) {
	bad := []http.Header{
		{"Host": {"evil"}}, {"Mcp-Name": {"x"}}, {"Mcp-Param-Region": {"x"}}, {"Content-Type": {"text/plain"}},
		{"Connection": {"close"}}, {"Transfer-Encoding": {"chunked"}}, {"Proxy-Authorization": {"x"}},
		{"X-Forwarded-For": {"x"}}, {"Origin": {"http://x"}}, {"Last-Event-Id": {"1"}}, {"Bad Name": {"x"}},
		{"Authorization": {"a\r\nX-Evil: 1"}}, {"Authorization": {""}}, {"Authorization": {" padded"}},
		{"Authorization": {"one", "two"}}, {"Authorization": {"nul\x00"}},
		{"Authorization": {"a"}, "authorization": {"b"}},
	}
	for _, headers := range bad {
		if upstream, err := DialHTTP(HTTPUpstreamConfig{URL: "http://127.0.0.1:8080/mcp", Headers: headers}); err == nil {
			_ = upstream.Close()
			t.Errorf("accepted %v", headers)
		}
	}
	good := http.Header{"Authorization": {"Bearer a b"}, "X-Api-Key": {"k\tv"}, "Cookie": {"s=1"}}
	upstream, err := DialHTTP(HTTPUpstreamConfig{URL: "http://127.0.0.1:8080/mcp", Headers: good})
	if err != nil {
		t.Fatal(err)
	}
	_ = upstream.Close()
}

func TestRequestsOnlyAndClosedUpstream(t *testing.T) {
	var hits atomic.Int32
	endpoint := startServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	upstream := dialUpstream(t, endpoint, nil, nil)
	note, fault := mcpwire.Parse([]byte(progressFrame("p", 1)))
	if fault != nil {
		t.Fatal(fault)
	}
	if err := upstream.Call(testContext(t), note, func(mcpwire.Event) error { return nil }); !errors.Is(err, ErrNotSent) {
		t.Fatalf("notification: %v", err)
	}
	if err := upstream.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-upstream.Done():
	default:
		t.Fatal("Done is open after Close")
	}
	if upstream.Err() == nil {
		t.Fatal("Err is nil after Close")
	}
	err := upstream.Call(testContext(t), toolCall(t, "1", "echo", `{}`, ""), func(mcpwire.Event) error { return nil })
	if !errors.Is(err, ErrNotSent) || hits.Load() != 0 {
		t.Fatalf("call after close: %v, hits %d", err, hits.Load())
	}
}

func TestSinkErrorIsReturnedAsIs(t *testing.T) {
	endpoint := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		writeJSON(w, http.StatusOK, okResult(requestID(t, body)))
	}))
	upstream := dialUpstream(t, endpoint, nil, nil)
	boom := errors.New("sink refused")
	err := upstream.Call(testContext(t), toolCall(t, "1", "echo", `{}`, ""), func(mcpwire.Event) error { return boom })
	if !errors.Is(err, boom) || errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("err %v", err)
	}
}

func TestOversizeReplies(t *testing.T) {
	big := strings.Repeat("A", mcpwire.MaxMessage)
	jsonBody := func(id string) string {
		return `{"jsonrpc":"2.0","id":` + id + `,"result":{"resultType":"complete","content":[{"type":"text","text":"` + big + `"}]}}`
	}
	for name, sse := range map[string]bool{"JSON": false, "SSE": true} {
		t.Run(name, func(t *testing.T) {
			endpoint := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if !sse {
					writeJSON(w, http.StatusOK, jsonBody(requestID(t, body)))
					return
				}
				startSSE(w)
				writeFrames(w, jsonBody(requestID(t, body)))
			}))
			upstream := dialUpstream(t, endpoint, nil, nil)
			events, err := callAll(t, upstream, toolCall(t, "1", "echo", `{}`, ""))
			if !errors.Is(err, mcpwire.ErrTooLarge) || len(events) != 0 {
				t.Fatalf("events %d, err %v", len(events), err)
			}
		})
	}
}

func TestOversizeHeaderBlockFails(t *testing.T) {
	endpoint := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Padding", strings.Repeat("a", maxUpstreamResponseHeader*2))
		writeJSON(w, http.StatusOK, "{}")
	}))
	upstream := dialUpstream(t, endpoint, nil, nil)
	if _, err := callAll(t, upstream, toolCall(t, "1", "echo", `{}`, "")); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("err %v", err)
	}
}

func TestCancellationClosesTheResponseStream(t *testing.T) {
	cases := map[string]func(w http.ResponseWriter, started chan<- struct{}){
		"after progress": func(w http.ResponseWriter, started chan<- struct{}) {
			startSSE(w)
			writeFrames(w, progressFrame("p1", 1))
			started <- struct{}{}
		},
		"before headers": func(_ http.ResponseWriter, started chan<- struct{}) { started <- struct{}{} },
	}
	for name, serve := range cases {
		t.Run(name, func(t *testing.T) {
			started, stopped := make(chan struct{}, 1), make(chan struct{})
			endpoint := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				serve(w, started)
				<-r.Context().Done()
				close(stopped)
			}))
			upstream := dialUpstream(t, endpoint, nil, nil)
			ctx, cancel := context.WithCancel(testContext(t))
			defer cancel()
			var delivered atomic.Int32
			errs := make(chan error, 1)
			go func() {
				errs <- upstream.Call(ctx, toolCall(t, "1", "echo", `{}`, `"progressToken":"p1"`), func(mcpwire.Event) error {
					delivered.Add(1)
					return nil
				})
			}()
			<-started
			cancel()
			if err := <-errs; !errors.Is(err, context.Canceled) {
				t.Fatalf("err %v", err)
			}
			<-stopped
		})
	}
}

func TestCloseCancelsCallsInFlight(t *testing.T) {
	started, stopped := make(chan struct{}, 1), make(chan struct{})
	endpoint := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		started <- struct{}{}
		<-r.Context().Done()
		close(stopped)
	}))
	upstream := dialUpstream(t, endpoint, nil, nil)
	errs := make(chan error, 1)
	go func() {
		errs <- upstream.Call(testContext(t), toolCall(t, "1", "echo", `{}`, ""), func(mcpwire.Event) error { return nil })
	}()
	<-started
	_ = upstream.Close()
	if err := <-errs; !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("err %v", err)
	}
	<-stopped
}

func TestSlowConsumerFailsOnlyItsCall(t *testing.T) {
	stopped := make(chan struct{})
	endpoint := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"fast"`) {
			writeJSON(w, http.StatusOK, okResult(requestID(t, body)))
			return
		}
		startSSE(w)
		for i := range httpCallQueueEvents + 50 {
			writeFrames(w, progressFrame("p1", i))
			if r.Context().Err() != nil {
				break
			}
		}
		<-r.Context().Done()
		close(stopped)
	}))
	upstream := dialUpstream(t, endpoint, nil, nil)
	release := make(chan struct{})
	errs := make(chan error, 1)
	var once atomic.Bool
	go func() {
		errs <- upstream.Call(testContext(t), toolCall(t, "1", "slow", `{}`, `"progressToken":"p1"`), func(mcpwire.Event) error {
			if once.CompareAndSwap(false, true) {
				<-release
			}
			return nil
		})
	}()
	// The stuck sink does not hold the connection or other calls.
	if events, err := callAll(t, upstream, toolCall(t, "2", "fast", `{}`, "")); err != nil || terminals(events) != 1 {
		t.Fatalf("other call: %v", err)
	}
	<-stopped
	close(release)
	if err := <-errs; !errors.Is(err, ErrSlowConsumer) {
		t.Fatalf("err %v", err)
	}
}
