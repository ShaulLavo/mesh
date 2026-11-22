package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/mcphost"
	"github.com/shaul/mesh/internal/mcphost/mcpfixture"
	"github.com/shaul/mesh/internal/mcpwire"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/protocol/mcpframe"
)

const mcpFixtureRole = "mcp-fixture"

func (f *nativePipeDaemon) mcpControl(t *testing.T, req protocol.MCPServiceRequest) protocol.MCPServiceResponse {
	t.Helper()
	conn, err := net.DialTimeout("unix", SocketPath(f.state), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	frame, err := protocol.EncodeMCP(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.NewWriter(conn).WriteControl(frame.Payload); err != nil {
		t.Fatal(err)
	}
	reply, err := protocol.NewReader(conn).ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	var out protocol.MCPServiceResponse
	if err := json.Unmarshal(reply.Payload, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *nativePipeDaemon) publishMCP(t *testing.T, d mcphost.Definition) {
	t.Helper()
	raw, _ := json.Marshal(d)
	reply := f.mcpControl(t, protocol.MCPServiceRequest{Type: protocol.TypeMCPServiceUpsert, RequestID: "publish", Definition: raw})
	if reply.Type != protocol.TypeMCPServiceResult {
		t.Fatalf("publish %s: %+v", d.Name, reply)
	}
}

// mcpStream opens one H3 forward and returns its connection for reading.
func (f *nativePipeDaemon) mcpStream(t *testing.T, route mcpframe.Route, raw string) (net.Conn, *protocol.Reader) {
	t.Helper()
	conn, err := net.DialTimeout("unix", SocketPath(f.state), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
	frame, err := mcpframe.EncodeForward(route, json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.NewWriter(conn).WriteControl(frame.Payload); err != nil {
		t.Fatal(err)
	}
	return conn, protocol.NewReader(conn)
}

// readMCP returns every message until the terminal mcp.result.
func readMCP(t *testing.T, reader *protocol.Reader) []mcpwire.Message {
	t.Helper()
	var out []mcpwire.Message
	for {
		frame, err := reader.ReadFrame()
		if err != nil {
			t.Fatalf("read H3 stream: %v", err)
		}
		kind, message, err := mcpframe.Decode(frame)
		if err != nil {
			t.Fatalf("decode H3 frame %.300s: %v", frame.Payload, err)
		}
		out = append(out, message)
		if kind == mcpframe.Result {
			return out
		}
	}
}

func (f *nativePipeDaemon) mcpCall(t *testing.T, credential, tool, args string) mcpwire.Message {
	t.Helper()
	_, reader := f.mcpStream(t, mcpframe.Route{Service: "fixture", AppCredential: credential}, mcpToolCall(`"call-1"`, tool, args))
	messages := readMCP(t, reader)
	return messages[len(messages)-1]
}

func mcpToolCall(id, tool, args string) string {
	return `{"jsonrpc":"2.0","id":` + id + `,"method":"tools/call","params":{"name":"` + tool + `","arguments":` + args +
		`,"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
}

func newCredential(t *testing.T) string {
	t.Helper()
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b[:])
}

func resultText(t *testing.T, m mcpwire.Message) string {
	t.Helper()
	var result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(m.Result(), &result); err != nil || len(result.Content) == 0 {
		t.Fatalf("not a tool result: %s", m.Raw())
	}
	return result.Content[0].Text
}

func fixtureInstance(t *testing.T, text string) string {
	t.Helper()
	_, rest, ok := strings.Cut(text, "instance=")
	if !ok {
		t.Fatalf("no instance in %q", text)
	}
	id, _, _ := strings.Cut(rest, " ")
	return id
}

func errorCode(m mcpwire.Message) string {
	var e struct {
		Data struct {
			MeshCode string `json:"meshCode"`
		} `json:"data"`
	}
	_ = json.Unmarshal(m.ErrorObject(), &e)
	return e.Data.MeshCode
}

func newMCPDaemon(t *testing.T) *nativePipeDaemon {
	t.Helper()
	f := newNativePipeDaemon(t)
	t.Cleanup(func() {
		if t.Failed() {
			log, _ := os.ReadFile(filepath.Join(f.root, "daemon.log"))
			t.Logf("daemon log:\n%.8000s", log)
			workers, _ := filepath.Glob(filepath.Join(f.sessions, "*", "worker.log"))
			for _, path := range workers {
				log, _ := os.ReadFile(path) //nolint:gosec // the test's own session directory
				t.Logf("%s:\n%.4000s", path, log)
			}
		}
	})
	return f
}

func TestHostedMCPNative(t *testing.T) {
	f := newMCPDaemon(t)
	secret := filepath.Join(f.root, "token")
	if err := os.WriteFile(secret, []byte("native-secret-42"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.publishMCP(t, mcphost.Definition{Name: "fixture", Argv: []string{f.executable, mcpFixtureRole}, Cwd: f.root,
		SecretEnv: map[string]string{mcpfixture.EnvSecret: secret}, AccountMode: mcphost.AccountHostAccount, AccountLabel: "fixture bot"})
	a, b := newCredential(t), newCredential(t)

	first := f.mcpCall(t, a, "whoami", `{}`)
	if string(member(first.Raw(), "id")) != `"call-1"` {
		t.Fatalf("caller ID was not restored: %s", first.Raw())
	}
	instanceA := fixtureInstance(t, resultText(t, first))
	instanceB := fixtureInstance(t, resultText(t, f.mcpCall(t, b, "whoami", `{}`)))
	if instanceA == instanceB {
		t.Fatal("two application callers on one device shared an isolated process")
	}

	t.Run("cancel on disconnect", func(t *testing.T) {
		conn, _ := f.mcpStream(t, mcpframe.Route{Service: "fixture", AppCredential: a}, mcpToolCall(`7`, "slow", `{}`))
		time.Sleep(200 * time.Millisecond)
		_ = conn.Close()
		pipeEventually(t, func() bool { return strings.Contains(f.mcpLogs(t, a), "cancelled \"mesh-") })
	})

	t.Run("catalog carries no secret", func(t *testing.T) {
		for _, credential := range []string{"", a} {
			reply := f.mcpControl(t, protocol.MCPServiceRequest{Type: protocol.TypeMCPCatalog, RequestID: "ls", AppCredential: credential})
			if strings.Contains(string(reply.Result), "native-secret") || strings.Contains(string(reply.Result), secret) || !strings.Contains(string(reply.Result), "fixture bot") {
				t.Fatalf("catalog: %s", reply.Result)
			}
		}
		if !strings.Contains(f.mcpLogs(t, a), "native-secret-42") {
			t.Fatal("the owner's diagnostics should hold the server's own stderr")
		}
		if logs := f.mcpControl(t, protocol.MCPServiceRequest{Type: protocol.TypeMCPServiceLogs, RequestID: "logs", Service: "fixture", AppCredential: b, Instance: f.instanceID(t, a)}); logs.ErrorCode != "access.denied" {
			t.Fatalf("another caller read diagnostics: %+v", logs)
		}
	})

	t.Run("adopted across daemon restart", func(t *testing.T) {
		f.stop(t)
		f.start(t)
		after := fixtureInstance(t, resultText(t, f.mcpCall(t, a, "whoami", `{}`)))
		if after != instanceA {
			t.Fatalf("daemon restart replaced the worker: %s then %s", instanceA, after)
		}
	})

	t.Run("crash is reported and restarted on demand", func(t *testing.T) {
		f.mcpCall(t, b, "exit", `{}`)
		pipeEventually(t, func() bool {
			reply := f.mcpCall(t, b, "whoami", `{}`)
			return reply.ErrorObject() == nil && fixtureInstance(t, resultText(t, reply)) != instanceB
		})
	})

	t.Run("legacy server is incompatible", func(t *testing.T) {
		f.publishMCP(t, mcphost.Definition{Name: "legacy", Argv: []string{f.executable, mcpFixtureRole}, Env: map[string]string{mcpfixture.EnvMode: "legacy"}})
		_, reader := f.mcpStream(t, mcpframe.Route{Service: "legacy", AppCredential: a}, mcpToolCall(`1`, "echo", `{"text":"x"}`))
		reply := readMCP(t, reader)[0]
		if errorCode(reply) != "service.incompatible" {
			t.Fatalf("legacy server: %s", reply.Raw())
		}
	})

	t.Run("missing runtime is classified", func(t *testing.T) {
		f.publishMCP(t, mcphost.Definition{Name: "missing", Argv: []string{"/bin/sh", "-c", "exec /no/such/runtime"}})
		_, reader := f.mcpStream(t, mcpframe.Route{Service: "missing", AppCredential: a}, mcpToolCall(`1`, "echo", `{"text":"x"}`))
		reply := readMCP(t, reader)[0]
		if errorCode(reply) != "service.failed" || !strings.Contains(string(reply.ErrorObject()), "127") {
			t.Fatalf("missing runtime: %s", reply.Raw())
		}
	})

	t.Run("remove stops workers", func(t *testing.T) {
		for _, name := range []string{"fixture", "legacy", "missing"} {
			if reply := f.mcpControl(t, protocol.MCPServiceRequest{Type: protocol.TypeMCPServiceDelete, RequestID: "rm", Service: name}); reply.Type != protocol.TypeMCPServiceResult {
				t.Fatalf("remove %s: %+v", name, reply)
			}
		}
		pipeEventually(t, func() bool { return f.liveWorkers(t) == 0 })
	})
}

func (f *nativePipeDaemon) mcpLogs(t *testing.T, credential string) string {
	t.Helper()
	reply := f.mcpControl(t, protocol.MCPServiceRequest{Type: protocol.TypeMCPServiceLogs, RequestID: "logs", Service: "fixture", AppCredential: credential})
	var out struct {
		Stderr []byte `json:"stderr"`
	}
	_ = json.Unmarshal(reply.Result, &out)
	return string(out.Stderr)
}

func (f *nativePipeDaemon) instanceID(t *testing.T, credential string) string {
	t.Helper()
	reply := f.mcpControl(t, protocol.MCPServiceRequest{Type: protocol.TypeMCPCatalog, RequestID: "ls", AppCredential: credential})
	var out struct {
		Services []mcphost.ServiceView `json:"services"`
	}
	if err := json.Unmarshal(reply.Result, &out); err != nil {
		t.Fatal(err)
	}
	for _, s := range out.Services {
		for _, inst := range s.Instances {
			if s.Name == "fixture" && inst.Mine {
				return inst.ID
			}
		}
	}
	t.Fatal("caller has no fixture instance")
	return ""
}

func (f *nativePipeDaemon) liveWorkers(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir(f.sessions)
	if err != nil {
		t.Fatal(err)
	}
	live := 0
	for _, entry := range entries {
		if _, err := os.Lstat(filepath.Join(f.sessions, entry.Name(), "sock")); err == nil {
			live++
		}
	}
	return live
}

func member(raw []byte, name string) json.RawMessage {
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	return fields[name]
}

// TestHostedMCPLargeAndSlow measures section 11's large-reply and
// slow-consumer cases. It records numbers and asserts only outcomes.
func TestHostedMCPLargeAndSlow(t *testing.T) {
	f := newNativePipeDaemon(t)
	f.publishMCP(t, mcphost.Definition{Name: "fixture", Argv: []string{f.executable, mcpFixtureRole}, Cwd: f.root, InstancePolicy: mcphost.PolicyShared})
	credential := newCredential(t)
	f.mcpCall(t, credential, "echo", `{"text":"warm"}`)
	const size = 3 << 20
	var samples []time.Duration
	for range 5 {
		start := time.Now()
		reply := f.mcpCall(t, credential, "big", fmt.Sprintf(`{"bytes":%d}`, size))
		samples = append(samples, time.Since(start))
		if len(resultText(t, reply)) != size {
			t.Fatal("3 MiB reply was truncated")
		}
	}
	t.Logf("3 MiB reply through one shared instance: %v", samples)

	// One caller stops reading while three others fetch 3 MiB replies.
	slow, slowReader := f.mcpStream(t, mcpframe.Route{Service: "fixture", AppCredential: newCredential(t)}, mcpToolCall(`1`, "big", fmt.Sprintf(`{"bytes":%d}`, size)))
	var wg sync.WaitGroup
	times := make([]time.Duration, 3)
	for i := range 3 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			start := time.Now()
			reply := f.mcpCall(t, newCredential(t), "big", fmt.Sprintf(`{"bytes":%d}`, size))
			times[i] = time.Since(start)
			if len(resultText(t, reply)) != size {
				t.Error("fast caller's reply was truncated")
			}
		}(i)
	}
	wg.Wait()
	t.Logf("three callers beside one stalled caller: %v", times)
	_ = slow.SetDeadline(time.Now().Add(30 * time.Second))
	if len(resultText(t, readMCP(t, slowReader)[0])) != size {
		t.Fatal("stalled caller's reply was lost")
	}
}
