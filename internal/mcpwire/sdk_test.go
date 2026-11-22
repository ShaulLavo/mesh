package mcpwire

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type wireRoundTrip func(raw []byte) Event

func TestSDKPositivePeers(t *testing.T) {
	t.Setenv("MCPGODEBUG", "")
	for _, mode := range []string{"stdio", "HTTP JSON", "HTTP SSE"} {
		t.Run(mode, func(t *testing.T) { testSDKPositivePeer(t, mode) })
	}
}

func testSDKPositivePeer(t *testing.T, mode string) {
	t.Helper()
	sdk := newSDKFixture()
	if mode == "stdio" {
		testSDKConversation(t, sdkStdio(t, sdk))
		return
	}
	url, client := sdkHTTP(t, sdk, mode == "HTTP JSON", true)
	testSDKConversation(t, func(raw []byte) Event {
		return sdkHTTPRoundTrip(t, url, client, raw)
	})
}

func newSDKFixture() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "reference", Version: "1.8.0"},
		&mcp.ServerOptions{SupportedProtocolVersions: []string{Version}})
	server.AddTool(&mcp.Tool{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if len(req.Params.InputResponses) == 0 && !sdkPlainCall(req.Params.Arguments) {
				return &mcp.CallToolResult{RequestState: "opaque", InputRequests: mcp.InputRequestMap{
					"confirm": &mcp.ElicitParams{Message: "Confirm fixture", RequestedSchema: json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`)},
				}}, nil
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
		})
	return server
}

func sdkPlainCall(raw json.RawMessage) bool {
	var args struct {
		Plain bool `json:"plain"`
	}
	return json.Unmarshal(raw, &args) == nil && args.Plain
}

func testSDKConversation(t *testing.T, trip wireRoundTrip) {
	t.Helper()
	for _, tc := range []struct {
		id, method, caps, params, resultType string
	}{
		{"1", "server/discover", "{}", "", "complete"},
		{"2", "tools/list", "{}", "", "complete"},
		{"3", "tools/call", "{}", `,"name":"echo","arguments":{"plain":true}`, "complete"},
		{"4", "tools/call", `{"elicitation":{"form":{}}}`, `,"name":"echo"`, "input_required"},
		{"5", "tools/call", `{"elicitation":{"form":{}}}`, `,"name":"echo","requestState":"opaque","inputResponses":{"confirm":{"action":"accept","content":{"ok":true}}}`, "complete"},
	} {
		event := trip(requestBytes(tc.id, tc.method, Version, tc.caps, tc.params))
		fields, _ := decodeObject(event.Message.Result())
		result, _ := decodeString(fields["resultType"])
		if !event.Terminal || result != tc.resultType {
			t.Fatalf("%s: %s", tc.method, event.Message.Raw())
		}
		t.Logf("%s → %s", tc.method, event.Message.Raw())
	}
}

func sdkStdio(t *testing.T, server *mcp.Server) wireRoundTrip {
	t.Helper()
	clientIn, clientOut := startSDKStdio(t, server)
	reader := NewStdioReader(clientIn)
	return func(raw []byte) Event {
		t.Helper()
		m := mustParse(t, raw)
		if fault := ValidateRequest(m, fixtureMethods); fault != nil {
			t.Fatal(fault)
		}
		if err := WriteStdio(clientOut, raw); err != nil {
			t.Fatal(err)
		}
		reply, err := reader.Read()
		if err != nil {
			t.Fatal(err)
		}
		event, err := NewExchange(m, StdioReply).Accept(reply)
		if err != nil {
			t.Fatalf("SDK stdio %s: %s: %v", m.method, reply, err)
		}
		return event
	}
}

func startSDKStdio(t *testing.T, server *mcp.Server) (*io.PipeReader, *io.PipeWriter) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	serverIn, clientOut := io.Pipe()
	clientIn, serverOut := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- server.Run(ctx, &mcp.IOTransport{Reader: serverIn, Writer: serverOut})
	}()
	t.Cleanup(func() {
		cancel()
		for _, pipe := range []io.Closer{clientOut, clientIn, serverOut, serverIn} {
			_ = pipe.Close()
		}
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Error("SDK stdio fixture did not exit")
		}
	})
	stopTimeout := context.AfterFunc(ctx, func() {
		_ = clientOut.CloseWithError(ctx.Err())
		_ = clientIn.CloseWithError(ctx.Err())
	})
	t.Cleanup(func() { stopTimeout() })
	return clientIn, clientOut
}

func sdkHTTP(t *testing.T, server *mcp.Server, jsonResponse, guarded bool) (string, *http.Client) {
	t.Helper()
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		Stateless: true, JSONResponse: jsonResponse, PropagateRequestCancellation: true,
	})
	var target http.Handler = handler
	fixture := httptest.NewUnstartedServer(nil)
	if guarded {
		target = guardedSDKHTTP(handler, fixture.Listener.Addr().String())
	}
	fixture.Config.Handler = target
	fixture.Start()
	t.Cleanup(fixture.Close)
	client := fixture.Client()
	client.Timeout = 20 * time.Second
	return fixture.URL + "/mcp", client
}

func guardedSDKHTTP(handler http.Handler, authority string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status := EndpointStatus(r, authority, "/mcp", nil); status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		if r.Header.Get("Authorization") != "Bearer fixture" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		input, err := ReadHTTP(r, HTTPPolicy{Methods: fixtureMethods})
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if input.Status != http.StatusOK {
			writeHTTPFixtureFault(w, input)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(input.Message.Raw()))
		handler.ServeHTTP(w, r)
	})
}

func writeHTTPFixtureFault(w http.ResponseWriter, input HTTPInput) {
	if input.Fault == nil {
		w.WriteHeader(input.Status)
		return
	}
	w.Header().Set("Content-Type", jsonMedia)
	w.WriteHeader(input.Status)
	raw, _ := input.Fault.Reply()
	_, _ = w.Write(raw)
}

func sdkHTTPRoundTrip(t *testing.T, endpoint string, client *http.Client, raw []byte) Event {
	t.Helper()
	m := mustParse(t, raw)
	r := newFixtureHTTPRequest(t, endpoint, m)

	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	reader, err := OpenHTTPResponse(m, response)
	if err != nil {
		t.Fatal(err)
	}
	event, err := reader.Next()
	if err != nil {
		t.Fatal(err)
	}
	if !event.Terminal {
		t.Fatalf("unexpected nonterminal event: %s", event.Message.Raw())
	}
	if _, err := reader.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("end of HTTP response: %v", err)
	}
	t.Logf("POST %s %s %d %s", r.URL.Path, m.method, response.StatusCode, response.Header.Get("Content-Type"))
	return event
}

func TestSDKHTTPStreamCancellation(t *testing.T) {
	t.Setenv("MCPGODEBUG", "")
	server := newSDKFixture()
	stopped := addCancelableSDKTool(server)
	endpoint, client := sdkHTTP(t, server, false, true)
	m := sdkCancellationRequest(t)
	res, err := client.Do(newFixtureHTTPRequest(t, endpoint, m))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	reader, err := OpenHTTPResponse(m, res)
	if err != nil {
		t.Fatal(err)
	}
	if event, err := reader.Next(); err != nil || event.Terminal || event.Message.Method() != "notifications/progress" {
		t.Fatalf("stream did not open with progress: %+v, %v", event, err)
	}
	if err := res.Body.Close(); err != nil {
		t.Fatal(err)
	}
	assertSDKHandlerStopped(t, stopped)
}

func TestSDKReuseComparison(t *testing.T) {
	t.Setenv("MCPGODEBUG", "")
	server := newSDKFixture()
	endpoint, client := sdkHTTP(t, server, true, true)
	trace := &requestTrace{next: client.Transport}
	client.Transport = trace
	sdkClient := mcp.NewClient(&mcp.Implementation{Name: "reference-client", Version: "1.8.0"},
		sdkClientOptions())
	transport := &mcp.StreamableClientTransport{
		Endpoint: endpoint, HTTPClient: client, MaxRetries: -1, DisableStandaloneSSE: true,
	}
	session, err := sdkClient.Connect(t.Context(), transport, &mcp.ClientSessionOptions{ProtocolVersion: Version})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	exerciseTypedSDKClient(t, session)
	assertSDKTrace(t, trace.snapshot())
	testSDKVersionGuard(t, endpoint, client)
	testSDKVendorField(t)
}

func exerciseTypedSDKClient(t *testing.T, session *mcp.ClientSession) {
	t.Helper()
	listed, err := session.ListTools(t.Context(), nil)
	if err != nil || len(listed.Tools) != 1 || listed.Tools[0].Name != "echo" {
		t.Fatalf("SDK typed list: %v, %v", listed, err)
	}
	called, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "echo"})
	if err != nil || called.NeedsInput() || len(called.Content) != 1 {
		t.Fatalf("SDK typed MRTR call: %v, %v", called, err)
	}
}

type requestTrace struct {
	mu       sync.Mutex
	next     http.RoundTripper
	requests []string
}

func (r *requestTrace) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer fixture")
	r.mu.Lock()
	r.requests = append(r.requests, req.Method+" "+req.Header.Get(MethodHeader)+" "+req.Header.Get(ProtocolHeader))
	r.mu.Unlock()
	response, err := r.next.RoundTrip(req)
	if err != nil {
		return nil, fmt.Errorf("SDK test HTTP round trip: %w", err)
	}
	return response, nil
}

func (r *requestTrace) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.requests)
}

func assertSDKTrace(t *testing.T, trace []string) {
	t.Helper()
	want := []string{
		"POST server/discover " + Version, "POST tools/list " + Version,
		"POST tools/call " + Version, "POST tools/call " + Version,
	}
	if !slices.Equal(trace, want) {
		t.Fatalf("SDK client HTTP trace = %q, want %q", trace, want)
	}
	t.Logf("guarded typed SDK trace: %v", trace)
}

func testSDKVersionGuard(t *testing.T, endpoint string, client *http.Client) {
	t.Helper()
	raw := requestBytes("9", "server/discover", "2025-11-25", "{}", "")
	r := testPOST(raw)
	r.Header.Set(ProtocolHeader, "2025-11-25")
	r.Header.Set(MethodHeader, "server/discover")
	r.URL, _ = url.Parse(endpoint)
	r.RequestURI = ""
	r.Host = r.URL.Host
	res, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	code, err := validateRPCError(mustParse(t, body))
	if err != nil || res.StatusCode != 400 || code != UnsupportedVersion {
		t.Fatalf("guarded unsupported probe: %d %s, %v", res.StatusCode, body, err)
	}
	t.Logf("guarded unsupported probe: %d %s", res.StatusCode, body)
}

func testSDKVendorField(t *testing.T) {
	t.Helper()
	request := fixtureRequest(t, "1", "tools/list", "")
	raw := envResult("1", `{"resultType":"complete", "tools":[{"name":"echo","inputSchema":{"type":"object"},"icons":[{"src":"https://fixture.invalid/icon.svg"}],"x-vendor-shape":{"opaque":900719925474099312345}}],"ttlMs":0,"cacheScope":"private","_meta":{"org.fixture/vendor":{"kept":true}}}`)
	event, err := NewExchange(request, HTTPReply).Accept(raw)
	if err != nil {
		t.Fatal(err)
	}
	var typed mcp.ListToolsResult
	if err := json.Unmarshal(event.Message.Result(), &typed); err != nil {
		t.Fatal(err)
	}
	typedRaw, err := json.Marshal(typed)
	if err != nil {
		t.Fatal(err)
	}
	newID, _ := ParseID([]byte(`"restored"`))
	rewritten, err := event.Message.WithID(newID)
	if err != nil {
		t.Fatal(err)
	}
	rewrittenMsg := mustParse(t, rewritten)
	rewritten, err = rewrittenMsg.WithMetadata(map[string]json.RawMessage{ServerInfoKey: json.RawMessage(fixtureInfo)})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(typedRaw, []byte("x-vendor-shape")) || !bytes.Contains(typedRaw, []byte("icons")) ||
		!bytes.Contains(rewritten, []byte(`"x-vendor-shape":{"opaque":900719925474099312345}`)) ||
		!bytes.Contains(rewritten, []byte(`"org.fixture/vendor":{"kept":true}`)) {
		t.Fatalf("typed rewrite %s; raw rewrite %s", typedRaw, rewritten)
	}
	t.Logf("typed rewrite: %s", typedRaw)
	t.Logf("raw rewrite: %s", rewritten)
}

func sdkClientOptions() *mcp.ClientOptions {
	return &mcp.ClientOptions{
		Capabilities: &mcp.ClientCapabilities{},
		ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"ok": true}}, nil
		},
	}
}

type guardedStdioWriter struct {
	io.WriteCloser
	mu      sync.Mutex
	methods []string
}

func (w *guardedStdioWriter) Write(line []byte) (int, error) {
	raw, delimited := bytes.CutSuffix(line, []byte("\n"))
	input := StdioInput(raw, fixtureMethods)
	if !delimited || input.Fault != nil || input.Ignored || input.Message.Kind() != Request {
		return 0, fmt.Errorf("stdio guard refused SDK message: %q", line)
	}
	w.mu.Lock()
	w.methods = append(w.methods, input.Message.Method())
	w.mu.Unlock()
	n, err := w.WriteCloser.Write(line)
	if err != nil {
		return n, fmt.Errorf("write SDK fixture stdio line: %w", err)
	}
	return n, nil
}

func TestSDKGuardedStdioReuse(t *testing.T) {
	t.Setenv("MCPGODEBUG", "")
	in, out := startSDKStdio(t, newSDKFixture())
	guard := &guardedStdioWriter{WriteCloser: out}
	client := mcp.NewClient(&mcp.Implementation{Name: "stdio-reference-client", Version: "1.8.0"}, sdkClientOptions())
	session, err := client.Connect(t.Context(), &mcp.IOTransport{Reader: in, Writer: guard}, &mcp.ClientSessionOptions{ProtocolVersion: Version})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	exerciseTypedSDKClient(t, session)
	guard.mu.Lock()
	defer guard.mu.Unlock()
	want := []string{"server/discover", "tools/list", "tools/call", "tools/call"}
	if !slices.Equal(guard.methods, want) {
		t.Fatalf("guarded typed SDK stdio methods = %v, want %v", guard.methods, want)
	}
	t.Logf("guarded typed SDK stdio methods: %v", guard.methods)
}

// This experiment records SDK behavior. Raw fixtures specify Mesh rejection.
func TestSDKReferenceLegacyBehavior(t *testing.T) {
	t.Setenv("MCPGODEBUG", "")
	in, out := startSDKStdio(t, newSDKFixture())
	const initialize = `{"jsonrpc":"2.0","id":7,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"fixture","version":"1"}}}`
	if err := WriteStdio(out, []byte(initialize)); err != nil {
		t.Fatal(err)
	}
	raw, err := NewStdioReader(in).Read()
	if err != nil {
		t.Fatal(err)
	}
	reply := mustParse(t, raw)
	result, _ := decodeObject(reply.Result())
	if version, _ := decodeString(result["protocolVersion"]); version != "2025-11-25" {
		t.Fatalf("expected legacy SDK initialization response: %s", raw)
	}
	t.Logf("SDK restricted to %s accepted initialize: %s", Version, raw)
	endpoint, client := sdkHTTP(t, newSDKFixture(), false, false)
	request := fixtureRequest(t, "1", "tools/list", "")
	r := newFixtureHTTPRequest(t, endpoint, request)
	r.Header.Set("Last-Event-ID", "prior-event")
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != 400 || !bytes.Contains(body, []byte("Last-Event-ID")) {
		t.Fatalf("SDK stateless POST with Last-Event-ID: status=%d, body=%q, err=%v", response.StatusCode, body, err)
	}
	t.Logf("SDK stateless POST with Last-Event-ID: %d %s", response.StatusCode, body)
}

func addCancelableSDKTool(server *mcp.Server) <-chan error {
	stopped := make(chan error, 1)
	server.AddTool(&mcp.Tool{Name: "wait", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			p := &mcp.ProgressNotificationParams{ProgressToken: "track", Progress: 1}
			if err := req.Session.NotifyProgress(ctx, p); err != nil {
				stopped <- err
				return nil, fmt.Errorf("send SDK fixture progress: %w", err)
			}
			<-ctx.Done()
			stopped <- ctx.Err()
			return nil, ctx.Err()
		})
	return stopped
}

func sdkCancellationRequest(t *testing.T) Message {
	t.Helper()
	raw := requestBytes("1", "tools/call", Version, "{}", `,"name":"wait"`)
	m := mustParse(t, raw)
	updated, err := m.WithMetadata(map[string]json.RawMessage{"progressToken": json.RawMessage(`"track"`)})
	if err != nil {
		t.Fatal(err)
	}
	return mustParse(t, updated)
}

func assertSDKHandlerStopped(t *testing.T, stopped <-chan error) {
	t.Helper()
	select {
	case err := <-stopped:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("SDK request stopped with %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("cancellation did not stop the SDK handler")
	}
}

func TestSDKStdioCancellation(t *testing.T) {
	t.Setenv("MCPGODEBUG", "")
	server := newSDKFixture()
	stopped := addCancelableSDKTool(server)
	in, out := startSDKStdio(t, server)
	reader := NewStdioReader(in)
	request := sdkCancellationRequest(t)
	if err := WriteStdio(out, request.Raw()); err != nil {
		t.Fatal(err)
	}
	raw, err := reader.Read()
	if err != nil {
		t.Fatal(err)
	}
	x := NewExchange(request, StdioReply)
	event, err := x.Accept(raw)
	if err != nil || event.Terminal || event.Message.Method() != "notifications/progress" {
		t.Fatalf("stdio request did not emit progress: %s, %v", raw, err)
	}
	const cancel = `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`
	input := StdioInput([]byte(cancel), fixtureMethods)
	if input.Fault != nil || input.Ignored || !input.Cancellation.Equal(request.ID()) {
		t.Fatalf("invalid stdio cancellation: %+v", input)
	}
	if err := WriteStdio(out, []byte(cancel)); err != nil {
		t.Fatal(err)
	}
	assertSDKHandlerStopped(t, stopped)
}
