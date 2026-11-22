package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/mcphost"
	"github.com/shaul/mesh/internal/mcpwire"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/protocol/mcpframe"
	"github.com/shaul/mesh/internal/tailnet"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/worker"
)

type remoteMCPDaemon struct {
	local    *nativePipeDaemon
	endpoint string
	options  transport.DialOptions
}

// startRemoteMCPDaemon runs a daemon in this process with a loopback
// control listener, so a second identity can reach it over mutual TLS.
func startRemoteMCPDaemon(t *testing.T) (*remoteMCPDaemon, identity.Host) {
	t.Helper()
	root := compactSocketTempDir(t)
	state := filepath.Join(root, "state")
	if err := os.MkdirAll(filepath.Join(state, "s"), 0o700); err != nil {
		t.Fatal(err)
	}
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Workers and the fixture server are this test binary in its fixture role.
	t.Setenv(pipeNativeFixture, root)
	host, _, err := identity.LoadOrCreate(state)
	if err != nil {
		t.Fatal(err)
	}
	device, key, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := identity.ApproveDevice(t.Context(), state, device.ID); err != nil {
		t.Fatal(err)
	}
	previous := mcpSweepInterval
	mcpSweepInterval = 100 * time.Millisecond
	listener, port := newTCPListener(t, "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	local := &nativePipeDaemon{root: root, state: state, sessions: filepath.Join(state, "s"), executable: bin}
	t.Cleanup(func() {
		local.stopWorkers(t)
		cancel()
		if err := waitRuntime(t, done); err != nil {
			t.Error(err)
		}
		mcpSweepInterval = previous
	})
	go func() {
		done <- run(ctx, Config{StateDir: state, ConfigDir: filepath.Join(root, "config"), TailnetPort: port}, runOptions{
			listen: useTCPListeners(listener), now: time.Now, bootID: worker.BootID,
			discoverSelf: func(context.Context) (tailnet.Peer, error) {
				return tailnet.Peer{Name: "mcp.example.ts.net", Addrs: []string{"127.0.0.1"}}, nil
			},
			discoverPeers: func(context.Context) ([]tailnet.Peer, error) { return nil, nil }, reconcileInterval: time.Hour,
		})
	}()
	conn := dialUnixRuntime(t, SocketPath(state))
	_ = conn.Close()
	return &remoteMCPDaemon{local: local, endpoint: fmt.Sprintf("ws://127.0.0.1:%d/mesh", port),
		options: transport.DialOptions{Auth: &transport.Authentication{Key: key, ExpectedIdentity: host.ID}}}, device
}

func (d *remoteMCPDaemon) dial(t *testing.T) transport.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	conn, err := transport.DialOnce(ctx, d.endpoint, d.options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// forward opens one remote H3 stream and returns it after the first frame.
func (d *remoteMCPDaemon) forward(t *testing.T, route mcpframe.Route, raw string) (transport.Conn, mcpwire.Message) {
	t.Helper()
	conn := d.dial(t)
	frame, err := mcpframe.EncodeForward(route, json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteFrame(frame); err != nil {
		t.Fatal(err)
	}
	reply, err := conn.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	_, message, err := mcpframe.Decode(reply)
	if err != nil {
		t.Fatalf("remote H3 frame %.300s: %v", reply.Payload, err)
	}
	return conn, message
}

func (d *remoteMCPDaemon) control(t *testing.T, req protocol.MCPServiceRequest) protocol.MCPServiceResponse {
	t.Helper()
	conn := d.dial(t)
	defer func() { _ = conn.Close() }()
	frame, err := protocol.EncodeMCP(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteFrame(frame); err != nil {
		t.Fatal(err)
	}
	reply, err := conn.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	var out protocol.MCPServiceResponse
	if err := json.Unmarshal(reply.Payload, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (d *remoteMCPDaemon) service(t *testing.T, name string) mcphost.ServiceView {
	t.Helper()
	reply := d.local.mcpControl(t, protocol.MCPServiceRequest{Type: protocol.TypeMCPCatalog, RequestID: "ls"})
	var out struct {
		Services []mcphost.ServiceView `json:"services"`
	}
	if err := json.Unmarshal(reply.Result, &out); err != nil {
		t.Fatal(err)
	}
	for _, s := range out.Services {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no service %s", name)
	return mcphost.ServiceView{}
}

// Revoking a full device and approving it again ends its live forwards and
// retires the old grant's lease and isolated instance; the shared instance
// keeps running and the re-approved device starts afresh.
func TestHostedMCPRemoteRevocationAndReapproval(t *testing.T) {
	d, device := startRemoteMCPDaemon(t)
	argv := []string{d.local.executable, mcpFixtureRole}
	d.local.publishMCP(t, mcphost.Definition{Name: "fixture", Argv: argv, Cwd: d.local.root})
	d.local.publishMCP(t, mcphost.Definition{Name: "shared", Argv: argv, Cwd: d.local.root, InstancePolicy: mcphost.PolicyShared})
	credential := newCredential(t)

	_, first := d.forward(t, mcpframe.Route{Service: "fixture", Caller: credential}, mcpToolCall(`"w"`, "whoami", `{}`))
	before := fixtureInstance(t, resultText(t, first))
	if reply := d.control(t, protocol.MCPServiceRequest{Type: protocol.TypeMCPServiceLease, RequestID: "lease", Service: "fixture", Caller: credential, LeaseTTLMillis: time.Hour.Milliseconds()}); reply.Type != protocol.TypeMCPServiceResult {
		t.Fatalf("remote lease: %+v", reply)
	}
	listen := `{"jsonrpc":"2.0","id":"L","method":"subscriptions/listen","params":{"notifications":{"toolsListChanged":true},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
	stream, ack := d.forward(t, mcpframe.Route{Service: "shared", Caller: credential}, listen)
	if ack.Method() != "notifications/subscriptions/acknowledged" {
		t.Fatalf("remote listen: %s", ack.Raw())
	}

	if err := identity.RevokeDevice(t.Context(), d.local.state, device.ID); err != nil {
		t.Fatal(err)
	}
	if err := identity.ApproveDevice(t.Context(), d.local.state, device.ID); err != nil {
		t.Fatal(err)
	}
	ended := make(chan error, 1)
	go func() { _, err := stream.ReadFrame(); ended <- err }()
	select {
	case err := <-ended:
		if err == nil {
			t.Fatal("the revoked listen received another frame")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the revoked listen stayed open")
	}
	pipeEventually(t, func() bool {
		inst := d.service(t, "fixture").Instances[0]
		return inst.State == mcphost.StateStopped && inst.Leases == 0
	})
	if inst := d.service(t, "shared").Instances[0]; inst.State != mcphost.StateReady {
		t.Fatalf("shared instance after revocation: %+v", inst)
	}

	_, again := d.forward(t, mcpframe.Route{Service: "fixture", Caller: credential}, mcpToolCall(`"w2"`, "whoami", `{}`))
	if after := fixtureInstance(t, resultText(t, again)); after == before {
		t.Fatal("the re-approved device reused the old grant's process")
	}
}
