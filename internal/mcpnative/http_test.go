package mcpnative

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/mcpwire"
)

const (
	testHTTPToken        = "mht1_test_secret_token_alpha_0001"
	testHTTPTokenRotated = "mht1_test_secret_token_beta_0002"
	testAllowedOrigin    = "http://127.0.0.1:3000"
)

func startTestHTTPServer(t *testing.T, opts HTTPServerOptions) (*HTTPServer, *httptest.Server, string) {
	t.Helper()
	ts := httptest.NewUnstartedServer(nil)
	authority := ts.Listener.Addr().String()
	opts.Authority = authority
	if opts.Token == "" {
		opts.Token = testHTTPToken
	}
	srv, err := NewHTTPServer(opts)
	if err != nil {
		ts.Close()
		t.Fatalf("NewHTTPServer: %v", err)
	}
	ts.Config.Handler = srv
	srv.Attach(ts.Config)
	ts.Start()
	t.Cleanup(func() {
		_ = srv.Shutdown(context.Background())
		ts.Close()
	})
	return srv, ts, ts.URL + DefaultHTTPPath
}

func makeMCPHTTPRequest(t *testing.T, endpoint, token, body string, mutate func(*http.Request)) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if mutate != nil {
		mutate(req)
	}
	return req
}

func standardMCPReqBody(id, method, extraParams string) string {
	return fmt.Sprintf(
		`{"jsonrpc":"2.0","id":%s,"method":%q,"params":{"_meta":{"%s":"%s","%s":{}}%s}}`,
		id, method, mcpwire.VersionKey, mcpwire.Version, mcpwire.ClientCapsKey, extraParams,
	)
}

func TestHTTPOriginAndEndpointValidation(t *testing.T) {
	for _, valid := range []string{"http://127.0.0.1:3000", "https://127.0.0.1:8443", "http://localhost:5173", "http://[::1]:3000"} {
		if err := ValidateLoopbackOrigin(valid); err != nil {
			t.Errorf("ValidateLoopbackOrigin(%q) = %v, want nil", valid, err)
		}
	}
	for _, invalid := range []string{"", "null", "https://example.com", "http://192.168.1.5:3000", "http://127.0.0.1:3000/path", "http://127.0.0.1:3000?a=1", "http://127.0.0.1:3000#x", "http://user@127.0.0.1:3000", "ftp://127.0.0.1:3000"} {
		if err := ValidateLoopbackOrigin(invalid); err == nil {
			t.Errorf("ValidateLoopbackOrigin(%q) = nil, want error", invalid)
		}
	}

	_, ts, endpoint := startTestHTTPServer(t, HTTPServerOptions{
		ServerOptions:  ServerOptions{Version: "0.1.0-http", Controls: &stubControls{}},
		AllowedOrigins: []string{testAllowedOrigin},
	})
	client := ts.Client()
	discoverBody := standardMCPReqBody("1", mcpwire.MethodDiscover, "")

	cases := []struct {
		name       string
		method     string
		url        string
		token      string
		mutate     func(*http.Request)
		wantStatus int
		wantAllow  string
		wantWWW    string
	}{
		{"absent origin ok", http.MethodPost, endpoint, testHTTPToken, func(r *http.Request) {
			r.Header.Set(mcpwire.ProtocolHeader, mcpwire.Version)
			r.Header.Set(mcpwire.MethodHeader, mcpwire.MethodDiscover)
		}, http.StatusOK, "", ""},
		{"allowed loopback origin ok", http.MethodPost, endpoint, testHTTPToken, func(r *http.Request) {
			r.Header.Set("Origin", testAllowedOrigin)
			r.Header.Set(mcpwire.ProtocolHeader, mcpwire.Version)
			r.Header.Set(mcpwire.MethodHeader, mcpwire.MethodDiscover)
		}, http.StatusOK, "", ""},
		{"unconfigured loopback origin forbidden", http.MethodPost, endpoint, testHTTPToken, func(r *http.Request) {
			r.Header.Set("Origin", "http://127.0.0.1:4000")
		}, http.StatusForbidden, "", ""},
		{"public origin forbidden", http.MethodPost, endpoint, testHTTPToken, func(r *http.Request) {
			r.Header.Set("Origin", "https://evil.example")
		}, http.StatusForbidden, "", ""},
		{"wrong host authority forbidden", http.MethodPost, endpoint, testHTTPToken, func(r *http.Request) {
			r.Host = "localhost:9999"
		}, http.StatusForbidden, "", ""},
		{"wrong path not found", http.MethodPost, ts.URL + "/other", testHTTPToken, nil, http.StatusNotFound, "", ""},
		{"query string not found", http.MethodPost, endpoint + "?x=1", testHTTPToken, nil, http.StatusNotFound, "", ""},
		{"GET 405", http.MethodGet, endpoint, testHTTPToken, nil, http.StatusMethodNotAllowed, "POST", ""},
		{"DELETE 405", http.MethodDelete, endpoint, testHTTPToken, nil, http.StatusMethodNotAllowed, "POST", ""},
		{"missing auth 401", http.MethodPost, endpoint, "", nil, http.StatusUnauthorized, "", "Bearer"},
		{"wrong bearer token 401", http.MethodPost, endpoint, "mht1_wrong", nil, http.StatusUnauthorized, "", "Bearer"},
		{"duplicate authorization header 401", http.MethodPost, endpoint, testHTTPToken, func(r *http.Request) {
			r.Header.Add("Authorization", "Bearer "+testHTTPToken)
		}, http.StatusUnauthorized, "", "Bearer"},
		{"legacy headers without auth still 401", http.MethodPost, endpoint, "", func(r *http.Request) {
			r.Header.Set("Mcp-Session-Id", "legacy-session")
			r.Header.Set("Last-Event-ID", "42")
		}, http.StatusUnauthorized, "", "Bearer"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := makeMCPHTTPRequest(t, tc.url, tc.token, discoverBody, tc.mutate)
			req.Method = tc.method
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("HTTP %s %s: %v", tc.method, tc.url, err)
			}
			defer func() { _ = resp.Body.Close() }()
			_, _ = io.ReadAll(resp.Body)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if tc.wantAllow != "" && resp.Header.Get("Allow") != tc.wantAllow {
				t.Fatalf("Allow = %q, want %q", resp.Header.Get("Allow"), tc.wantAllow)
			}
			if tc.wantWWW != "" && resp.Header.Get("WWW-Authenticate") != tc.wantWWW {
				t.Fatalf("WWW-Authenticate = %q, want %q", resp.Header.Get("WWW-Authenticate"), tc.wantWWW)
			}
			if resp.Header.Get("Mcp-Session-Id") != "" || resp.Header.Get("Last-Event-ID") != "" {
				t.Fatalf("server echoed legacy session/resume header: %v", resp.Header)
			}
		})
	}
}

