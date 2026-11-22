package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/mcphost"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

func TestMCPPublishManagedHTTPProbeAndToggleCLI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateDir := t.TempDir()
	t.Setenv("MESH_STATE_DIR", stateDir)
	localID, _, err := identity.LoadOrCreate(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	host := HostRecord{ID: localID.ID, MeshIdentity: localID.ID, MachineName: "pc", local: true}

	var mu sync.Mutex
	var published mcphost.Definition
	var sawProbe, sawDisable, sawEnable bool
	disabled := false

	dial := func(ctx context.Context, _ HostRecord) (transport.Conn, error) {
		clientPipe, serverPipe := net.Pipe()
		clientConn, err := transport.NewStreamConn(clientPipe)
		if err != nil {
			return nil, fmt.Errorf("wrap client pipe: %w", err)
		}
		serverConn, err := transport.NewStreamConn(serverPipe)
		if err != nil {
			return nil, fmt.Errorf("wrap server pipe: %w", err)
		}
		go func() {
			defer func() { _ = serverConn.Close() }()
			for {
				frame, err := serverConn.ReadFrame()
				if err != nil {
					return
				}
				if kind, ok := protocol.MCPControlType(frame); ok {
					req, err := protocol.DecodeMCPServiceRequest(frame.Payload)
					if err != nil {
						return
					}
					mu.Lock()
					var payload any
					switch kind {
					case protocol.TypeMCPCatalog:
						var svcs []mcphost.CatalogService
						if published.Name != "" {
							svcs = append(svcs, mcphost.CatalogService{
								ID: "11111111111111111111111111111111", Name: published.Name, Revision: 1,
								Transport: mcphost.TransportHTTP, Lifecycle: mcphost.LifecycleOnDemand,
								InstancePolicy: mcphost.PolicyShared, AccountMode: published.AccountMode,
								AccountLabel: published.AccountLabel, Disabled: disabled,
								Instances: []mcphost.CatalogInstance{{ID: "22222222222222222222222222222222", State: mcphost.StateReady, Generation: 1}},
							})
						}
						payload = map[string]any{"hostId": host.ID, "services": svcs}
					case protocol.TypeMCPServiceUpsert:
						_ = json.Unmarshal(req.Definition, &published)
						payload = map[string]any{"service": mcphost.CatalogService{
							ID: "11111111111111111111111111111111", Name: published.Name, Revision: 1,
							Transport: mcphost.TransportHTTP, Lifecycle: mcphost.LifecycleOnDemand,
							InstancePolicy: mcphost.PolicyShared, AccountMode: published.AccountMode, AccountLabel: published.AccountLabel,
						}}
					case protocol.TypeMCPServiceProbe:
						sawProbe = true
						payload = map[string]any{"instance": mcphost.InstanceView{
							ID: "22222222222222222222222222222222", ServiceID: "11111111111111111111111111111111",
							Shared: true, State: mcphost.StateReady, Generation: 1,
						}}
					case protocol.TypeMCPServiceDisable:
						sawDisable, disabled = true, true
						payload = map[string]any{"service": mcphost.CatalogService{
							ID: "11111111111111111111111111111111", Name: published.Name, Revision: 2,
							Transport: mcphost.TransportHTTP, InstancePolicy: mcphost.PolicyShared, Disabled: true,
						}}
					case protocol.TypeMCPServiceEnable:
						sawEnable, disabled = true, false
						payload = map[string]any{"service": mcphost.CatalogService{
							ID: "11111111111111111111111111111111", Name: published.Name, Revision: 3,
							Transport: mcphost.TransportHTTP, InstancePolicy: mcphost.PolicyShared, Disabled: false,
						}}
					}
					mu.Unlock()
					raw, _ := json.Marshal(payload)
					reply, _ := protocol.EncodeMCP(protocol.MCPServiceResponse{Type: protocol.TypeMCPServiceResult, RequestID: req.RequestID, Result: raw})
					_ = serverConn.WriteFrame(reply)
					continue
				}
				ctrl, err := protocol.DecodeControl(frame.Payload)
				if err != nil {
					return
				}
				if ctrl.Type == protocol.TypeHostInfo {
					info := protocol.HostInfo{ID: host.ID, MeshIdentity: host.MeshIdentity, MachineName: host.MachineName, NameRevision: 1, MCPHostingSupported: true}
					frames, _ := protocol.ControlFrames(protocol.Control{Type: protocol.TypeHostInfoResult, RequestID: ctrl.RequestID, Host: &info})
					for _, f := range frames {
						_ = serverConn.WriteFrame(f)
					}
				}
			}
		}()
		return clientConn, nil
	}

	stdout, _, err := executeCommand(t, Dependencies{DialPassive: dial},
		"mcp", "publish", "managed-search",
		"--url", "http://127.0.0.1:8080/mcp", "--shared",
		"--account-mode", "host-account", "--account-label", "search bot",
		"--run", "./search-server", "--cwd", "/srv/search", "--probe", "--json")
	if err != nil {
		t.Fatalf("publish --probe failed: %v\n%s", err, stdout)
	}
	var env agentapi.Envelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil || env.Operation != "mcp.publish" || env.Status != agentapi.StatusOK {
		t.Fatalf("publish --probe envelope: %s (%v)", stdout, err)
	}
	mu.Lock()
	if !sawProbe || published.RunCwd != "/srv/search" || published.Cwd != "" || len(published.RunArgv) == 0 {
		t.Fatalf("published definition = %+v, sawProbe=%v", published, sawProbe)
	}
	mu.Unlock()

	if _, _, err := executeCommand(t, Dependencies{DialPassive: dial},
		"mcp", "publish", "bad", "--url", "http://127.0.0.1:8080/mcp", "--shared", "--run", "./x", "--cwd", "/a", "--run-cwd", "/b"); err == nil {
		t.Fatal("expected error when both --cwd and --run-cwd are passed with --run")
	}

	out, _, err := executeCommand(t, Dependencies{DialPassive: dial}, "mcp", "disable", "managed-search")
	if err != nil || !strings.Contains(out, "disabled managed-search") {
		t.Fatalf("disable output = %q, err = %v", out, err)
	}
	out, _, err = executeCommand(t, Dependencies{DialPassive: dial}, "mcp", "enable", "managed-search", "--json")
	if err != nil || !strings.Contains(out, `"operation":"mcp.enable"`) {
		t.Fatalf("enable --json output = %q, err = %v", out, err)
	}
	out, _, err = executeCommand(t, Dependencies{DialPassive: dial}, "mcp", "probe", "managed-search", "--json")
	if err != nil || !strings.Contains(out, `"operation":"mcp.probe"`) {
		t.Fatalf("probe --json output = %q, err = %v", out, err)
	}
	mu.Lock()
	if !sawDisable || !sawEnable {
		t.Fatalf("expected enable and disable calls: disable=%v enable=%v", sawDisable, sawEnable)
	}
	mu.Unlock()
}

