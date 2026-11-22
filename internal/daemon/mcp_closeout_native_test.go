package daemon

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/mcphost"
	"github.com/shaul/mesh/internal/mcpwire"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/protocol/mcpframe"
)

const mcpHTTPFixtureRole = "mcp-http-fixture"

func runMCPHTTPFixture(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("mcp-http-fixture needs LISTEN_ADDR")
	}
	ln, err := net.Listen("tcp", args[0])
	if err != nil {
		return fmt.Errorf("listen %s: %w", args[0], err)
	}
	cwd, _ := os.Getwd()
	_, _ = fmt.Fprintf(os.Stderr, "managed-http-fixture pid=%d cwd=%s\n", os.Getpid(), cwd)
	// Also write non-MCP text to stdout to prove managed HTTP runs on a plain
	// pipe worker rather than a line-framed MCP stdio worker.
	_, _ = fmt.Fprintf(os.Stdout, "plain stdout banner from pid=%d\n", os.Getpid())
	srv := &http.Server{Handler: mcpHTTPFixtureHandler(os.Getpid()), ReadHeaderTimeout: 5 * time.Second}
	if err := srv.Serve(ln); err != nil {
		return fmt.Errorf("serve HTTP fixture: %w", err)
	}
	return nil
}

func mcpHTTPFixtureHandler(pid int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		req, fault := mcpwire.Parse(body)
		if fault != nil {
			http.Error(w, fault.Message, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		var reply map[string]any
		switch req.Method() {
		case mcpwire.MethodDiscover:
			reply = map[string]any{
				"jsonrpc": "2.0", "id": req.ID(),
				"result": map[string]any{
					"resultType": "complete", "supportedVersions": []string{"2026-07-28"},
					"capabilities": map[string]any{
						"tools":      map[string]any{},
						"privateCap": map[string]any{"note": "PROBE_CAP_EXTENSION_SENTINEL"},
					},
					"ttlMs": 60000, "cacheScope": "private",
					"_meta": map[string]any{
						"io.modelcontextprotocol/serverInfo": map[string]any{
							"name":       "managed-http",
							"version":    "1",
							"privateExt": "PROBE_INFO_EXTENSION_SENTINEL",
						},
					},
				},
			}
		case mcpwire.MethodListTools:
			reply = map[string]any{
				"jsonrpc": "2.0", "id": req.ID(),
				"result": map[string]any{
					"resultType": "complete",
					"tools":      []map[string]any{{"name": "info", "inputSchema": map[string]any{"type": "object"}}},
					"ttlMs":      60000, "cacheScope": "private",
				},
			}
		case mcpwire.MethodCallTool:
			reply = map[string]any{
				"jsonrpc": "2.0", "id": req.ID(),
				"result": map[string]any{
					"resultType": "complete",
					"content":    []map[string]any{{"type": "text", "text": fmt.Sprintf("pid=%d auth=%s", pid, r.Header.Get("Authorization"))}},
				},
			}
		default:
			reply = map[string]any{
				"jsonrpc": "2.0", "id": req.ID(),
				"error": map[string]any{"code": -32601, "message": "unknown"},
			}
		}
		_ = json.NewEncoder(w).Encode(reply)
	})
}

func freeTestLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// TestHostedMCPOversizedStdioReplyKeepsInstanceAlive proves end-to-end against
// a real daemon and line worker that a stdio server reply over 3.5 MiB with a
// verified ID and newline fails only that request with mcp.message_too_large.
func TestHostedMCPOversizedStdioReplyKeepsInstanceAlive(t *testing.T) {
	f := newMCPDaemon(t)
	f.publishMCP(t, mcphost.Definition{
		Name:           "fixture",
		Argv:           []string{f.executable, mcpFixtureRole},
		Cwd:            f.root,
		InstancePolicy: mcphost.PolicyShared,
	})
	cred := newCredential(t)
	before := fixtureInstance(t, resultText(t, f.mcpCall(t, cred, "whoami", `{}`)))

	// 4 MiB reply exceeds the 3.5 MiB cap but stays within the 16 MiB drain.
	oversized := f.mcpCall(t, cred, "big", fmt.Sprintf(`{"bytes":%d}`, 4<<20))
	if errorCode(oversized) != "mcp.message_too_large" {
		t.Fatalf("oversized stdio reply = %s, want mcp.message_too_large", oversized.Raw())
	}

	// Concurrent in-flight request beside an oversized reply: only the oversized
	// call fails with mcp.message_too_large while the concurrent request succeeds.
	var wg sync.WaitGroup
	var bigReply, okReply mcpwire.Message
	wg.Add(2)
	go func() {
		defer wg.Done()
		bigReply = f.mcpCall(t, cred, "big", fmt.Sprintf(`{"bytes":%d}`, 4<<20))
	}()
	go func() {
		defer wg.Done()
		okReply = f.mcpCall(t, cred, "echo", `{"text":"concurrent-ok"}`)
	}()
	wg.Wait()
	if errorCode(bigReply) != "mcp.message_too_large" {
		t.Fatalf("concurrent oversized reply = %s, want mcp.message_too_large", bigReply.Raw())
	}
	if got := resultText(t, okReply); got != "concurrent-ok" {
		t.Fatalf("concurrent normal reply = %q, want concurrent-ok", got)
	}
	after := fixtureInstance(t, resultText(t, f.mcpCall(t, cred, "whoami", `{}`)))
	if after != before {
		t.Fatalf("oversized reply faulted the instance: before=%s after=%s", before, after)
	}

	// Malformed stdout framing must fault the channel and restart on next call.
	bad := f.mcpCall(t, cred, "garbage", `{}`)
	if bad.ErrorObject() == nil {
		t.Fatalf("malformed framing call unexpectedly succeeded: %s", bad.Raw())
	}
	pipeEventually(t, func() bool {
		reply := f.mcpCall(t, cred, "whoami", `{}`)
		return reply.ErrorObject() == nil && fixtureInstance(t, resultText(t, reply)) != before
	})
}

