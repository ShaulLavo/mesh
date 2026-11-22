package mcpwire

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPRequestMatrix(t *testing.T) {
	methods := Methods{
		"server/discover": nil, "tools/call": {"elicitation": map[string]any{}}, "resources/read": nil, "prompts/get": nil,
	}
	schema, err := CompileParameterHeaders(json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","x-mcp-header":"Query"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	policy := HTTPPolicy{Methods: methods, ToolHeaders: func(string) ParameterHeaders { return schema }}
	for _, tc := range []struct {
		name, method, version, headerVersion, caps, params string
		mutate                                             func(http.Header)
		status, code                                       int
	}{
		{"discover", "server/discover", Version, Version, "{}", "", nil, 200, 0},
		{"missing version header", "server/discover", Version, "", "{}", "", nil, 400, HeaderMismatch},
		{"mismatched version", "server/discover", "old", Version, "{}", "", nil, 400, HeaderMismatch},
		{"malformed version", "server/discover", "old, old", "old, old", "{}", "", nil, 400, HeaderMismatch},
		{"unsupported version", "server/discover", "old", "old", "{}", "", nil, 400, UnsupportedVersion},
		{"unsupported before invalid capability metadata", "server/discover", "old", "old", "null", "", nil, 400, UnsupportedVersion},
		{"missing method header", "server/discover", Version, Version, "{}", "", removeHeader(MethodHeader), 400, HeaderMismatch},
		{"method mismatch", "server/discover", Version, Version, "{}", "", setHeader(MethodHeader, "SERVER/discover"), 400, HeaderMismatch},
		{"duplicate method header", "server/discover", Version, Version, "{}", "", func(h http.Header) { h["mcp-method"] = []string{"server/discover"} }, 400, HeaderMismatch},
		{"missing request metadata", "server/discover", Version, Version, "null", "", nil, 400, InvalidParams},
		{"unknown with modern headers", "vendor/unknown", Version, Version, "{}", "", nil, 404, MethodNotFound},
		{"legacy with modern headers", "initialize", Version, Version, "{}", "", nil, 404, MethodNotFound},
		{"legacy without modern headers", "initialize", "old", "", "{}", "", removeHeader(MethodHeader), 400, HeaderMismatch},
		{"accepted types missing", "server/discover", Version, Version, "{}", "", setHeader("Accept", "application/json, text/event-stream;q=0"), 406, 0},
		{"missing name", "tools/call", Version, Version, "{}", `,"name":"tool"`, nil, 400, HeaderMismatch},
		{"missing capability", "tools/call", Version, Version, "{}", `,"name":"tool"`, setHeader(NameHeader, "tool"), 400, MissingCapability},
		{"missing mirrored argument", "tools/call", Version, Version, `{"elicitation":{}}`, `,"name":"tool","arguments":{"query":"ok"}`, setHeader(NameHeader, "tool"), 400, HeaderMismatch},
		{"malformed name sentinel", "resources/read", Version, Version, "{}", `,"uri":"mesh://a"`, setHeader(NameHeader, "=?base64?%%?="), 400, HeaderMismatch},
		{"resource URI", "resources/read", Version, Version, "{}", `,"uri":"mesh://資源"`, setHeader(NameHeader, EncodeHeaderValue("mesh://資源")), 200, 0},
		{"prompt name", "prompts/get", Version, Version, "{}", `,"name":"prompt"`, setHeader(NameHeader, "prompt"), 200, 0},
	} {
		raw := requestBytes(`"request"`, tc.method, tc.version, tc.caps, tc.params)
		r := testPOST(raw)
		r.Header.Set(MethodHeader, tc.method)
		if tc.headerVersion != "" {
			r.Header.Set(ProtocolHeader, tc.headerVersion)
		}
		if tc.mutate != nil {
			tc.mutate(r.Header)
		}
		t.Run(tc.name, func(t *testing.T) {
			in, err := ReadHTTP(r, policy)
			assertHTTPInput(t, in, err, tc.status, tc.code)
		})
	}
}

func setHeader(key, value string) func(http.Header) {
	return func(h http.Header) { h.Set(key, value) }
}

func removeHeader(key string) func(http.Header) {
	return func(h http.Header) { h.Del(key) }
}

func testPOST(raw []byte) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(raw))
	r.Header.Set("Content-Type", jsonMedia)
	r.Header.Set("Accept", jsonMedia+", "+sseMedia)
	return r
}

func assertHTTPInput(t *testing.T, in HTTPInput, err error, status, code int) {
	t.Helper()
	if err != nil || in.Status != status {
		t.Fatalf("HTTP input = %+v, err = %v, expected status %d", in, err, status)
	}
	if code == 0 && in.Fault == nil {
		return
	}
	if in.Fault == nil || in.Fault.Code != code {
		t.Fatalf("fault = %+v, expected code %d", in.Fault, code)
	}
	raw, err := in.Fault.Reply()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := validateRPCError(mustParse(t, raw)); err != nil || got != code {
		t.Fatalf("invalid JSON-RPC fault body %s: %v", raw, err)
	}
}