// TestMCPHostedCommandsAcceptLeadingDashHostID proves that base64url canonical
// host IDs starting with '-' or '--' work as HOST/NAME operands (and --host
// values) across mesh mcp connect and every hosted MCP command without being
// parsed as Cobra flags.
func TestMCPHostedCommandsAcceptLeadingDashHostID(t *testing.T) {
	// argumentIdentities returns [no-dash, single-dash "-", double-dash "--"].
	ids := argumentIdentities(t)[1:]
	for _, hostID := range ids {
		t.Run(hostID[:2], func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("MESH_CONFIG_DIR", t.TempDir())
			stateDir := t.TempDir()
			t.Setenv("MESH_STATE_DIR", stateDir)
			if _, _, err := identity.LoadOrCreate(stateDir); err != nil {
				t.Fatal(err)
			}
			remote := HostRecord{ID: hostID, MeshIdentity: hostID, MachineName: "dash-host", Endpoint: "ws://127.0.0.1:7337/mesh"}
			if err := saveNamedTestHost(t, remote); err != nil {
				t.Fatal(err)
			}
			if _, _, err := executeCommand(t, Dependencies{}, "mcp", "identity", "create", "dash-id"); err != nil {
				t.Fatalf("create identity: %v", err)
			}

			var mu sync.Mutex
			seenControls := map[string]string{}
			dial := func(_ context.Context, target HostRecord) (transport.Conn, error) {
				if target.ID != hostID {
					return nil, fmt.Errorf("dialed host %q, want %q", target.ID, hostID)
				}
				clientPipe, serverPipe := net.Pipe()
				clientConn, err := transport.NewStreamConn(clientPipe)
				if err != nil {
					return nil, fmt.Errorf("wrap client pipe: %w", err)
				}
				serverConn, err := transport.NewStreamConn(serverPipe)
				if err != nil {
					return nil, fmt.Errorf("wrap server pipe: %w", err)
				}
				go func() {
					defer func() { _ = serverConn.Close() }()
					for {
						frame, err := serverConn.ReadFrame()
						if err != nil {
							return
						}
						if kind, ok := protocol.MCPControlType(frame); ok {
							req, err := protocol.DecodeMCPServiceRequest(frame.Payload)
							if err != nil {
								return
							}
							mu.Lock()
							seenControls[kind] = req.Service
							mu.Unlock()
							payload := dashHostMCPPayload(hostID, kind)
							raw, _ := json.Marshal(payload)
							reply, _ := protocol.EncodeMCP(protocol.MCPServiceResponse{Type: protocol.TypeMCPServiceResult, RequestID: req.RequestID, Result: raw})
							_ = serverConn.WriteFrame(reply)
							continue
						}
						ctrl, err := protocol.DecodeControl(frame.Payload)
						if err != nil {
							return
						}
						if ctrl.Type == protocol.TypeHostInfo {
							info := protocol.HostInfo{ID: hostID, MeshIdentity: hostID, MachineName: "dash-host", NameRevision: 1, MCPHostingSupported: true}
							frames, _ := protocol.ControlFrames(protocol.Control{Type: protocol.TypeHostInfoResult, RequestID: ctrl.RequestID, Host: &info})
							for _, f := range frames {
								_ = serverConn.WriteFrame(f)
							}
						}
					}
				}()
				return clientConn, nil
			}

			ref := hostID + "/repo-tools"
			// Exercise mesh mcp connect with HOST/NAME both before and after --identity.
			for _, args := range [][]string{
				{"mcp", "connect", ref, "--identity", "dash-id"},
				{"mcp", "connect", "--identity", "dash-id", ref},
			} {
				if out, errOut, err := executeCommand(t, Dependencies{DialPassive: dial}, args...); err != nil {
					t.Fatalf("mesh %s failed: %v\nstdout=%s\nstderr=%s", strings.Join(args, " "), err, out, errOut)
				}
			}

			// Exercise every hosted management command with HOST/NAME and --host.
			commands := [][]string{
				{"mcp", "ls", "--host", hostID, "--json"},
				{"mcp", "publish", "repo-tools", "--host", hostID, "--url", "http://127.0.0.1:8080/mcp", "--shared", "--json"},
				{"mcp", "show", ref, "--json"},
				{"mcp", "start", ref, "--json"},
				{"mcp", "stop", ref, "--json"},
				{"mcp", "restart", ref, "--json"},
				{"mcp", "probe", ref, "--json"},
				{"mcp", "enable", ref, "--json"},
				{"mcp", "disable", ref, "--json"},
				{"mcp", "logs", ref, "--json"},
				{"mcp", "rm", ref, "--json"},
			}
			for _, args := range commands {
				if out, errOut, err := executeCommand(t, Dependencies{DialPassive: dial}, args...); err != nil {
					t.Fatalf("mesh %s failed: %v\nstdout=%s\nstderr=%s", strings.Join(args, " "), err, out, errOut)
				}
			}
			for _, kind := range []string{
				protocol.TypeMCPCatalog,
				protocol.TypeMCPServiceUpsert,
				protocol.TypeMCPServiceStart,
				protocol.TypeMCPServiceStop,
				protocol.TypeMCPServiceRestart,
				protocol.TypeMCPServiceProbe,
				protocol.TypeMCPServiceEnable,
				protocol.TypeMCPServiceDisable,
				protocol.TypeMCPServiceLogs,
				protocol.TypeMCPServiceDelete,
			} {
				mu.Lock()
				_, ok := seenControls[kind]
				mu.Unlock()
				if !ok {
					t.Fatalf("expected control %s to reach leading-dash host %s", kind, hostID)
				}
			}
		})
	}
}