// TestHostedMCPManagedHTTPNative exercises a Mesh-managed HTTP MCP server on a
// real plain pipe worker: probe, call, stderr diagnostics, raw execution
// isolation, restart, daemon-restart adoption, stop and remove.
func TestHostedMCPManagedHTTPNative(t *testing.T) {
	f := newMCPDaemon(t)
	addr := freeTestLoopbackAddr(t)
	runDir := filepath.Join(f.root, "managed-dir")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(f.root, "http-auth")
	if err := os.WriteFile(secret, []byte("Bearer managed-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.publishMCP(t, mcphost.Definition{
		Name:           "managed",
		URL:            "http://" + addr + "/mcp",
		RunArgv:        []string{f.executable, mcpHTTPFixtureRole, addr},
		RunCwd:         runDir,
		InstancePolicy: mcphost.PolicyShared,
		AccountMode:    mcphost.AccountHostAccount,
		AccountLabel:   "managed bot",
		HeaderSecrets:  map[string]string{"Authorization": secret},
	})

	probe := f.mcpControl(t, protocol.MCPServiceRequest{Type: protocol.TypeMCPServiceProbe, RequestID: "probe", Service: "managed"})
	if probe.Type != protocol.TypeMCPServiceResult || !strings.Contains(string(probe.Result), `"state":"ready"`) {
		t.Fatalf("probe managed HTTP: %+v", probe)
	}

	cred := newCredential(t)
	_, reader := f.mcpStream(t, mcpframe.Route{Service: "managed", Caller: cred}, mcpToolCall(`"m1"`, "info", `{}`))
	first := resultText(t, readMCP(t, reader)[0])
	if !strings.Contains(first, "auth=Bearer managed-secret") || !strings.Contains(first, "pid=") {
		t.Fatalf("managed HTTP call: %q", first)
	}
	pid1 := strings.Fields(first)[0]

	pipeEventually(t, func() bool {
		reply := f.mcpControl(t, protocol.MCPServiceRequest{Type: protocol.TypeMCPServiceLogs, RequestID: "logs", Service: "managed"})
		var out struct {
			Stderr []byte `json:"stderr"`
		}
		_ = json.Unmarshal(reply.Result, &out)
		return strings.Contains(string(out.Stderr), "managed-http-fixture "+pid1) && strings.Contains(string(out.Stderr), "cwd="+runDir)
	})

	// Raw execution controls cannot touch the internal plain pipe worker.
	entries, err := os.ReadDir(f.sessions)
	if err != nil {
		t.Fatal(err)
	}
	checkedWorkers := 0
	for _, entry := range entries {
		metaBytes, err := os.ReadFile(filepath.Join(f.sessions, entry.Name(), "meta.json"))
		if err != nil {
			continue
		}
		var meta struct {
			ID          string `json:"id"`
			Reservation struct {
				Instance string `json:"instance"`
			} `json:"reservation"`
		}
		if json.Unmarshal(metaBytes, &meta) != nil || meta.ID == "" || meta.Reservation.Instance == "" {
			continue
		}
		checkedWorkers++
		reply, err := f.exchange(protocol.Control{
			Type:      protocol.TypeExecutionLogs,
			RequestID: "raw-logs",
			SessionID: meta.ID,
			Instance:  meta.Reservation.Instance,
			Stream:    "stderr",
		})
		if err != nil || reply.Type != protocol.TypeError {
			t.Fatalf("raw execution.logs reached managed HTTP worker %s: %+v (%v)", meta.ID, reply, err)
		}
	}
	if checkedWorkers == 0 {
		t.Fatal("expected at least one managed HTTP worker directory to verify raw execution isolation")
	}

	// Restart replaces the plain pipe worker with a new PID at generation 2.
	restarted := f.mcpControl(t, protocol.MCPServiceRequest{Type: protocol.TypeMCPServiceRestart, RequestID: "restart", Service: "managed"})
	if restarted.Type != protocol.TypeMCPServiceResult || !strings.Contains(string(restarted.Result), `"generation":2`) {
		t.Fatalf("restart managed HTTP: %+v", restarted)
	}
	_, reader2 := f.mcpStream(t, mcpframe.Route{Service: "managed", Caller: cred}, mcpToolCall(`"m2"`, "info", `{}`))
	second := resultText(t, readMCP(t, reader2)[0])
	pid2 := strings.Fields(second)[0]
	if pid2 == pid1 {
		t.Fatalf("restart kept the same PID: %s", pid1)
	}

	// Daemon restart adopts the exact managed HTTP plain pipe worker.
	f.stop(t)
	f.start(t)
	_, reader3 := f.mcpStream(t, mcpframe.Route{Service: "managed", Caller: cred}, mcpToolCall(`"m3"`, "info", `{}`))
	third := resultText(t, readMCP(t, reader3)[0])
	if pid3 := strings.Fields(third)[0]; pid3 != pid2 {
		t.Fatalf("daemon restart replaced managed HTTP worker: %s then %s", pid2, pid3)
	}

	// Stop kills the owned plain pipe worker; rm removes the service.
	if reply := f.mcpControl(t, protocol.MCPServiceRequest{Type: protocol.TypeMCPServiceStop, RequestID: "stop", Service: "managed"}); reply.Type != protocol.TypeMCPServiceResult {
		t.Fatalf("stop managed HTTP: %+v", reply)
	}
	pipeEventually(t, func() bool { return f.liveWorkers(t) == 0 })
	if reply := f.mcpControl(t, protocol.MCPServiceRequest{Type: protocol.TypeMCPServiceDelete, RequestID: "rm", Service: "managed"}); reply.Type != protocol.TypeMCPServiceResult {
		t.Fatalf("rm managed HTTP: %+v", reply)
	}
}