func TestHTTPSection22ErrorTableAndInertLegacyHeaders(t *testing.T) {
	_, ts, endpoint := startTestHTTPServer(t, HTTPServerOptions{
		ServerOptions: ServerOptions{Version: "0.1.0-http", Controls: &stubControls{}},
	})
	client := ts.Client()

	cases := []struct {
		name       string
		body       string
		mutate     func(*http.Request)
		wantStatus int
		wantCode   int
	}{
		{"wrong content type", standardMCPReqBody("1", mcpwire.MethodDiscover, ""), func(r *http.Request) {
			r.Header.Set("Content-Type", "text/plain")
		}, http.StatusUnsupportedMediaType, 0},
		{"malformed JSON", `{"jsonrpc":`, nil, http.StatusBadRequest, mcpwire.ParseError},
		{"batch rejected", `[{}]`, nil, http.StatusBadRequest, mcpwire.InvalidRequest},
		{"unsupported notification", `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`, nil, http.StatusBadRequest, mcpwire.InvalidRequest},
		{"missing version header", standardMCPReqBody("1", mcpwire.MethodDiscover, ""), func(r *http.Request) {
			r.Header.Set(mcpwire.MethodHeader, mcpwire.MethodDiscover)
		}, http.StatusBadRequest, mcpwire.HeaderMismatch},
		{"mismatched method header", standardMCPReqBody("1", mcpwire.MethodDiscover, ""), func(r *http.Request) {
			r.Header.Set(mcpwire.ProtocolHeader, mcpwire.Version)
			r.Header.Set(mcpwire.MethodHeader, mcpwire.MethodListTools)
		}, http.StatusBadRequest, mcpwire.HeaderMismatch},
		{"legacy initialize without modern headers", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{}}}`, nil, http.StatusBadRequest, mcpwire.HeaderMismatch},
		{"consistent unsupported version", fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"%s":"2025-11-25","%s":{}}}}`, mcpwire.VersionKey, mcpwire.ClientCapsKey), func(r *http.Request) {
			r.Header.Set(mcpwire.ProtocolHeader, "2025-11-25")
			r.Header.Set(mcpwire.MethodHeader, mcpwire.MethodDiscover)
		}, http.StatusBadRequest, mcpwire.UnsupportedVersion},
		{"missing required metadata", fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"%s":"%s"}}}`, mcpwire.VersionKey, mcpwire.Version), func(r *http.Request) {
			r.Header.Set(mcpwire.ProtocolHeader, mcpwire.Version)
			r.Header.Set(mcpwire.MethodHeader, mcpwire.MethodDiscover)
		}, http.StatusBadRequest, mcpwire.InvalidParams},
		{"legacy initialize with modern headers", standardMCPReqBody("1", "initialize", ""), func(r *http.Request) {
			r.Header.Set(mcpwire.ProtocolHeader, mcpwire.Version)
			r.Header.Set(mcpwire.MethodHeader, "initialize")
		}, http.StatusNotFound, mcpwire.MethodNotFound},
		{"unknown method with modern headers", standardMCPReqBody("1", "vendor/nope", ""), func(r *http.Request) {
			r.Header.Set(mcpwire.ProtocolHeader, mcpwire.Version)
			r.Header.Set(mcpwire.MethodHeader, "vendor/nope")
		}, http.StatusNotFound, mcpwire.MethodNotFound},
		{"missing sse accept", standardMCPReqBody("1", mcpwire.MethodDiscover, ""), func(r *http.Request) {
			r.Header.Set("Accept", "application/json")
			r.Header.Set(mcpwire.ProtocolHeader, mcpwire.Version)
			r.Header.Set(mcpwire.MethodHeader, mcpwire.MethodDiscover)
		}, http.StatusNotAcceptable, 0},
		{"missing name header on tools/call", standardMCPReqBody("1", mcpwire.MethodCallTool, `,"name":"mesh_skill","arguments":{}`), func(r *http.Request) {
			r.Header.Set(mcpwire.ProtocolHeader, mcpwire.Version)
			r.Header.Set(mcpwire.MethodHeader, mcpwire.MethodCallTool)
		}, http.StatusBadRequest, mcpwire.HeaderMismatch},
		{"unknown tool returns 400 -32602", standardMCPReqBody("1", mcpwire.MethodCallTool, `,"name":"mesh_no_such_tool","arguments":{}`), func(r *http.Request) {
			r.Header.Set(mcpwire.ProtocolHeader, mcpwire.Version)
			r.Header.Set(mcpwire.MethodHeader, mcpwire.MethodCallTool)
			r.Header.Set(mcpwire.NameHeader, "mesh_no_such_tool")
		}, http.StatusBadRequest, mcpwire.InvalidParams},
		{"invalid tool args returns 400 -32602", standardMCPReqBody("1", mcpwire.MethodCallTool, `,"name":"mesh_job_status","arguments":{"job":"bad-handle"}`), func(r *http.Request) {
			r.Header.Set(mcpwire.ProtocolHeader, mcpwire.Version)
			r.Header.Set(mcpwire.MethodHeader, mcpwire.MethodCallTool)
			r.Header.Set(mcpwire.NameHeader, "mesh_job_status")
		}, http.StatusBadRequest, mcpwire.InvalidParams},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := makeMCPHTTPRequest(t, endpoint, testHTTPToken, tc.body, tc.mutate)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("POST: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body=%s)", resp.StatusCode, tc.wantStatus, raw)
			}
			if tc.wantCode == 0 {
				return
			}
			var decoded struct {
				Error struct {
					Code int            `json:"code"`
					Data map[string]any `json:"data"`
				} `json:"error"`
			}
			if err := json.Unmarshal(raw, &decoded); err != nil || decoded.Error.Code != tc.wantCode {
				t.Fatalf("error code = %d, want %d (body=%s, err=%v)", decoded.Error.Code, tc.wantCode, raw, err)
			}
			if tc.wantCode == mcpwire.UnsupportedVersion {
				if decoded.Error.Data["requested"] != "2025-11-25" {
					t.Fatalf("unsupported version data.requested = %#v", decoded.Error.Data)
				}
			}
		})
	}

	// Legacy Mcp-Session-Id and Last-Event-ID headers are inert on fresh connections.
	listBody := standardMCPReqBody("7", mcpwire.MethodListTools, "")
	var baseline []byte
	for _, withLegacy := range []bool{false, true} {
		req := makeMCPHTTPRequest(t, endpoint, testHTTPToken, listBody, func(r *http.Request) {
			r.Close = true
			r.Header.Set(mcpwire.ProtocolHeader, mcpwire.Version)
			r.Header.Set(mcpwire.MethodHeader, mcpwire.MethodListTools)
			if withLegacy {
				r.Header.Set("Mcp-Session-Id", "session-xyz")
				r.Header.Set("Last-Event-ID", "event-99")
			}
		})
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("withLegacy=%v: status=%d err=%v body=%s", withLegacy, resp.StatusCode, err, raw)
		}
		if resp.Header.Get("Mcp-Session-Id") != "" || resp.Header.Get("Last-Event-ID") != "" {
			t.Fatalf("server emitted session/resume header: %v", resp.Header)
		}
		if baseline == nil {
			baseline = raw
		} else if !bytes.Equal(baseline, raw) {
			t.Fatalf("legacy headers altered response:\nwant: %s\ngot:  %s", baseline, raw)
		}
	}
}

func TestHTTPMirroredParameterHeaders(t *testing.T) {
	mirroredSpec := ToolSpec{
		Name:        "mesh_mirrored_test",
		Title:       "Mirrored header test tool",
		Operation:   "hosts.list",
		Group:       groupHosts,
		Description: "Exercises x-mcp-header parameter mirroring on H6.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"additionalProperties":false,
			"required":["label"],
			"properties":{
				"label":{"type":"string","x-mcp-header":"Label"},
				"nested":{
					"type":"object",
					"properties":{
						"flag":{"type":"boolean","x-mcp-header":"Flag"},
						"count":{"type":"integer","x-mcp-header":"Count"}
					}
				}
			}
		}`),
		Annotations: readOnlyAnnotations(),
		Handler: func(_ context.Context, _ Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
			return CallOutcome{Envelope: okEnvelope("hosts.list", map[string]any{"raw": string(raw)})}, nil
		},
	}
	registry := NewRegistry(append(DefaultRegistry().ordered, mirroredSpec))
	_, ts, endpoint := startTestHTTPServer(t, HTTPServerOptions{
		ServerOptions: ServerOptions{Version: "0.1.0-http", Registry: registry, Controls: &stubControls{}},
	})
	client := ts.Client()

	body := standardMCPReqBody("1", mcpwire.MethodCallTool, `,"name":"mesh_mirrored_test","arguments":{"label":"héllo","nested":{"flag":true,"count":42}}`)

	// Missing mirrored headers -> 400 / -32020.
	badReq := makeMCPHTTPRequest(t, endpoint, testHTTPToken, body, func(r *http.Request) {
		r.Header.Set(mcpwire.ProtocolHeader, mcpwire.Version)
		r.Header.Set(mcpwire.MethodHeader, mcpwire.MethodCallTool)
		r.Header.Set(mcpwire.NameHeader, "mesh_mirrored_test")
	})
	badResp, err := client.Do(badReq)
	if err != nil {
		t.Fatal(err)
	}
	badRaw, _ := io.ReadAll(badResp.Body)
	_ = badResp.Body.Close()
	if badResp.StatusCode != http.StatusBadRequest || !bytes.Contains(badRaw, []byte("-32020")) {
		t.Fatalf("missing mirrored headers: status=%d body=%s", badResp.StatusCode, badRaw)
	}

	// Exact mirrored headers (with Base64 UTF-8 sentinel for non-ASCII string) -> 200.
	goodReq := makeMCPHTTPRequest(t, endpoint, testHTTPToken, body, func(r *http.Request) {
		r.Header.Set(mcpwire.ProtocolHeader, mcpwire.Version)
		r.Header.Set(mcpwire.MethodHeader, mcpwire.MethodCallTool)
		r.Header.Set(mcpwire.NameHeader, "mesh_mirrored_test")
		r.Header.Set("Mcp-Param-Label", mcpwire.EncodeHeaderValue("héllo"))
		r.Header.Set("Mcp-Param-Flag", "true")
		r.Header.Set("Mcp-Param-Count", "42")
	})
	goodResp, err := client.Do(goodReq)
	if err != nil {
		t.Fatal(err)
	}
	goodRaw, _ := io.ReadAll(goodResp.Body)
	_ = goodResp.Body.Close()
	if goodResp.StatusCode != http.StatusOK {
		t.Fatalf("valid mirrored headers: status=%d body=%s", goodResp.StatusCode, goodRaw)
	}

	// Unexpected mirrored header when optional nested field is omitted -> 400 / -32020.
	omittedBody := standardMCPReqBody("2", mcpwire.MethodCallTool, `,"name":"mesh_mirrored_test","arguments":{"label":"plain"}`)
	extraReq := makeMCPHTTPRequest(t, endpoint, testHTTPToken, omittedBody, func(r *http.Request) {
		r.Header.Set(mcpwire.ProtocolHeader, mcpwire.Version)
		r.Header.Set(mcpwire.MethodHeader, mcpwire.MethodCallTool)
		r.Header.Set(mcpwire.NameHeader, "mesh_mirrored_test")
		r.Header.Set("Mcp-Param-Label", "plain")
		r.Header.Set("Mcp-Param-Count", "7")
	})
	extraResp, err := client.Do(extraReq)
	if err != nil {
		t.Fatal(err)
	}
	extraRaw, _ := io.ReadAll(extraResp.Body)
	_ = extraResp.Body.Close()
	if extraResp.StatusCode != http.StatusBadRequest || !bytes.Contains(extraRaw, []byte("-32020")) {
		t.Fatalf("unexpected mirrored header: status=%d body=%s", extraResp.StatusCode, extraRaw)
	}
}

