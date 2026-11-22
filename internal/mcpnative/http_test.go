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