// TestHostedMCPExternalHTTPControlsNative verifies probe, enable, disable and
// non-ownership refusal for an external loopback HTTP MCP server.
func TestHostedMCPExternalHTTPControlsNative(t *testing.T) {
	f := newMCPDaemon(t)
	srv := httptest.NewServer(mcpHTTPFixtureHandler(9999))
	t.Cleanup(srv.Close)

	f.publishMCP(t, mcphost.Definition{
		Name:           "external",
		URL:            srv.URL + "/mcp",
		InstancePolicy: mcphost.PolicyShared,
	})
	probe := f.mcpControl(t, protocol.MCPServiceRequest{Type: protocol.TypeMCPServiceProbe, RequestID: "probe", Service: "external"})
	if probe.Type != protocol.TypeMCPServiceResult || !strings.Contains(string(probe.Result), `"state":"ready"`) {
		t.Fatalf("probe external HTTP: %+v", probe)
	}
	// Finding 7: raw wire probe response must contain only allowlisted discovery fields.
	for _, forbidden := range []string{"PROBE_CAP_EXTENSION_SENTINEL", "PROBE_INFO_EXTENSION_SENTINEL", "privateCap", "privateExt"} {
		if strings.Contains(string(probe.Result), forbidden) {
			t.Fatalf("probe wire response leaked unprojected discovery field %q: %s", forbidden, probe.Result)
		}
	}
	if !strings.Contains(string(probe.Result), `"name":"managed-http"`) || !strings.Contains(string(probe.Result), `"tools"`) {
		t.Fatalf("probe wire response missing allowlisted discovery fields: %s", probe.Result)
	}
	if got := f.liveWorkers(t); got != 0 {
		t.Fatalf("external HTTP spawned %d workers, want 0", got)
	}
	for _, kind := range []string{protocol.TypeMCPServiceStart, protocol.TypeMCPServiceStop, protocol.TypeMCPServiceRestart, protocol.TypeMCPServiceLogs} {
		reply := f.mcpControl(t, protocol.MCPServiceRequest{Type: kind, RequestID: kind, Service: "external"})
		if reply.Type != protocol.TypeError || reply.ErrorCode != executionUsageInvalidCode {
			t.Fatalf("%s on external HTTP = %+v, want %s", kind, reply, executionUsageInvalidCode)
		}
	}
	disabled := f.mcpControl(t, protocol.MCPServiceRequest{Type: protocol.TypeMCPServiceDisable, RequestID: "disable", Service: "external"})
	if disabled.Type != protocol.TypeMCPServiceResult || !strings.Contains(string(disabled.Result), `"disabled":true`) {
		t.Fatalf("disable external HTTP: %+v", disabled)
	}
	cred := newCredential(t)
	_, reader := f.mcpStream(t, mcpframe.Route{Service: "external", Caller: cred}, mcpToolCall(`"e1"`, "info", `{}`))
	if reply := readMCP(t, reader)[0]; errorCode(reply) != "service.unavailable" {
		t.Fatalf("forward while disabled: %s", reply.Raw())
	}
	enabled := f.mcpControl(t, protocol.MCPServiceRequest{Type: protocol.TypeMCPServiceEnable, RequestID: "enable", Service: "external"})
	if enabled.Type != protocol.TypeMCPServiceResult || strings.Contains(string(enabled.Result), `"disabled":true`) {
		t.Fatalf("enable external HTTP: %+v", enabled)
	}
	_, reader2 := f.mcpStream(t, mcpframe.Route{Service: "external", Caller: cred}, mcpToolCall(`"e2"`, "info", `{}`))
	if got := resultText(t, readMCP(t, reader2)[0]); !strings.Contains(got, "pid=9999") {
		t.Fatalf("forward after enable: %q", got)
	}
}