func TestHTTPBothRepresentationsAndStreamCancellationAndRotation(t *testing.T) {
	waitStarted := make(chan struct{}, 2)
	waitCancelled := make(chan error, 2)
	var jobCancelCalls atomic.Int32

	controls := &stubControls{
		waitFn: func(ctx context.Context, req JobWaitRequest, notify func(ProgressUpdate)) (CallOutcome, error) {
			total := float64(req.WaitMS)
			notify(ProgressUpdate{Progress: 1, Total: &total, Message: "waiting"})
			waitStarted <- struct{}{}
			<-ctx.Done()
			waitCancelled <- ctx.Err()
			return CallOutcome{}, ctx.Err()
		},
		cancelFn: func(context.Context, JobRefRequest) (CallOutcome, error) {
			jobCancelCalls.Add(1)
			return CallOutcome{}, errors.New("JobCancel must not be invoked by stream cancellation or token rotation")
		},
	}

	stderr := &syncBuffer{}
	srv, ts, endpoint := startTestHTTPServer(t, HTTPServerOptions{
		ServerOptions: ServerOptions{Version: "0.1.0-http", Controls: controls, Stderr: stderr},
	})
	client := ts.Client()

	// 1. JSON representation (default when Accept gives equal quality).
	skillBody := standardMCPReqBody("1", mcpwire.MethodCallTool, `,"name":"mesh_skill","arguments":{}`)
	jsonReq := makeMCPHTTPRequest(t, endpoint, testHTTPToken, skillBody, func(r *http.Request) {
		r.Header.Set(mcpwire.ProtocolHeader, mcpwire.Version)
		r.Header.Set(mcpwire.MethodHeader, mcpwire.MethodCallTool)
		r.Header.Set(mcpwire.NameHeader, "mesh_skill")
	})
	jsonResp, err := client.Do(jsonReq)
	if err != nil {
		t.Fatal(err)
	}
	msg1, _ := mcpwire.Parse([]byte(skillBody))
	jsonReader, err := mcpwire.OpenHTTPResponse(msg1, jsonResp)
	if err != nil {
		t.Fatalf("OpenHTTPResponse JSON: %v", err)
	}
	if !strings.HasPrefix(jsonResp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("expected application/json, got %q", jsonResp.Header.Get("Content-Type"))
	}
	if ev, err := jsonReader.Next(); err != nil || !ev.Terminal {
		t.Fatalf("JSON event = %+v, err=%v", ev, err)
	}
	_ = jsonResp.Body.Close()

	// 2. Request-scoped SSE representation (Accept prefers text/event-stream).
	sseReq := makeMCPHTTPRequest(t, endpoint, testHTTPToken, skillBody, func(r *http.Request) {
		r.Header.Set("Accept", "text/event-stream, application/json;q=0.5")
		r.Header.Set(mcpwire.ProtocolHeader, mcpwire.Version)
		r.Header.Set(mcpwire.MethodHeader, mcpwire.MethodCallTool)
		r.Header.Set(mcpwire.NameHeader, "mesh_skill")
	})
	sseResp, err := client.Do(sseReq)
	if err != nil {
		t.Fatal(err)
	}
	sseReader, err := mcpwire.OpenHTTPResponse(msg1, sseResp)
	if err != nil {
		t.Fatalf("OpenHTTPResponse SSE: %v", err)
	}
	if !strings.HasPrefix(sseResp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("expected text/event-stream, got %q", sseResp.Header.Get("Content-Type"))
	}
	if ev, err := sseReader.Next(); err != nil || !ev.Terminal {
		t.Fatalf("SSE event = %+v, err=%v", ev, err)
	}
	_ = sseResp.Body.Close()

	// 3. Stream cancellation: closing SSE response body cancels the wait handler
	// without ever invoking JobCancel.
	waitBody := fmt.Sprintf(
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"_meta":{"%s":"%s","%s":{},"progressToken":"p1"},"name":"mesh_job_wait","arguments":{"job":%q,"wait_ms":30000}}}`,
		mcpwire.VersionKey, mcpwire.Version, mcpwire.ClientCapsKey, testJob,
	)
	waitMsg, _ := mcpwire.Parse([]byte(waitBody))
	cancelReq := makeMCPHTTPRequest(t, endpoint, testHTTPToken, waitBody, func(r *http.Request) {
		r.Header.Set(mcpwire.ProtocolHeader, mcpwire.Version)
		r.Header.Set(mcpwire.MethodHeader, mcpwire.MethodCallTool)
		r.Header.Set(mcpwire.NameHeader, "mesh_job_wait")
	})
	cancelResp, err := client.Do(cancelReq)
	if err != nil {
		t.Fatal(err)
	}
	cancelReader, err := mcpwire.OpenHTTPResponse(waitMsg, cancelResp)
	if err != nil {
		t.Fatalf("OpenHTTPResponse wait SSE: %v", err)
	}
	ev, err := cancelReader.Next()
	if err != nil || ev.Terminal || ev.Message.Method() != "notifications/progress" {
		t.Fatalf("expected initial progress SSE event, got %+v err=%v", ev, err)
	}
	<-waitStarted
	if err := cancelResp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-waitCancelled:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("wait cancelled with %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("closing HTTP SSE response stream did not cancel handler context")
	}
	// The worker sees the cancellation before the handler finishes its
	// exchange; rotation below must count only the stream it retires.
	exchangesDone := make(chan struct{})
	go func() {
		srv.exchanges.Wait()
		close(exchangesDone)
	}()
	select {
	case <-exchangesDone:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled SSE exchange never finished")
	}

	// 4. Token rotation: retires old token AND its active SSE stream, while new
	// token works immediately and token values are never logged.
	rotReq := makeMCPHTTPRequest(t, endpoint, testHTTPToken, waitBody, func(r *http.Request) {
		r.Header.Set(mcpwire.ProtocolHeader, mcpwire.Version)
		r.Header.Set(mcpwire.MethodHeader, mcpwire.MethodCallTool)
		r.Header.Set(mcpwire.NameHeader, "mesh_job_wait")
	})
	rotResp, err := client.Do(rotReq)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rotResp.Body.Close() }()
	rotReader, err := mcpwire.OpenHTTPResponse(waitMsg, rotResp)
	if err != nil {
		t.Fatalf("OpenHTTPResponse rotation SSE: %v", err)
	}
	if ev, err := rotReader.Next(); err != nil || ev.Terminal {
		t.Fatalf("rotation wait initial event = %+v err=%v", ev, err)
	}
	<-waitStarted

	gen, retired, err := srv.RotateToken(testHTTPTokenRotated)
	if err != nil || gen != 2 || retired != 1 {
		t.Fatalf("RotateToken = gen=%d retired=%d err=%v, want gen=2 retired=1", gen, retired, err)
	}
	select {
	case err := <-waitCancelled:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("rotated stream cancelled with %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("token rotation did not cancel active SSE stream")
	}
	if _, err := rotReader.Next(); !errors.Is(err, mcpwire.ErrInterrupted) {
		t.Fatalf("retired SSE reader Next() = %v, want ErrInterrupted", err)
	}

	// Old token now returns 401; new token succeeds.
	oldCheck := makeMCPHTTPRequest(t, endpoint, testHTTPToken, skillBody, func(r *http.Request) {
		r.Header.Set(mcpwire.ProtocolHeader, mcpwire.Version)
		r.Header.Set(mcpwire.MethodHeader, mcpwire.MethodCallTool)
		r.Header.Set(mcpwire.NameHeader, "mesh_skill")
	})
	oldResp, err := client.Do(oldCheck)
	if err != nil {
		t.Fatal(err)
	}
	_ = oldResp.Body.Close()
	if oldResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old token after rotation status = %d, want 401", oldResp.StatusCode)
	}

	newCheck := makeMCPHTTPRequest(t, endpoint, testHTTPTokenRotated, skillBody, func(r *http.Request) {
		r.Header.Set(mcpwire.ProtocolHeader, mcpwire.Version)
		r.Header.Set(mcpwire.MethodHeader, mcpwire.MethodCallTool)
		r.Header.Set(mcpwire.NameHeader, "mesh_skill")
	})
	newResp, err := client.Do(newCheck)
	if err != nil {
		t.Fatal(err)
	}
	_ = newResp.Body.Close()
	if newResp.StatusCode != http.StatusOK {
		t.Fatalf("new token after rotation status = %d, want 200", newResp.StatusCode)
	}
	if jobCancelCalls.Load() != 0 {
		t.Fatalf("JobCancel called %d times, want 0", jobCancelCalls.Load())
	}
	if logs := stderr.String(); strings.Contains(logs, testHTTPToken) || strings.Contains(logs, testHTTPTokenRotated) {
		t.Fatalf("bearer token leaked to stderr: %q", logs)
	}
}

func TestHTTPCapacityAndOversizeLimits(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	controls := &stubControls{
		hostsFn: func(_ context.Context, _ ListHostsRequest) (CallOutcome, error) {
			entered <- struct{}{}
			<-release
			return CallOutcome{Envelope: okEnvelope("hosts.list", agentapi.HostsData{})}, nil
		},
		outputFn: func(_ context.Context, req JobOutputRequest) (CallOutcome, error) {
			return CallOutcome{Envelope: okEnvelope("sessions.logs", JobOutputData{
				Job: req.Job.String(), Stream: req.Stream, Encoding: "utf-8", Data: strings.Repeat("A", mcpwire.MaxMessage),
			})}, nil
		},
	}

	_, ts, endpoint := startTestHTTPServer(t, HTTPServerOptions{
		ServerOptions: ServerOptions{Version: "0.1.0-http", Controls: controls, MaxRequests: 1},
	})
	client := ts.Client()

	// Fill the single request slot.
	hostsBody := standardMCPReqBody("1", mcpwire.MethodCallTool, `,"name":"mesh_hosts_list","arguments":{}`)
	slotDone := make(chan int, 1)
	go func() {
		req := makeMCPHTTPRequest(t, endpoint, testHTTPToken, hostsBody, func(r *http.Request) {
			r.Header.Set(mcpwire.ProtocolHeader, mcpwire.Version)
			r.Header.Set(mcpwire.MethodHeader, mcpwire.MethodCallTool)
			r.Header.Set(mcpwire.NameHeader, "mesh_hosts_list")
		})
		resp, err := client.Do(req)
		if err != nil {
			slotDone <- 0
			return
		}
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		slotDone <- resp.StatusCode
	}()
	<-entered

	// Second concurrent request hits resource.limit (-31000).
	skillBody := standardMCPReqBody("2", mcpwire.MethodCallTool, `,"name":"mesh_skill","arguments":{}`)
	overReq := makeMCPHTTPRequest(t, endpoint, testHTTPToken, skillBody, func(r *http.Request) {
		r.Header.Set(mcpwire.ProtocolHeader, mcpwire.Version)
		r.Header.Set(mcpwire.MethodHeader, mcpwire.MethodCallTool)
		r.Header.Set(mcpwire.NameHeader, "mesh_skill")
	})
	overResp, err := client.Do(overReq)
	if err != nil {
		t.Fatal(err)
	}
	overRaw, _ := io.ReadAll(overResp.Body)
	_ = overResp.Body.Close()
	if overResp.StatusCode != http.StatusOK || !bytes.Contains(overRaw, []byte(string(agentapi.ResourceLimit))) {
		t.Fatalf("capacity error = status %d body=%s", overResp.StatusCode, overRaw)
	}

	close(release)
	if code := <-slotDone; code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", code)
	}

	// Oversized reply (> 3.5 MiB) returns mcp.message_too_large (-31000).
	bigBody := standardMCPReqBody("3", mcpwire.MethodCallTool, fmt.Sprintf(`,"name":"mesh_job_output","arguments":{"job":%q}`, testJob))
	bigReq := makeMCPHTTPRequest(t, endpoint, testHTTPToken, bigBody, func(r *http.Request) {
		r.Header.Set(mcpwire.ProtocolHeader, mcpwire.Version)
		r.Header.Set(mcpwire.MethodHeader, mcpwire.MethodCallTool)
		r.Header.Set(mcpwire.NameHeader, "mesh_job_output")
	})
	bigResp, err := client.Do(bigReq)
	if err != nil {
		t.Fatal(err)
	}
	bigRaw, _ := io.ReadAll(bigResp.Body)
	_ = bigResp.Body.Close()
	if bigResp.StatusCode != http.StatusOK || !bytes.Contains(bigRaw, []byte(string(agentapi.MCPMessageTooLarge))) {
		t.Fatalf("oversize reply = status %d body=%s", bigResp.StatusCode, bigRaw)
	}
}

type bearerRoundTripper struct {
	next  http.RoundTripper
	token string
}

func (b *bearerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+b.token)
	resp, err := b.next.RoundTrip(clone)
	if err != nil {
		return nil, fmt.Errorf("bearer test HTTP round trip: %w", err)
	}
	return resp, nil
}

func TestSDKStreamableHTTPPositivePeerInterop(t *testing.T) {
	t.Setenv("MCPGODEBUG", "")
	for _, preferSSE := range []bool{false, true} {
		mode := "JSON"
		if preferSSE {
			mode = "SSE"
		}
		t.Run(mode, func(t *testing.T) {
			controls := &stubControls{
				outputFn: func(_ context.Context, req JobOutputRequest) (CallOutcome, error) {
					if req.Cursor == "expired" {
						return CallOutcome{
							Envelope: agentapi.Envelope{
								Operation: "sessions.logs",
								Status:    agentapi.StatusError,
								Data:      JobOutputData{Job: req.Job.String(), Stream: req.Stream, Gap: true, EarliestCursor: "earliest-tok"},
								Errors:    []agentapi.Error{agentapi.Fail(agentapi.LogsCursorExpired, "sessions.logs", req.Job.HostID, "cursor expired").Err},
							},
							IsError: true,
						}, nil
					}
					target := &TargetMeta{HostID: req.Job.HostID, HostName: "pc", Account: "agent"}
					return CallOutcome{
						Envelope: okEnvelope("sessions.logs", JobOutputData{Job: req.Job.String(), Target: *target, Stream: req.Stream, Encoding: "utf-8", Data: "sdk-http-hello\n", EOF: true}),
						Target:   target,
					}, nil
				},
			}

			_, ts, endpoint := startTestHTTPServer(t, HTTPServerOptions{
				ServerOptions: ServerOptions{Version: "0.1.0-sdk-http", Controls: controls},
				PreferSSE:     preferSSE,
			})
			httpClient := ts.Client()
			httpClient.Transport = &bearerRoundTripper{next: httpClient.Transport, token: testHTTPToken}

			sdkClient := mcp.NewClient(&mcp.Implementation{Name: "go-sdk-http-peer", Version: "1.8.0"}, nil)
			transport := &mcp.StreamableClientTransport{
				Endpoint:             endpoint,
				HTTPClient:           httpClient,
				MaxRetries:           -1,
				DisableStandaloneSSE: true,
			}
			session, err := sdkClient.Connect(t.Context(), transport, &mcp.ClientSessionOptions{
				ProtocolVersion: mcpwire.Version,
			})
			if err != nil {
				t.Fatalf("SDK HTTP connect (%s): %v", mode, err)
			}
			defer func() { _ = session.Close() }()

			listed, err := session.ListTools(t.Context(), nil)
			if err != nil {
				t.Fatalf("SDK HTTP ListTools (%s): %v", mode, err)
			}
			if len(listed.Tools) != len(DefaultRegistry().Definitions()) {
				t.Fatalf("SDK HTTP ListTools count = %d, want %d", len(listed.Tools), len(DefaultRegistry().Definitions()))
			}

			skillRes, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "mesh_skill"})
			if err != nil || skillRes.IsError || len(skillRes.Content) == 0 {
				t.Fatalf("SDK HTTP mesh_skill (%s): res=%+v err=%v", mode, skillRes, err)
			}

			hostsRes, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "mesh_hosts_list"})
			if err != nil || hostsRes.IsError || hostsRes.StructuredContent == nil {
				t.Fatalf("SDK HTTP mesh_hosts_list (%s): res=%+v err=%v", mode, hostsRes, err)
			}
			rawEnv, err := json.Marshal(hostsRes.StructuredContent)
			if err != nil {
				t.Fatal(err)
			}
			var env agentapi.Envelope
			if err := json.Unmarshal(rawEnv, &env); err != nil || env.Status != agentapi.StatusOK || env.Operation != "hosts.list" {
				t.Fatalf("SDK HTTP structuredContent (%s) = %s, err=%v", mode, rawEnv, err)
			}

			outRes, err := session.CallTool(t.Context(), &mcp.CallToolParams{
				Name:      "mesh_job_output",
				Arguments: map[string]any{"job": testJob, "stream": "stdout"},
			})
			if err != nil || outRes.IsError {
				t.Fatalf("SDK HTTP mesh_job_output (%s): res=%+v err=%v", mode, outRes, err)
			}

			errRes, err := session.CallTool(t.Context(), &mcp.CallToolParams{
				Name:      "mesh_job_output",
				Arguments: map[string]any{"job": testJob, "stream": "stdout", "cursor": "expired"},
			})
			if err != nil || !errRes.IsError || errRes.StructuredContent == nil {
				t.Fatalf("SDK HTTP tool error (%s): res=%+v err=%v", mode, errRes, err)
			}
		})
	}
}

type trackingBody struct {
	data  []byte
	off   int
	reads atomic.Int64
}

func (b *trackingBody) Read(p []byte) (int, error) {
	b.reads.Add(1)
	if b.off >= len(b.data) {
		return 0, io.EOF
	}
	n := copy(p, b.data[b.off:])
	b.off += n
	return n, nil
}

func (b *trackingBody) Close() error { return nil }

type blockingResponseWriter struct {
	header       http.Header
	status       int
	mu           sync.Mutex
	buf          bytes.Buffer
	flushEntered chan struct{}
	flushRelease chan struct{}
	blockFirst   bool
	flushes      int
	aborted      atomic.Bool
}

func newBlockingResponseWriter(blockFirst bool) *blockingResponseWriter {
	return &blockingResponseWriter{
		header:       make(http.Header),
		flushEntered: make(chan struct{}, 1),
		flushRelease: make(chan struct{}),
		blockFirst:   blockFirst,
	}
}

func (w *blockingResponseWriter) Header() http.Header { return w.header }

func (w *blockingResponseWriter) WriteHeader(code int) {
	w.mu.Lock()
	if w.status == 0 {
		w.status = code
	}
	w.mu.Unlock()
}

func (w *blockingResponseWriter) Write(p []byte) (int, error) {
	if w.aborted.Load() {
		return 0, context.DeadlineExceeded
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.buf.Write(p)
	if err != nil {
		return n, fmt.Errorf("write blocking test buffer: %w", err)
	}
	return n, nil
}

func (w *blockingResponseWriter) Flush() {
	w.mu.Lock()
	w.flushes++
	n := w.flushes
	w.mu.Unlock()
	if w.blockFirst && n == 1 {
		select {
		case w.flushEntered <- struct{}{}:
		default:
		}
		<-w.flushRelease
	}
}

func (w *blockingResponseWriter) SetWriteDeadline(t time.Time) error {
	if !t.IsZero() && time.Now().After(t) {
		w.aborted.Store(true)
	}
	return nil
}

func TestHTTPByteBudgetsIngressAndOutput(t *testing.T) {
	// 1. Connection output budget (8192 bytes) caps a ~14 KiB tools/list reply.
	_, ts, endpoint := startTestHTTPServer(t, HTTPServerOptions{
		ServerOptions: ServerOptions{
			Version:          "0.1.0-budget",
			Controls:         &stubControls{},
			MaxConnBytes:     8192,
			MaxFrontendBytes: 16384,
		},
	})
	listBody := standardMCPReqBody("1", mcpwire.MethodListTools, "")
	req := makeMCPHTTPRequest(t, endpoint, testHTTPToken, listBody, func(r *http.Request) {
		r.Header.Set(mcpwire.ProtocolHeader, mcpwire.Version)
		r.Header.Set(mcpwire.MethodHeader, mcpwire.MethodListTools)
	})
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if len(raw) > 8192 || !bytes.Contains(raw, []byte(string(agentapi.ResourceLimit))) {
		t.Fatalf("tools/list exceeded 8192 conn budget: len=%d body=%q", len(raw), raw[:min(len(raw), 200)])
	}

	// 2. When all request slots are full, admission fails BEFORE reading body.
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	srv, err := NewHTTPServer(HTTPServerOptions{
		ServerOptions: ServerOptions{
			Version: "0.1.0-budget",
			Controls: &stubControls{
				hostsFn: func(_ context.Context, _ ListHostsRequest) (CallOutcome, error) {
					entered <- struct{}{}
					<-release
					return CallOutcome{Envelope: okEnvelope("hosts.list", agentapi.HostsData{})}, nil
				},
			},
			MaxRequests:      1,
			MaxConnBytes:     32768,
			MaxFrontendBytes: 32768,
		},
		Authority: "127.0.0.1:9000",
		Token:     testHTTPToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Shutdown(context.Background()) }()

	hostsBody := standardMCPReqBody("1", mcpwire.MethodCallTool, `,"name":"mesh_hosts_list","arguments":{}`)
	firstReq := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9000/mcp", strings.NewReader(hostsBody))
	firstReq.URL.Host = ""
	firstReq.Host = "127.0.0.1:9000"
	firstReq.Header.Set("Authorization", "Bearer "+testHTTPToken)
	firstReq.Header.Set("Content-Type", "application/json")
	firstReq.Header.Set("Accept", "application/json, text/event-stream")
	firstReq.Header.Set(mcpwire.ProtocolHeader, mcpwire.Version)
	firstReq.Header.Set(mcpwire.MethodHeader, mcpwire.MethodCallTool)
	firstReq.Header.Set(mcpwire.NameHeader, "mesh_hosts_list")
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		srv.ServeHTTP(httptest.NewRecorder(), firstReq)
	}()
	<-entered

	secondBody := &trackingBody{data: []byte(standardMCPReqBody("2", mcpwire.MethodDiscover, ""))}
	secondReq := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9000/mcp", secondBody)
	secondReq.URL.Host = ""
	secondReq.Host = "127.0.0.1:9000"
	secondReq.ContentLength = -1
	secondReq.Header.Set("Authorization", "Bearer "+testHTTPToken)
	secondReq.Header.Set("Content-Type", "application/json")
	secondReq.Header.Set("Accept", "application/json, text/event-stream")
	secondReq.Header.Set(mcpwire.ProtocolHeader, mcpwire.Version)
	secondReq.Header.Set(mcpwire.MethodHeader, mcpwire.MethodDiscover)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, secondReq)
	if secondBody.reads.Load() != 0 {
		t.Fatalf("body was read %d times while request slots were full, want 0", secondBody.reads.Load())
	}
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte(string(agentapi.ResourceLimit))) {
		t.Fatalf("over-capacity reply = status %d body=%s", rec.Code, rec.Body.String())
	}
	close(release)
	<-firstDone

	// 3. Frontend byte budget enforces concurrent body reads and blocked response writes.
	srvBytes, err := NewHTTPServer(HTTPServerOptions{
		ServerOptions: ServerOptions{
			Version:          "0.1.0-budget",
			Controls:         &stubControls{},
			MaxRequests:      4,
			MaxConnBytes:     16384,
			MaxFrontendBytes: 12000,
		},
		Authority: "127.0.0.1:9000",
		Token:     testHTTPToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srvBytes.Shutdown(context.Background()) }()

	// Streamed chunked body larger than frontend budget is refused mid-read without buffering beyond budget.
	oversizedChunked := &trackingBody{data: bytes.Repeat([]byte(" "), 20000)}
	chunkReq := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9000/mcp", oversizedChunked)
	chunkReq.URL.Host = ""
	chunkReq.Host = "127.0.0.1:9000"
	chunkReq.ContentLength = -1
	chunkReq.Header.Set("Authorization", "Bearer "+testHTTPToken)
	chunkReq.Header.Set("Content-Type", "application/json")
	chunkReq.Header.Set("Accept", "application/json, text/event-stream")
	chunkRec := httptest.NewRecorder()
	srvBytes.ServeHTTP(chunkRec, chunkReq)
	if !bytes.Contains(chunkRec.Body.Bytes(), []byte(string(agentapi.ResourceLimit))) {
		t.Fatalf("oversized chunked body did not fail with resource.limit: status=%d body=%s", chunkRec.Code, chunkRec.Body.String())
	}

	// 4. Blocked response write holds terminal output bytes against MaxFrontendBytes
	// until delivery finishes, so a concurrent request sees resource.limit.
	srvBlocked, err := NewHTTPServer(HTTPServerOptions{
		ServerOptions: ServerOptions{
			Version: "0.1.0-budget",
			Controls: &stubControls{
				outputFn: func(_ context.Context, req JobOutputRequest) (CallOutcome, error) {
					return CallOutcome{Envelope: okEnvelope("sessions.logs", JobOutputData{
						Job: req.Job.String(), Stream: req.Stream, Encoding: "utf-8", Data: strings.Repeat("Z", 5000), EOF: true,
					})}, nil
				},
			},
			MaxRequests:      4,
			MaxConnBytes:     12000,
			MaxFrontendBytes: 12000,
		},
		Authority: "127.0.0.1:9000",
		Token:     testHTTPToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srvBlocked.Shutdown(context.Background()) }()

	outBody := standardMCPReqBody("3", mcpwire.MethodCallTool, fmt.Sprintf(`,"name":"mesh_job_output","arguments":{"job":%q}`, testJob))
	outReq1 := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9000/mcp", strings.NewReader(outBody))
	outReq1.URL.Host = ""
	outReq1.Host = "127.0.0.1:9000"
	outReq1.Header.Set("Authorization", "Bearer "+testHTTPToken)
	outReq1.Header.Set("Content-Type", "application/json")
	outReq1.Header.Set("Accept", "application/json, text/event-stream")
	outReq1.Header.Set(mcpwire.ProtocolHeader, mcpwire.Version)
	outReq1.Header.Set(mcpwire.MethodHeader, mcpwire.MethodCallTool)
	outReq1.Header.Set(mcpwire.NameHeader, "mesh_job_output")

	bwOut := newBlockingResponseWriter(true)
	outDone1 := make(chan struct{})
	go func() {
		defer close(outDone1)
		srvBlocked.ServeHTTP(bwOut, outReq1)
	}()
	<-bwOut.flushEntered

	// While outReq1 is blocked flushing ~5.5 KiB terminal output, a second
	// ~5.5 KiB reply would exceed the 12,000-byte frontend budget (and 4 KiB
	// reserve), so it must fall back to resource.limit.
	outReq2 := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9000/mcp", strings.NewReader(outBody))
	outReq2.URL.Host = ""
	outReq2.Host = "127.0.0.1:9000"
	outReq2.Header.Set("Authorization", "Bearer "+testHTTPToken)
	outReq2.Header.Set("Content-Type", "application/json")
	outReq2.Header.Set("Accept", "application/json, text/event-stream")
	outReq2.Header.Set(mcpwire.ProtocolHeader, mcpwire.Version)
	outReq2.Header.Set(mcpwire.MethodHeader, mcpwire.MethodCallTool)
	outReq2.Header.Set(mcpwire.NameHeader, "mesh_job_output")
	rec2 := httptest.NewRecorder()
	srvBlocked.ServeHTTP(rec2, outReq2)
	if !bytes.Contains(rec2.Body.Bytes(), []byte(string(agentapi.ResourceLimit))) {
		close(bwOut.flushRelease)
		<-outDone1
		t.Fatalf("second reply while first write blocked did not hit resource.limit: len=%d body=%q", rec2.Body.Len(), rec2.Body.String()[:min(rec2.Body.Len(), 200)])
	}
	close(bwOut.flushRelease)
	<-outDone1

	// 5. Stdio server construction does not fail on HTTP-only x-mcp-header annotations.
	badHTTPRouteSpec := ToolSpec{
		Name:        "mesh_http_bad_annotation",
		Operation:   "hosts.list",
		Group:       groupHosts,
		Description: "Invalid HTTP routing annotation must not block stdio startup.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"x":{"type":"object","x-mcp-header":"Bad"}}}`),
		Annotations: readOnlyAnnotations(),
		Handler:     handleHostsList,
	}
	reg := NewRegistry([]ToolSpec{badHTTPRouteSpec})
	if _, err := NewStdioServer(ServerOptions{Registry: reg, Controls: &stubControls{}}); err != nil {
		t.Fatalf("NewStdioServer failed on HTTP-only annotation: %v", err)
	}
	if _, err := NewHTTPServer(HTTPServerOptions{ServerOptions: ServerOptions{Registry: reg, Controls: &stubControls{}}, Authority: "127.0.0.1:9000", Token: testHTTPToken}); err == nil {
		t.Fatal("NewHTTPServer accepted invalid x-mcp-header annotation")
	}
}