func TestHTTPEndpoint(t *testing.T) {
	for _, tc := range []struct {
		method, path, authority, origin string
		status                          int
	}{
		{"POST", "/mcp", "127.0.0.1:9000", "", 200},
		{"POST", "/mesh/mcp", "127.0.0.1:9000", "", 200},
		{"POST", "/mc%70", "127.0.0.1:9000", "", 404},
		{"POST", "/mcp?session=7", "127.0.0.1:9000", "", 404},
		{"POST", "/mcp", "localhost:9000", "", 403},
		{"POST", "/mcp", "127.0.0.1:9000", "null", 403},
		{"POST", "/mcp", "127.0.0.1:9000", "https://allowed.test", 200},
		{"GET", "/mcp", "127.0.0.1:9000", "https://wrong.test", 403},
		{"GET", "/mcp", "127.0.0.1:9000", "", 405},
		{"DELETE", "/mcp", "127.0.0.1:9000", "", 405},
	} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		r.Host = tc.authority
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		endpoint := "/mcp"
		if tc.path == "/mesh/mcp" {
			endpoint = "/mesh/mcp"
		}
		got := EndpointStatus(r, "127.0.0.1:9000", endpoint, func(value string) bool { return value == "https://allowed.test" })
		if got != tc.status {
			t.Errorf("%+v: got %d", tc, got)
		}
	}
}

func TestHTTPBodyFramingAndNotifications(t *testing.T) {
	policy := HTTPPolicy{Methods: fixtureMethods, Notification: func(m Message) bool { return m.Method() == "vendor/event" }}
	for _, tc := range []struct {
		raw          string
		status, code int
	}{
		{`{"jsonrpc":"2.0","method":"vendor/event"}`, 202, 0},
		{`{"jsonrpc":"2.0","method":"vendor/event","params":[]}`, 400, InvalidRequest},
		{`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":12}}`, 400, InvalidRequest},
		{`{"jsonrpc":"2.0","method":"notifications/initialized"}`, 400, InvalidRequest},
		{`{"jsonrpc":"2.0","id":1,"result":{"resultType":"complete"}}`, 400, InvalidRequest},
		{`[{}]`, 400, InvalidRequest},
		{`{"jsonrpc":`, 400, ParseError},
		{strings.Repeat(" ", MaxMessage+1), 400, InvalidRequest},
	} {
		r := testPOST([]byte(tc.raw))
		r.Header.Del("Accept")
		in, err := ReadHTTP(r, policy)
		assertHTTPInput(t, in, err, tc.status, tc.code)
	}
	notification := mustParse(t, []byte(`{"jsonrpc":"2.0","method":"vendor/event"}`))
	for _, body := range []string{"", " ", "{}"} {
		got := checkHTTPResponse(notification, 202, "", body)
		if (body == "") != (got == nil) {
			t.Errorf("accepted notification body %q: %v", body, got)
		}
	}
}

func TestHTTPResponseValidation(t *testing.T) {
	m := fixtureRequest(t, "7", "tools/list", "")
	good := string(envResult("7", `{"resultType":"complete","tools":[],"ttlMs":0,"cacheScope":"private"}`))
	methodError := `{"jsonrpc":"2.0","id":7,"error":{"code":-32601,"message":"unavailable"}}`
	for _, tc := range []struct {
		body, media string
		status      int
		valid       bool
	}{
		{good, jsonMedia, 200, true},
		{good, jsonMedia + "; charset=utf-8", 200, true},
		{good, jsonMedia, 404, false},
		{good, "text/html", 200, false},
		{good, jsonMedia + "; charset=iso-8859-1", 200, false},
		{good + good, jsonMedia, 200, false},
		{methodError, jsonMedia, 404, true},
		{methodError, jsonMedia, 200, false},
		{methodError, jsonMedia, 403, false},
		{"", sseMedia, 200, false},
		{"data: " + good + "\n\n", sseMedia, 200, true},
		{"data: " + good + "\n\ndata: " + good + "\n\n", sseMedia, 200, false},
		{"event: endpoint\ndata: /messages\n\n", sseMedia, 200, false},
		{"id: legacy-session\nretry: 5\ndata: " + good + "\n\n", sseMedia, 200, true},
		{"id: 1\nevent: endpoint\ndata: " + good + "\n\n", sseMedia, 200, false},
		{"data: " + good + "\n", sseMedia, 200, false},
	} {
		got := checkHTTPResponse(m, tc.status, tc.media, tc.body)
		if (got == nil) != tc.valid {
			t.Errorf("status %d %s body %q: %v", tc.status, tc.media, tc.body, got)
		}
	}
	// Session and resume headers are inert on receipt: the reply is delivered as if they were absent.
	inert := testHTTPResponse(200, jsonMedia, good)
	defer func() { _ = inert.Body.Close() }()
	inert.Header.Set("Mcp-Session-ID", "added-by-middleware")
	inert.Header.Set("Last-Event-ID", "7")
	reader, err := OpenHTTPResponse(m, inert)
	if err != nil {
		t.Fatalf("refused a reply that carries inert session metadata: %v", err)
	}
	if event, err := reader.Next(); err != nil || !event.Terminal {
		t.Errorf("inert session metadata changed delivery: %v, %v", event, err)
	}
}