// TestHostedMCPManagedHTTPOwnerGuardNative proves against real Linux/macOS
// sockets and workers that managed HTTP refuses an occupied port before spawn
// and refuses to send credentials to a rogue listener that takes over after
// the managed worker exits.
func TestHostedMCPManagedHTTPOwnerGuardNative(t *testing.T) {
	f := newMCPDaemon(t)
	secret := filepath.Join(f.root, "guard-secret")
	if err := os.WriteFile(secret, []byte("Bearer native-guard-token"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Case 1: occupied port must fail before spawning any worker or sending credentials.
	var incumbentHits atomic.Int32
	var incumbentAuth atomic.Value
	incumbent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		incumbentHits.Add(1)
		if auth := r.Header.Get("Authorization"); auth != "" {
			incumbentAuth.Store(auth)
		}
		mcpHTTPFixtureHandler(7777).ServeHTTP(w, r)
	}))
	t.Cleanup(incumbent.Close)
	incAddr := strings.TrimPrefix(incumbent.URL, "http://")
	f.publishMCP(t, mcphost.Definition{
		Name:           "occupied",
		URL:            incumbent.URL + "/mcp",
		RunArgv:        []string{f.executable, mcpHTTPFixtureRole, incAddr},
		RunCwd:         f.root,
		InstancePolicy: mcphost.PolicyShared,
		AccountMode:    mcphost.AccountHostAccount,
		AccountLabel:   "guard bot",
		HeaderSecrets:  map[string]string{"Authorization": secret},
	})
	if reply := f.mcpControl(t, protocol.MCPServiceRequest{Type: protocol.TypeMCPServiceStart, RequestID: "start-occ", Service: "occupied"}); reply.Type != protocol.TypeError {
		t.Fatalf("managed HTTP started on occupied port: %+v", reply)
	}
	if f.liveWorkers(t) != 0 {
		t.Fatalf("managed HTTP spawned %d workers on occupied port, want 0", f.liveWorkers(t))
	}
	if incumbentHits.Load() != 0 || incumbentAuth.Load() != nil {
		t.Fatalf("incumbent received %d hits (auth=%v), want 0", incumbentHits.Load(), incumbentAuth.Load())
	}

	// Case 2: after managed worker exits, a rogue listener taking over the port
	// must never receive credentials.
	addr := freeTestLoopbackAddr(t)
	f.publishMCP(t, mcphost.Definition{
		Name:           "takeover",
		URL:            "http://" + addr + "/mcp",
		RunArgv:        []string{f.executable, mcpHTTPFixtureRole, addr},
		RunCwd:         f.root,
		InstancePolicy: mcphost.PolicyShared,
		AccountMode:    mcphost.AccountHostAccount,
		AccountLabel:   "guard bot",
		HeaderSecrets:  map[string]string{"Authorization": secret},
	})
	if reply := f.mcpControl(t, protocol.MCPServiceRequest{Type: protocol.TypeMCPServiceStart, RequestID: "start-ok", Service: "takeover"}); reply.Type != protocol.TypeMCPServiceResult {
		t.Fatalf("start managed takeover fixture: %+v", reply)
	}
	cred := newCredential(t)
	_, reader := f.mcpStream(t, mcpframe.Route{Service: "takeover", Caller: cred}, mcpToolCall(`"t1"`, "info", `{}`))
	first := resultText(t, readMCP(t, reader)[0])
	pidStr := strings.TrimPrefix(strings.Fields(first)[0], "pid=")
	childPID, err := strconv.Atoi(pidStr)
	if err != nil || childPID <= 1 {
		t.Fatalf("parse managed fixture pid from %q: %v", first, err)
	}
	// Kill the fixture child directly and immediately bind a rogue listener on addr.
	_ = syscall.Kill(childPID, syscall.SIGKILL)
	var rogueLn net.Listener
	pipeEventually(t, func() bool {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return false
		}
		rogueLn = ln
		return true
	})
	var rogueHits atomic.Int32
	var rogueAuth atomic.Value
	rogueSrv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rogueHits.Add(1)
			if auth := r.Header.Get("Authorization"); auth != "" {
				rogueAuth.Store(auth)
			}
			mcpHTTPFixtureHandler(8888).ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = rogueSrv.Serve(rogueLn) }()
	t.Cleanup(func() { _ = rogueSrv.Close() })

	_, reader2 := f.mcpStream(t, mcpframe.Route{Service: "takeover", Caller: cred}, mcpToolCall(`"t2"`, "info", `{}`))
	reply2 := readMCP(t, reader2)[0]
	if reply2.ErrorObject() == nil {
		t.Fatalf("call against rogue listener succeeded: %s", reply2.Raw())
	}
	if rogueHits.Load() != 0 || rogueAuth.Load() != nil {
		t.Fatalf("rogue listener received %d hits (auth=%v) after worker exit", rogueHits.Load(), rogueAuth.Load())
	}
}