func TestHTTPBoundarySizeInvalidEnvelopeWithLongID(t *testing.T) {
	_, ts, endpoint := startTestHTTPServer(t, HTTPServerOptions{
		ServerOptions: ServerOptions{Version: "0.1.0-bound", Controls: &stubControls{}},
	})

	// Construct an invalid JSON-RPC envelope (missing jsonrpc / invalid method) at
	// exact MaxMessage size with a long string ID that cannot fit back inside an
	// error reply within MaxMessage.
	prefix := `{"jsonrpc":"2.0","id":"`
	suffix := `","method":"server/discover","params":{}}`
	longID := strings.Repeat("x", mcpwire.MaxMessage-len(prefix)-len(suffix))
	body := prefix + longID + suffix
	if len(body) != mcpwire.MaxMessage {
		t.Fatalf("test setup len = %d, want %d", len(body), mcpwire.MaxMessage)
	}

	// Missing routing headers -> section 2.2 requires HTTP 400 with a JSON-RPC -32020 body (id:null fallback).
	req := makeMCPHTTPRequest(t, endpoint, testHTTPToken, body, nil)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("boundary invalid envelope status = %d, want 400 (body=%q)", resp.StatusCode, raw)
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("boundary Content-Type = %q, want application/json", resp.Header.Get("Content-Type"))
	}
	var decoded struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded.JSONRPC != "2.0" || string(decoded.ID) != "null" || decoded.Error == nil || decoded.Error.Code != mcpwire.HeaderMismatch {
		t.Fatalf("boundary error body = %s, err=%v", raw, err)
	}

	// Also verify invalid envelope (non-2.0 jsonrpc) at exact MaxMessage with long ID -> 400 / -32600.
	prefix2 := `{"jsonrpc":"1.0","id":"`
	suffix2 := `","method":"server/discover"}`
	body2 := prefix2 + strings.Repeat("y", mcpwire.MaxMessage-len(prefix2)-len(suffix2)) + suffix2
	req2 := makeMCPHTTPRequest(t, endpoint, testHTTPToken, body2, nil)
	resp2, err := ts.Client().Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	raw2, _ := io.ReadAll(resp2.Body)
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest || !bytes.Contains(raw2, []byte(`"id":null`)) || !bytes.Contains(raw2, []byte("-32600")) {
		t.Fatalf("boundary jsonrpc 1.0 status=%d body=%s", resp2.StatusCode, raw2)
	}
}