func testHTTPResponse(status int, media, body string) *http.Response {
	header := make(http.Header)
	if media != "" {
		header.Set("Content-Type", media)
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body))}
}

func checkHTTPResponse(m Message, status int, media, body string) error {
	response := testHTTPResponse(status, media, body)
	defer func() { _ = response.Body.Close() }()
	reader, err := OpenHTTPResponse(m, response)
	if err != nil {
		return err
	}
	for {
		_, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func TestSSEFramingAndBounds(t *testing.T) {
	body := "\ufeff: keep-alive\r\n\r\nevent: message\rdata: {\rdata: }\r\r"
	raw, err := NewSSEReader(strings.NewReader(body)).Read()
	if err != nil || string(raw) != "{\n}" {
		t.Fatalf("multiline SSE: %q, %v", raw, err)
	}
	large := strings.Repeat("x", MaxMessage)
	for _, content := range []string{"data: " + large + "x\n\n", "data: " + large + "\ndata: x\n\n", "data: " + strings.Repeat("x", MaxMessage+80)} {
		if _, err := NewSSEReader(strings.NewReader(content)).Read(); !errors.Is(err, ErrTooLarge) {
			t.Errorf("SSE bounds (length %d): %v", len(content), err)
		}
	}
}

func FuzzSSE(f *testing.F) {
	f.Add([]byte(": comment\r\ndata: {}\r\n\r\n"))
	f.Add([]byte("event: endpoint\ndata: /legacy\n\n"))
	f.Fuzz(checkSSEFrameLengths)
}

func checkSSEFrameLengths(t *testing.T, raw []byte) {
	reader := NewSSEReader(bytes.NewReader(raw))
	for {
		event, err := reader.Read()
		if err != nil {
			return
		}
		if len(event) > MaxMessage {
			t.Fatalf("oversized event len=%d", len(event))
		}
	}
}

// This loopback fixture supplies auth and routing outside the wire package.
func TestHTTPInertLegacyHeaders(t *testing.T) {
	fixture := httptest.NewUnstartedServer(nil)
	authority := fixture.Listener.Addr().String()
	fixture.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveRawFixture(w, r, authority)
	})
	fixture.Start()
	t.Cleanup(fixture.Close)
	client := fixture.Client()
	client.CloseIdleConnections()
	m := fixtureRequest(t, "1", "tools/list", "")
	var baseline []byte
	for _, attempt := range []struct {
		method string
		auth   bool
		legacy bool
		want   int
	}{
		{"POST", true, false, 200}, {"POST", true, true, 200},
		{"POST", false, true, 401}, {"GET", true, true, 405}, {"DELETE", true, true, 405},
	} {
		r := newFixtureHTTPRequest(t, fixture.URL+"/mcp", m)
		r.Method = attempt.method
		r.Close = true
		if !attempt.auth {
			r.Header.Del("Authorization")
		}
		if attempt.legacy {
			r.Header.Set("Mcp-Session-ID", "a-session")
			r.Header.Set("Last-Event-ID", "a-resume-point")
		}
		body := runLegacyHeaderAttempt(t, client, r, attempt.want)
		if attempt.want == 200 && baseline == nil {
			baseline = body
		}
		if attempt.want == 200 && !bytes.Equal(baseline, body) {
			t.Errorf("legacy headers changed routing or delivery: %s vs %s", baseline, body)
		}
	}
}

func serveRawFixture(w http.ResponseWriter, r *http.Request, authority string) {
	if status := EndpointStatus(r, authority, "/mcp", nil); status != http.StatusOK {
		w.WriteHeader(status)
		return
	}
	if r.Header.Get("Authorization") != "Bearer fixture" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	input, err := ReadHTTP(r, HTTPPolicy{Methods: fixtureMethods})
	if err != nil || input.Status != 200 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	raw, err := Complete(input.Message, json.RawMessage(`{"tools":[{"name":"raw","inputSchema":{"type":"object"}}]}`), json.RawMessage(fixtureInfo), 0)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", jsonMedia)
	_, _ = w.Write(raw)
}

func runLegacyHeaderAttempt(t *testing.T, client *http.Client, r *http.Request, want int) []byte {
	t.Helper()
	res, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != want || res.Header.Get("Mcp-Session-ID") != "" || res.Header.Get("Last-Event-ID") != "" {
		t.Fatalf("%s: status=%d, headers=%v, body=%q, err=%v", r.Method, res.StatusCode, res.Header, body, err)
	}
	return body
}
func newFixtureHTTPRequest(t *testing.T, endpoint string, m Message) *http.Request {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint, bytes.NewReader(m.Raw()))
	if err != nil {
		t.Fatal(err)
	}
	r.Header, err = BuildHTTPHeaders(m, ParameterHeaders{})
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer fixture")
	return r
}
