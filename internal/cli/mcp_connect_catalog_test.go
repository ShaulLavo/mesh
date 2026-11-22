package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/mcphost"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

func TestMCPFleetCatalogStatesAndScopedCacheInvalidation(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	stateDir := filepath.Join(root, "state")
	t.Setenv("MESH_CONFIG_DIR", configDir)
	t.Setenv("MESH_STATE_DIR", stateDir)

	localHost, _, err := identity.LoadOrCreate(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	// Create 6 distinct saved remote host identities to exercise every catalog state.
	makeRemote := func(name string) HostRecord {
		h, _, err := identity.LoadOrCreate(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		rec := HostRecord{
			ID:           h.ID,
			MeshIdentity: h.ID,
			MachineName:  name,
			NameRevision: 1,
			NameVerified: true,
			Endpoint:     "ws://127.0.0.1:1/mesh",
		}
		if err := SaveHost(rec); err != nil {
			t.Fatal(err)
		}
		return rec
	}

	hOnline := makeRemote("online-box")
	hOffline := makeRemote("offline-box")
	hStale := makeRemote("stale-box")
	hExpired := makeRemote("expired-box")
	hDenied := makeRemote("denied-box")
	hUnsupported := makeRemote("legacy-box")
	hPinMismatch := makeRemote("changed-box")

	// Create two application identities to verify private cache isolation by caller.
	if _, _, err := executeCommand(t, Dependencies{}, "mcp", "identity", "create", "agent-a", "--json"); err != nil {
		t.Fatalf("create agent-a: %v", err)
	}
	if _, _, err := executeCommand(t, Dependencies{}, "mcp", "identity", "create", "agent-b", "--json"); err != nil {
		t.Fatalf("create agent-b: %v", err)
	}
	idA, err := loadApplicationIdentity("mcp.list", "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	scopeA := mcpCallerScope(localHost.ID, "app:"+idA.Credential.Fingerprint())

	baseNow := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	cache := openMCPCatalogCache(stateDir)

	// Pre-populate private cache for stale (2h old), expired (25h old), denied (1h old), and changed (1h old).
	if err := cache.Save(mcpCatalogCacheEntry{
		HostID: hStale.ID, MeshIdentity: hStale.MeshIdentity, MachineName: hStale.MachineName, CallerScope: scopeA,
		ReceivedAt: baseNow.Add(-2 * time.Hour),
		Services: []mcphost.ServiceView{{
			ID: strings.Repeat("1", 32), Name: "cached-svc", Revision: 1, Transport: mcphost.TransportStdio,
			Lifecycle: mcphost.LifecycleOnDemand, InstancePolicy: mcphost.PolicyIsolated, AccountMode: mcphost.AccountNone,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := cache.Save(mcpCatalogCacheEntry{
		HostID: hExpired.ID, MeshIdentity: hExpired.MeshIdentity, MachineName: hExpired.MachineName, CallerScope: scopeA,
		ReceivedAt: baseNow.Add(-25 * time.Hour),
		Services: []mcphost.ServiceView{{
			ID: strings.Repeat("2", 32), Name: "old-svc", Revision: 1, Transport: mcphost.TransportStdio,
			Lifecycle: mcphost.LifecycleOnDemand, InstancePolicy: mcphost.PolicyIsolated, AccountMode: mcphost.AccountNone,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := cache.Save(mcpCatalogCacheEntry{
		HostID: hDenied.ID, MeshIdentity: hDenied.MeshIdentity, MachineName: hDenied.MachineName, CallerScope: scopeA,
		ReceivedAt: baseNow.Add(-1 * time.Hour),
		Services: []mcphost.ServiceView{{
			ID: strings.Repeat("3", 32), Name: "secret-svc", Revision: 1, Transport: mcphost.TransportStdio,
			Lifecycle: mcphost.LifecycleOnDemand, InstancePolicy: mcphost.PolicyIsolated, AccountMode: mcphost.AccountNone,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := cache.Save(mcpCatalogCacheEntry{
		HostID: hPinMismatch.ID, MeshIdentity: hPinMismatch.MeshIdentity, MachineName: hPinMismatch.MachineName, CallerScope: scopeA,
		ReceivedAt: baseNow.Add(-1 * time.Hour),
		Services: []mcphost.ServiceView{{
			ID: strings.Repeat("4", 32), Name: "pin-svc", Revision: 1, Transport: mcphost.TransportStdio,
			Lifecycle: mcphost.LifecycleOnDemand, InstancePolicy: mcphost.PolicyIsolated, AccountMode: mcphost.AccountNone,
		}},
	}); err != nil {
		t.Fatal(err)
	}

	var startCount atomic.Int32
	dial := func(ctx context.Context, host HostRecord) (transport.Conn, error) {
		switch host.ID {
		case localHost.ID, hOffline.ID, hStale.ID, hExpired.ID:
			return nil, errors.New("connection refused")
		case hDenied.ID:
			return nil, transport.ErrAuthentication
		case hPinMismatch.ID:
			return nil, transport.ErrDestinationIdentityMismatch
		case hUnsupported.ID:
			return fakeMCPHostConn(ctx, t, host, false, nil, &startCount), nil
		case hOnline.ID:
			views := []mcphost.ServiceView{
				{
					ID: strings.Repeat("a", 32), Name: "ready-svc", Revision: 2, Transport: mcphost.TransportStdio,
					Lifecycle: mcphost.LifecycleOnDemand, InstancePolicy: mcphost.PolicyIsolated, AccountMode: mcphost.AccountNone,
					Instances: []mcphost.InstanceView{{ID: strings.Repeat("e", 32), State: mcphost.StateReady, Revision: 2, Generation: 1}},
				},
				{
					ID: strings.Repeat("b", 32), Name: "stopped-svc", Revision: 1, Transport: mcphost.TransportStdio,
					Lifecycle: mcphost.LifecycleOnDemand, InstancePolicy: mcphost.PolicyIsolated, AccountMode: mcphost.AccountNone,
				},
				{
					ID: strings.Repeat("c", 32), Name: "legacy-svc", Revision: 1, Transport: mcphost.TransportStdio,
					Lifecycle: mcphost.LifecycleOnDemand, InstancePolicy: mcphost.PolicyIsolated, AccountMode: mcphost.AccountNone,
					Instances: []mcphost.InstanceView{{ID: strings.Repeat("f", 32), State: mcphost.StateIncompatible, Reason: "refused server/discover"}},
				},
				{
					ID: strings.Repeat("d", 32), Name: "broken-svc", Revision: 1, Transport: mcphost.TransportStdio,
					Lifecycle: mcphost.LifecycleOnDemand, InstancePolicy: mcphost.PolicyIsolated, AccountMode: mcphost.AccountNone,
					Instances: []mcphost.InstanceView{{ID: strings.Repeat("0", 32), State: mcphost.StateFailed, Reason: "exit status 127"}},
				},
			}
			return fakeMCPHostConn(ctx, t, host, true, views, &startCount), nil
		default:
			return nil, errors.New("unexpected host")
		}
	}

	stdout, stderr, err := executeCommand(t, Dependencies{
		DialPassive: dial,
		Now:         func() time.Time { return baseNow },
	}, "mcp", "ls", "--identity", "agent-a", "--json")
	if stderr != "" {
		t.Fatalf("unexpected stderr: %q", stderr)
	}
	if status, ok := StatusCode(err); !ok || status != 3 {
		t.Fatalf("expected partial exit 3, got err=%v stdout=%s", err, stdout)
	}
	if startCount.Load() != 0 {
		t.Fatalf("mcp ls started a service (%d starts)", startCount.Load())
	}

	var env struct {
		SchemaVersion int                     `json:"schemaVersion"`
		Operation     string                  `json:"operation"`
		Status        agentapi.Status         `json:"status"`
		Data          agentapi.MCPCatalogData `json:"data"`
		Errors        []agentapi.Error        `json:"errors"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("decode envelope: %v\n%s", err, stdout)
	}
	if env.SchemaVersion != 1 || env.Operation != "mcp.list" || env.Status != agentapi.StatusPartial {
		t.Fatalf("unexpected envelope header: %+v", env)
	}

	hostState := map[string]string{}
	for _, h := range env.Data.Hosts {
		hostState[h.HostID] = h.State
	}
	wantHostStates := map[string]string{
		hOnline.ID:      agentapi.MCPHostOnline,
		hOffline.ID:     agentapi.MCPHostOffline,
		hStale.ID:       agentapi.MCPHostStale,
		hExpired.ID:     agentapi.MCPHostExpired,
		hDenied.ID:      agentapi.MCPHostUnauthorized,
		hUnsupported.ID: agentapi.MCPHostUnsupported,
		hPinMismatch.ID: agentapi.MCPHostIdentityChanged,
	}
	for id, want := range wantHostStates {
		if got := hostState[id]; got != want {
			t.Errorf("host %s state = %q, want %q", id, got, want)
		}
	}

	svcState := map[string]string{}
	for _, s := range env.Data.Services {
		svcState[s.Name] = s.State
	}
	for name, want := range map[string]string{
		"ready-svc":   mcphost.StateReady,
		"stopped-svc": mcphost.StateStopped,
		"legacy-svc":  mcphost.StateIncompatible,
		"broken-svc":  mcphost.StateFailed,
		"cached-svc":  agentapi.MCPHostStale,
	} {
		if got := svcState[name]; got != want {
			t.Errorf("service %s state = %q, want %q", name, got, want)
		}
	}
	for _, absent := range []string{"old-svc", "secret-svc", "pin-svc"} {
		if _, exists := svcState[absent]; exists {
			t.Errorf("service %s should not be advertised after expiry/denial/pin change", absent)
		}
	}

	// Verify explicit denial and pin change IMMEDIATELY removed protected cache files.
	if entry, _ := cache.Read(hDenied.ID, hDenied.MeshIdentity, scopeA); entry != nil {
		t.Fatalf("denied host cache was not invalidated: %+v", entry)
	}
	if entry, _ := cache.Read(hPinMismatch.ID, hPinMismatch.MeshIdentity, scopeA); entry != nil {
		t.Fatalf("pin-changed host cache was not invalidated: %+v", entry)
	}

	// Verify caller B cannot see caller A's cached entries for hStale or hOnline.
	stdoutB, _, errB := executeCommand(t, Dependencies{
		DialPassive: dial,
		Now:         func() time.Time { return baseNow },
	}, "mcp", "ls", "--identity", "agent-b", "--cached", "--json")
	if status, ok := StatusCode(errB); !ok || status != 1 {
		t.Fatalf("expected error exit 1 for uncached caller B, got %v stdout=%s", errB, stdoutB)
	}
	if strings.Contains(stdoutB, "cached-svc") || strings.Contains(stdoutB, "ready-svc") {
		t.Fatalf("caller B saw caller A's private cache: %s", stdoutB)
	}
}

func TestMCPListDoesNotCreateMissingState(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "absent-state")
	t.Setenv("MESH_CONFIG_DIR", filepath.Join(root, "absent-config"))
	t.Setenv("MESH_STATE_DIR", state)

	stdout, stderr, err := executeCommand(t, Dependencies{}, "mcp", "ls", "--json")
	if err != nil || stderr != "" || !strings.Contains(stdout, `"services":[]`) {
		t.Fatalf("empty fleet mcp ls: out=%q err=%q rv=%v", stdout, stderr, err)
	}
	if _, statErr := os.Lstat(state); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("passive mcp ls created missing state dir: %v", statErr)
	}
}

func fakeMCPHostConn(ctx context.Context, t *testing.T, host HostRecord, mcpSupported bool, views []mcphost.ServiceView, startCount *atomic.Int32) transport.Conn {
	t.Helper()
	clientPipe, serverPipe := net.Pipe()
	clientConn, err := transport.NewStreamConn(clientPipe)
	if err != nil {
		t.Fatal(err)
	}
	serverConn, err := transport.NewStreamConn(serverPipe)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		defer func() { _ = serverConn.Close() }()
		for {
			if ctx.Err() != nil {
				return
			}
			frame, err := serverConn.ReadFrame()
			if err != nil {
				return
			}
			if kind, ok := protocol.MCPControlType(frame); ok {
				req, err := protocol.DecodeMCPServiceRequest(frame.Payload)
				if err != nil {
					return
				}
				if kind == protocol.TypeMCPServiceStart {
					startCount.Add(1)
				}
				if !mcpSupported {
					reply, _ := protocol.EncodeMCP(protocol.MCPServiceResponse{
						Type: protocol.TypeError, RequestID: req.RequestID, ErrorCode: "control.unknown", Message: "unsupported",
					})
					_ = serverConn.WriteFrame(reply)
					continue
				}
				raw, _ := json.Marshal(map[string]any{"hostId": host.ID, "services": views})
				reply, _ := protocol.EncodeMCP(protocol.MCPServiceResponse{
					Type: protocol.TypeMCPServiceResult, RequestID: req.RequestID, Result: raw,
				})
				_ = serverConn.WriteFrame(reply)
				continue
			}
			ctrl, err := protocol.DecodeControl(frame.Payload)
			if err != nil {
				return
			}
			if ctrl.Type == protocol.TypeHostInfo {
				info := protocol.HostInfo{
					ID:                  host.ID,
					MeshIdentity:        host.MeshIdentity,
					MachineName:         host.MachineName,
					NameRevision:        host.NameRevision,
					MCPHostingSupported: mcpSupported,
				}
				frames, _ := protocol.ControlFrames(protocol.Control{Type: protocol.TypeHostInfoResult, RequestID: ctrl.RequestID, Host: &info})
				for _, f := range frames {
					_ = serverConn.WriteFrame(f)
				}
			}
		}
	}()
	return clientConn
}