func TestHTTPContextOpenAndExec(t *testing.T) {
	var openedReq OpenContextRequest
	var execReq ExecRequest
	releaseExec := make(chan struct{})
	exitZero := 0
	controls := &stubControls{
		openContextFn: func(_ context.Context, req OpenContextRequest) (CallOutcome, error) {
			openedReq = req
			return CallOutcome{
				Envelope: okEnvelope("contexts.open", agentapi.ExecutionContext{
					Handle:    testContext,
					HostID:    testHostID,
					ContextID: testContextID,
					State:     "open",
					Revision:  1,
					Cwd:       req.Cwd,
					Shell:     req.Shell,
				}),
				Target: &TargetMeta{HostID: testHostID, ContextID: testContext, Cwd: req.Cwd},
			}, nil
		},
		execFn: func(ctx context.Context, req ExecRequest, notify func(ProgressUpdate)) (CallOutcome, error) {
			execReq = req
			if notify != nil {
				notify(ProgressUpdate{Progress: 12, Message: "running " + testJob})
			}
			select {
			case <-releaseExec:
			case <-ctx.Done():
				return CallOutcome{}, ctx.Err()
			}
			return CallOutcome{
				Envelope: okEnvelope("sessions.create", ExecResultData{
					Job:        testJob,
					RequestKey: req.RequestKey,
					Target:     TargetMeta{HostID: testHostID, ContextID: req.Context.String(), Cwd: "/tmp"},
					State:      "exited",
					Wait:       "completed",
					Result: &agentapi.ExecutionResult{
						Termination: agentapi.ExecutionTermination{Kind: "exit", Code: &exitZero},
					},
					Stdout: &ExecStreamData{Encoding: "utf-8", Data: "hello-from-http-exec\n", EOF: true},
					Stderr: &ExecStreamData{Encoding: "utf-8", Data: "", EOF: true},
				}),
				Target: &TargetMeta{HostID: testHostID, ContextID: req.Context.String(), Cwd: "/tmp"},
			}, nil
		},
	}

	_, ts, endpoint := startTestHTTPServer(t, HTTPServerOptions{
		ServerOptions: ServerOptions{Version: "0.1.0-ctx", Controls: controls},
	})
	client := ts.Client()

	// 1. tools/list over HTTP exposes context tools and mesh_exec from the shared registry.
	listReq := makeMCPHTTPRequest(t, endpoint, testHTTPToken, standardMCPReqBody("1", mcpwire.MethodListTools, ""), func(r *http.Request) {
		r.Header.Set(mcpwire.ProtocolHeader, mcpwire.Version)
		r.Header.Set(mcpwire.MethodHeader, mcpwire.MethodListTools)
	})
	listResp, err := client.Do(listReq)
	if err != nil {
		t.Fatal(err)
	}
	listRaw, err := io.ReadAll(listResp.Body)
	_ = listResp.Body.Close()
	if err != nil || listResp.StatusCode != http.StatusOK {
		t.Fatalf("tools/list status=%d err=%v body=%s", listResp.StatusCode, err, listRaw)
	}
	for _, wantTool := range []string{"mesh_context_open", "mesh_context_get", "mesh_context_list", "mesh_context_receipt", "mesh_context_update", "mesh_context_close", "mesh_exec"} {
		if !bytes.Contains(listRaw, []byte(`"`+wantTool+`"`)) {
			t.Fatalf("HTTP tools/list missing %s", wantTool)
		}
	}

	// 2. Open a context over HTTP (JSON response).
	openBody := standardMCPReqBody("2", mcpwire.MethodCallTool, `,"name":"mesh_context_open","arguments":{"host":"pc","request_key":"http-ctx-1","cwd":"/tmp","shell":"sh"}`)
	openReq := makeMCPHTTPRequest(t, endpoint, testHTTPToken, openBody, func(r *http.Request) {
		r.Header.Set(mcpwire.ProtocolHeader, mcpwire.Version)
		r.Header.Set(mcpwire.MethodHeader, mcpwire.MethodCallTool)
		r.Header.Set(mcpwire.NameHeader, "mesh_context_open")
	})
	openResp, err := client.Do(openReq)
	if err != nil {
		t.Fatal(err)
	}
	openRaw, err := io.ReadAll(openResp.Body)
	_ = openResp.Body.Close()
	if err != nil || openResp.StatusCode != http.StatusOK || !bytes.Contains(openRaw, []byte(testContext)) {
		t.Fatalf("mesh_context_open status=%d err=%v body=%s", openResp.StatusCode, err, openRaw)
	}
	if openedReq.Host != "pc" || openedReq.RequestKey != "http-ctx-1" || openedReq.Cwd != "/tmp" || openedReq.Shell != "sh" {
		t.Fatalf("unexpected OpenContextRequest: %+v", openedReq)
	}

	// 3. Run a short job with mesh_exec over HTTP (SSE with progress notification + terminal result).
	execBody := fmt.Sprintf(
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"_meta":{"%s":"%s","%s":{},"progressToken":"exec-1"},"name":"mesh_exec","arguments":{"context":%q,"request_key":"http-exec-1","command":"echo hello-from-http-exec"}}}`,
		mcpwire.VersionKey, mcpwire.Version, mcpwire.ClientCapsKey, testContext,
	)
	execHTTPReq := makeMCPHTTPRequest(t, endpoint, testHTTPToken, execBody, func(r *http.Request) {
		r.Header.Set(mcpwire.ProtocolHeader, mcpwire.Version)
		r.Header.Set(mcpwire.MethodHeader, mcpwire.MethodCallTool)
		r.Header.Set(mcpwire.NameHeader, "mesh_exec")
	})
	execResp, err := client.Do(execHTTPReq)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = execResp.Body.Close() }()
	if execResp.StatusCode != http.StatusOK || !strings.HasPrefix(execResp.Header.Get("Content-Type"), "text/event-stream") {
		raw, _ := io.ReadAll(execResp.Body)
		close(releaseExec)
		t.Fatalf("mesh_exec status=%d content-type=%q body=%s", execResp.StatusCode, execResp.Header.Get("Content-Type"), raw)
	}
	// Read the first SSE progress chunk before releasing exec completion so the test is deterministic.
	buf := make([]byte, 4096)
	n, readErr := execResp.Body.Read(buf)
	close(releaseExec)
	rest, restErr := io.ReadAll(execResp.Body)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		t.Fatalf("read SSE progress: %v", readErr)
	}
	if restErr != nil {
		t.Fatalf("read SSE completion: %v", restErr)
	}
	var execRaw []byte
	execRaw = append(execRaw, buf[:n]...)
	execRaw = append(execRaw, rest...)
	if !bytes.Contains(execRaw, []byte(`"notifications/progress"`)) || !bytes.Contains(execRaw, []byte("hello-from-http-exec")) {
		t.Fatalf("mesh_exec SSE missing progress or stdout: %s", execRaw)
	}
	if execReq.Context.String() != testContext || execReq.RequestKey != "http-exec-1" || execReq.Command != "echo hello-from-http-exec" {
		t.Fatalf("unexpected ExecRequest: %+v", execReq)
	}
}