func dashHostMCPPayload(hostID, kind string) any {
	svc := mcphost.CatalogService{
		ID: "11111111111111111111111111111111", Name: "repo-tools", Revision: 1,
		Transport: mcphost.TransportHTTP, Lifecycle: mcphost.LifecycleOnDemand,
		InstancePolicy: mcphost.PolicyShared,
		Instances:      []mcphost.CatalogInstance{{ID: "22222222222222222222222222222222", State: mcphost.StateReady, Generation: 1}},
	}
	inst := mcphost.InstanceView{
		ID: "22222222222222222222222222222222", ServiceID: svc.ID, Shared: true, State: mcphost.StateReady, Generation: 1,
	}
	switch kind {
	case protocol.TypeMCPCatalog:
		return map[string]any{"hostId": hostID, "services": []mcphost.CatalogService{svc}}
	case protocol.TypeMCPServiceUpsert, protocol.TypeMCPServiceEnable, protocol.TypeMCPServiceDisable:
		return map[string]any{"service": svc}
	case protocol.TypeMCPServiceStop:
		return map[string]any{"instances": []mcphost.InstanceView{inst}}
	case protocol.TypeMCPServiceLogs:
		return map[string]any{"stderr": []byte("ok\n")}
	case protocol.TypeMCPServiceDelete:
		return map[string]any{}
	default:
		return map[string]any{"instance": inst}
	}
}
