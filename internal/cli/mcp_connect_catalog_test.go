package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/mcphost"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

func saveCacheForTest(t *testing.T, cache *mcpCatalogCache, entry mcpCatalogCacheEntry) {
	t.Helper()
	gen, err := cache.BeginObservation(t.Context(), entry.HostID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.SaveIfCurrent(t.Context(), entry, gen); err != nil {
		t.Fatal(err)
	}
}

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
	_, scopeA, err := resolveMCPListCaller("agent-a", localHost.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, scopeB, err := resolveMCPListCaller("agent-b", localHost.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Verify caller scopes use full 256-bit derived digests, not 48-bit fingerprints.
	if scopeA == scopeB || !strings.HasPrefix(scopeA, "app:") || len(scopeA) < 4+40 {
		t.Fatalf("expected distinct full derived caller scopes, got %q and %q", scopeA, scopeB)
	}

	baseNow := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	cache := openMCPCatalogCache(stateDir)

	// Pre-populate private cache for stale (2h old), expired (25h old), denied (1h old), and changed (1h old).
	saveCacheForTest(t, cache, mcpCatalogCacheEntry{
		HostID: hStale.ID, MeshIdentity: hStale.MeshIdentity, MachineName: hStale.MachineName, CallerScope: scopeA,
		ReceivedAt: baseNow.Add(-2 * time.Hour),
		Services: []mcphost.CatalogService{{
			ID: strings.Repeat("1", 32), Name: "cached-svc", Revision: 1, Transport: mcphost.TransportStdio,
			Lifecycle: mcphost.LifecycleOnDemand, InstancePolicy: mcphost.PolicyIsolated, AccountMode: mcphost.AccountNone,
		}},
	})
	saveCacheForTest(t, cache, mcpCatalogCacheEntry{
		HostID: hExpired.ID, MeshIdentity: hExpired.MeshIdentity, MachineName: hExpired.MachineName, CallerScope: scopeA,
		ReceivedAt: baseNow.Add(-25 * time.Hour),
		Services: []mcphost.CatalogService{{
			ID: strings.Repeat("2", 32), Name: "old-svc", Revision: 1, Transport: mcphost.TransportStdio,
			Lifecycle: mcphost.LifecycleOnDemand, InstancePolicy: mcphost.PolicyIsolated, AccountMode: mcphost.AccountNone,
		}},
	})
	saveCacheForTest(t, cache, mcpCatalogCacheEntry{
		HostID: hDenied.ID, MeshIdentity: hDenied.MeshIdentity, MachineName: hDenied.MachineName, CallerScope: scopeA,
		ReceivedAt: baseNow.Add(-1 * time.Hour),
		Services: []mcphost.CatalogService{{
			ID: strings.Repeat("3", 32), Name: "secret-svc", Revision: 1, Transport: mcphost.TransportStdio,
			Lifecycle: mcphost.LifecycleOnDemand, InstancePolicy: mcphost.PolicyIsolated, AccountMode: mcphost.AccountNone,
		}},
	})
	// Also seed caller B on hDenied: explicit device refusal MUST invalidate EVERY caller scope on this host.
	saveCacheForTest(t, cache, mcpCatalogCacheEntry{
		HostID: hDenied.ID, MeshIdentity: hDenied.MeshIdentity, MachineName: hDenied.MachineName, CallerScope: scopeB,
		ReceivedAt: baseNow.Add(-1 * time.Hour),
		Services: []mcphost.CatalogService{{
			ID: strings.Repeat("3", 32), Name: "secret-svc-b", Revision: 1, Transport: mcphost.TransportStdio,
			Lifecycle: mcphost.LifecycleOnDemand, InstancePolicy: mcphost.PolicyIsolated, AccountMode: mcphost.AccountNone,
		}},
	})
	saveCacheForTest(t, cache, mcpCatalogCacheEntry{
		HostID: hPinMismatch.ID, MeshIdentity: hPinMismatch.MeshIdentity, MachineName: hPinMismatch.MachineName, CallerScope: scopeA,
		ReceivedAt: baseNow.Add(-1 * time.Hour),
		Services: []mcphost.CatalogService{{
			ID: strings.Repeat("4", 32), Name: "pin-svc", Revision: 1, Transport: mcphost.TransportStdio,
			Lifecycle: mcphost.LifecycleOnDemand, InstancePolicy: mcphost.PolicyIsolated, AccountMode: mcphost.AccountNone,
		}},
	})

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
			views := []mcphost.CatalogService{
				{
					ID: strings.Repeat("a", 32), Name: "ready-svc", Revision: 2, Transport: mcphost.TransportStdio,
					Lifecycle: mcphost.LifecycleOnDemand, InstancePolicy: mcphost.PolicyIsolated, AccountMode: mcphost.AccountNone,
					ProtocolVersion: "2026-07-28",
					Instances:       []mcphost.CatalogInstance{{ID: strings.Repeat("e", 32), State: mcphost.StateReady, Revision: 2, Generation: 1}},
				},
				{
					ID: strings.Repeat("b", 32), Name: "stopped-svc", Revision: 1, Transport: mcphost.TransportStdio,
					Lifecycle: mcphost.LifecycleOnDemand, InstancePolicy: mcphost.PolicyIsolated, AccountMode: mcphost.AccountNone,
				},
				{
					ID: strings.Repeat("c", 32), Name: "legacy-svc", Revision: 1, Transport: mcphost.TransportStdio,
					Lifecycle: mcphost.LifecycleOnDemand, InstancePolicy: mcphost.PolicyIsolated, AccountMode: mcphost.AccountNone,
					Instances: []mcphost.CatalogInstance{{ID: strings.Repeat("f", 32), State: mcphost.StateIncompatible, Reason: "refused server/discover"}},
				},
				{
					ID: strings.Repeat("d", 32), Name: "broken-svc", Revision: 1, Transport: mcphost.TransportStdio,
					Lifecycle: mcphost.LifecycleOnDemand, InstancePolicy: mcphost.PolicyIsolated, AccountMode: mcphost.AccountNone,
					Instances: []mcphost.CatalogInstance{{ID: strings.Repeat("0", 32), State: mcphost.StateFailed, Reason: "exit status 127"}},
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

	// Verify explicit device refusal invalidated BOTH caller A and caller B caches for hDenied.
	if entry, _ := cache.Read(hDenied.ID, hDenied.MeshIdentity, scopeA); entry != nil {
		t.Fatalf("denied host caller A cache was not invalidated: %+v", entry)
	}
	if entry, _ := cache.Read(hDenied.ID, hDenied.MeshIdentity, scopeB); entry != nil {
		t.Fatalf("denied host caller B cache was not invalidated on device refusal: %+v", entry)
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
	if strings.Contains(stdoutB, "cached-svc") || strings.Contains(stdoutB, "ready-svc") || strings.Contains(stdoutB, "secret-svc-b") {
		t.Fatalf("caller B saw unauthorized or caller A private cache: %s", stdoutB)
	}
}

func TestMCPCatalogStripsUnprojectedFieldsFromOutputAndDiskCache(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MESH_CONFIG_DIR", filepath.Join(root, "config"))
	stateDir := filepath.Join(root, "state")
	t.Setenv("MESH_STATE_DIR", stateDir)
	if _, _, err := identity.LoadOrCreate(stateDir); err != nil {
		t.Fatal(err)
	}
	remoteID, _, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	host := HostRecord{
		ID: remoteID.ID, MeshIdentity: remoteID.ID, MachineName: "sentinel-box",
		NameRevision: 1, NameVerified: true, Endpoint: "ws://127.0.0.1:1/mesh",
	}
	if err := SaveHost(host); err != nil {
		t.Fatal(err)
	}

	// Simulate a raw host reply carrying forbidden operational fields and arbitrary upstream metadata.
	rawServices := json.RawMessage(`[{
		"id": "` + strings.Repeat("a", 32) + `",
		"name": "repo-tools",
		"revision": 1,
		"transport": "stdio",
		"instancePolicy": "isolated",
		"lifecycle": "on-demand",
		"accountMode": "host-account",
		"accountLabel": "safe-bot",
		"protocolVersion": "2026-07-28",
		"command": ["/usr/bin/tool", "--token=SYNTHETIC_ARGV_SENTINEL"],
		"cwd": "/srv/SYNTHETIC_CWD_SENTINEL",
		"url": "http://127.0.0.1:9999/SYNTHETIC_URL_SENTINEL",
		"envNames": ["SYNTHETIC_ENV_SENTINEL"],
		"secretNames": ["SYNTHETIC_SECRET_SENTINEL"],
		"headerNames": ["SYNTHETIC_HEADER_SENTINEL"],
		"instances": [{
			"id": "` + strings.Repeat("b", 32) + `",
			"state": "ready",
			"revision": 1,
			"generation": 1,
			"serverInfo": {"name": "repo-srv", "version": "1.2", "x-hidden": "SYNTHETIC_META_SENTINEL"},
			"capabilities": {"tools": {"listChanged": true}, "x-vendor": "SYNTHETIC_CAP_SENTINEL"}
		}]
	}]`)

	dial := func(ctx context.Context, h HostRecord) (transport.Conn, error) {
		return fakeRawMCPHostConn(ctx, t, h, rawServices, ""), nil
	}

	stdout, _, err := executeCommand(t, Dependencies{DialPassive: dial}, "mcp", "ls", "--host", host.ID, "--json")
	if err != nil {
		t.Fatalf("mcp ls: %v\n%s", err, stdout)
	}

	sentinels := []string{
		"SYNTHETIC_ARGV_SENTINEL", "SYNTHETIC_CWD_SENTINEL", "SYNTHETIC_URL_SENTINEL",
		"SYNTHETIC_ENV_SENTINEL", "SYNTHETIC_SECRET_SENTINEL", "SYNTHETIC_HEADER_SENTINEL",
		"SYNTHETIC_META_SENTINEL", "SYNTHETIC_CAP_SENTINEL",
	}
	for _, s := range sentinels {
		if strings.Contains(stdout, s) {
			t.Fatalf("mcp ls JSON leaked %q:\n%s", s, stdout)
		}
	}

	// Inspect every file on disk under mcp-catalog-cache to ensure sentinels are never persisted.
	cacheDir := filepath.Join(stateDir, mcpCatalogCacheDirName)
	cacheRoot, err := os.OpenRoot(cacheDir)
	if err != nil {
		t.Fatalf("open cache root: %v", err)
	}
	defer func() { _ = cacheRoot.Close() }()
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		t.Fatalf("read cache dir: %v", err)
	}
	var checked int
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		raw, err := cacheRoot.ReadFile(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		checked++
		for _, s := range sentinels {
			if strings.Contains(string(raw), s) {
				t.Fatalf("cache file %s persisted sentinel %q:\n%s", entry.Name(), s, string(raw))
			}
		}
	}
	if checked == 0 {
		t.Fatal("expected at least one catalog cache file written")
	}
}

func TestMCPCatalogAuthInterruptedVsAppDeniedAndPinPruningInAllModes(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MESH_CONFIG_DIR", filepath.Join(root, "config"))
	stateDir := filepath.Join(root, "state")
	t.Setenv("MESH_STATE_DIR", stateDir)
	localHost, _, err := identity.LoadOrCreate(stateDir)
	if err != nil {
		t.Fatal(err)
	}

	makeRemote := func(name string) HostRecord {
		h, _, err := identity.LoadOrCreate(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		rec := HostRecord{
			ID: h.ID, MeshIdentity: h.ID, MachineName: name,
			NameRevision: 1, NameVerified: true, Endpoint: "ws://127.0.0.1:1/mesh",
		}
		if err := SaveHost(rec); err != nil {
			t.Fatal(err)
		}
		return rec
	}

	hInterrupt := makeRemote("interrupt-box")
	hAppDeny := makeRemote("appdeny-box")
	hPinRotate := makeRemote("pinrotate-box")

	if _, _, err := executeCommand(t, Dependencies{}, "mcp", "identity", "create", "agent-a", "--json"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := executeCommand(t, Dependencies{}, "mcp", "identity", "create", "agent-b", "--json"); err != nil {
		t.Fatal(err)
	}
	_, scopeA, err := resolveMCPListCaller("agent-a", localHost.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, scopeB, err := resolveMCPListCaller("agent-b", localHost.ID)
	if err != nil {
		t.Fatal(err)
	}

	baseNow := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	cache := openMCPCatalogCache(stateDir)
	seed := func(h HostRecord, scope, svcName string) {
		saveCacheForTest(t, cache, mcpCatalogCacheEntry{
			HostID: h.ID, MeshIdentity: h.MeshIdentity, MachineName: h.MachineName, CallerScope: scope,
			ReceivedAt: baseNow.Add(-30 * time.Minute),
			Services: []mcphost.CatalogService{{
				ID: strings.Repeat("5", 32), Name: svcName, Revision: 1, Transport: mcphost.TransportStdio,
				Lifecycle: mcphost.LifecycleOnDemand, InstancePolicy: mcphost.PolicyIsolated, AccountMode: mcphost.AccountNone,
			}},
		})
	}
	seed(hInterrupt, scopeA, "interrupt-svc")
	seed(hAppDeny, scopeA, "appdeny-svc-a")
	seed(hAppDeny, scopeB, "appdeny-svc-b")
	seed(hPinRotate, scopeA, "pinrotate-svc")

	// 1. Interrupted authentication (deadline during handshake) MUST keep stale cache, NOT discard or report unauthorized.
	dialInterrupt := func(ctx context.Context, h HostRecord) (transport.Conn, error) {
		if h.ID == hInterrupt.ID {
			return nil, fmt.Errorf("%w: TLS handshake: %w", transport.ErrAuthentication, context.DeadlineExceeded)
		}
		if h.ID == hAppDeny.ID {
			return fakeRawMCPHostConn(ctx, t, h, nil, "access.denied"), nil
		}
		return nil, errors.New("offline")
	}
	stdoutInt, _, errInt := executeCommand(t, Dependencies{
		DialPassive: dialInterrupt,
		Now:         func() time.Time { return baseNow },
	}, "mcp", "ls", "--host", hInterrupt.ID, "--identity", "agent-a", "--json")
	if status, ok := StatusCode(errInt); !ok || status != 3 {
		t.Fatalf("interrupted auth expected partial stale exit 3, got %v\n%s", errInt, stdoutInt)
	}
	if !strings.Contains(stdoutInt, `"state":"stale"`) || !strings.Contains(stdoutInt, "interrupt-svc") {
		t.Fatalf("interrupted auth should serve stale cache: %s", stdoutInt)
	}
	if entry, _ := cache.Read(hInterrupt.ID, hInterrupt.MeshIdentity, scopeA); entry == nil {
		t.Fatal("interrupted auth wrongly discarded cached entry")
	}

	// 2. Application-specific access denial invalidates ONLY caller A, preserving caller B's cache.
	stdoutApp, _, errApp := executeCommand(t, Dependencies{
		DialPassive: dialInterrupt,
		Now:         func() time.Time { return baseNow },
	}, "mcp", "ls", "--host", hAppDeny.ID, "--identity", "agent-a", "--json")
	if status, ok := StatusCode(errApp); !ok || status != 1 {
		t.Fatalf("app denial expected exit 1, got %v\n%s", errApp, stdoutApp)
	}
	if !strings.Contains(stdoutApp, `"state":"unauthorized"`) {
		t.Fatalf("app denial expected unauthorized host state: %s", stdoutApp)
	}
	if entry, _ := cache.Read(hAppDeny.ID, hAppDeny.MeshIdentity, scopeA); entry != nil {
		t.Fatal("app denial failed to invalidate caller A cache")
	}
	if entry, _ := cache.Read(hAppDeny.ID, hAppDeny.MeshIdentity, scopeB); entry == nil {
		t.Fatal("app denial wrongly invalidated caller B cache")
	}

	// 3. Pin change in hosts.json followed by --cached MUST prune old snapshot from disk immediately;
	//    restoring the old pin later MUST NOT resurrect it.
	oldPin := hPinRotate.MeshIdentity
	newIdentity, _, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hPinRotate.MeshIdentity = newIdentity.ID
	if err := SaveHost(hPinRotate); err != nil {
		t.Fatal(err)
	}
	_, _, _ = executeCommand(t, Dependencies{
		Now: func() time.Time { return baseNow },
	}, "mcp", "ls", "--host", hPinRotate.ID, "--cached", "--identity", "agent-a", "--json")
	// Switch pin back and verify old snapshot is gone from disk.
	hPinRotate.MeshIdentity = oldPin
	if err := SaveHost(hPinRotate); err != nil {
		t.Fatal(err)
	}
	if entry, _ := cache.Read(hPinRotate.ID, oldPin, scopeA); entry != nil {
		t.Fatalf("pin change under --cached did not remove stale-pin snapshot: %+v", entry)
	}

	// 4. Invalidation/prune failure (insecure cache dir mode) surfaces a state.unavailable diagnostic.
	cacheDir := filepath.Join(stateDir, mcpCatalogCacheDirName)
	if err := os.Chmod(cacheDir, 0o755); err != nil { //nolint:gosec // intentionally testing insecure mode rejection
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(cacheDir, 0o700) }() //nolint:gosec // restoring private directory mode
	stdoutPerm, _, _ := executeCommand(t, Dependencies{
		DialPassive: dialInterrupt,
		Now:         func() time.Time { return baseNow },
	}, "mcp", "ls", "--host", hAppDeny.ID, "--identity", "agent-a", "--json")
	if !strings.Contains(stdoutPerm, string(agentapi.StateUnavailable)) {
		t.Fatalf("expected state.unavailable diagnostic when invalidation fails: %s", stdoutPerm)
	}
}

func TestMCPCatalogCacheConcurrencyBudgetAntiResurrectionAndPermissions(t *testing.T) {
	stateDir := t.TempDir()
	cache := openMCPCatalogCache(stateDir)
	cache.maxTotal = 4096 // Tight budget to force concurrent eviction under lock.
	ctx := t.Context()
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

	// 1. Concurrent writers racing under tight budget never exceed cap and leave valid JSON.
	var wg sync.WaitGroup
	errs := make(chan error, 24)
	for i := range 24 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			entry := mcpCatalogCacheEntry{
				HostID:       fmt.Sprintf("host-%02d", idx),
				MeshIdentity: fmt.Sprintf("pin-%02d", idx),
				CallerScope:  "app:scope",
				ReceivedAt:   now.Add(time.Duration(idx) * time.Second),
				Services: []mcphost.CatalogService{{
					ID: strings.Repeat("a", 32), Name: fmt.Sprintf("svc-%02d", idx), Revision: 1,
					Transport: mcphost.TransportStdio, Lifecycle: mcphost.LifecycleOnDemand,
					InstancePolicy: mcphost.PolicyIsolated, AccountMode: mcphost.AccountNone,
					AccountLabel: strings.Repeat("x", 180),
				}},
			}
			gen, err := cache.BeginObservation(ctx, entry.HostID)
			if err != nil {
				errs <- err
				return
			}
			if _, err := cache.SaveIfCurrent(ctx, entry, gen); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent cache Save failed: %v", err)
	}

	cacheDir := filepath.Join(stateDir, mcpCatalogCacheDirName)
	cacheRoot, err := os.OpenRoot(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cacheRoot.Close() }()
	dirEntries, err := os.ReadDir(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, e := range dirEntries {
		if !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		total += info.Size()
		raw, err := cacheRoot.ReadFile(e.Name())
		if err != nil {
			t.Fatal(err)
		}
		var parsed mcpCatalogCacheEntry
		if err := json.Unmarshal(raw, &parsed); err != nil {
			t.Fatalf("corrupt cache file %s: %v", e.Name(), err)
		}
	}
	if total > cache.maxTotal {
		t.Fatalf("concurrent writes exceeded cache budget: %d > %d", total, cache.maxTotal)
	}

	// 2. Refresh-vs-invalidation race: advancing HostGen prevents SaveIfCurrent from resurrecting a deleted snapshot.
	raceCache := openMCPCatalogCache(t.TempDir())
	entry := mcpCatalogCacheEntry{
		HostID: "race-host", MeshIdentity: "race-pin", CallerScope: "app:a", ReceivedAt: now,
		Services: []mcphost.CatalogService{{ID: strings.Repeat("1", 32), Name: "svc", Revision: 1}},
	}
	saveCacheForTest(t, raceCache, entry)
	obsGen, err := raceCache.BeginObservation(ctx, "race-host")
	if err != nil {
		t.Fatal(err)
	}
	if err := raceCache.InvalidateHost(ctx, "race-host"); err != nil {
		t.Fatal(err)
	}
	published, err := raceCache.SaveIfCurrent(ctx, entry, obsGen)
	if err != nil {
		t.Fatalf("SaveIfCurrent error: %v", err)
	}
	if published {
		t.Fatal("stale refresh resurrected invalidated host snapshot")
	}
	if got, err := raceCache.Read("race-host", "race-pin", "app:a"); err != nil || got != nil {
		t.Fatalf("invalidated host snapshot still present: got=%+v err=%v", got, err)
	}

	// 3. Insecure permissions (0755 dir or 0644 file) are refused on Read and SaveIfCurrent.
	permState := t.TempDir()
	permCache := openMCPCatalogCache(permState)
	saveCacheForTest(t, permCache, entry)
	entryPath := filepath.Join(permState, mcpCatalogCacheDirName, mcpCatalogCacheKey(entry.HostID, entry.MeshIdentity, entry.CallerScope))
	if err := os.Chmod(entryPath, 0o644); err != nil { //nolint:gosec // intentionally testing insecure file mode
		t.Fatal(err)
	}
	if _, err := permCache.Read(entry.HostID, entry.MeshIdentity, entry.CallerScope); err == nil {
		t.Fatal("expected insecure 0644 cache file to be rejected on Read")
	}
	if err := os.Chmod(entryPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(permState, mcpCatalogCacheDirName), 0o755); err != nil { //nolint:gosec // intentionally testing insecure dir mode
		t.Fatal(err)
	}
	if _, err := permCache.SaveIfCurrent(ctx, entry, 0); err == nil {
		t.Fatal("expected insecure 0755 cache dir to be rejected on SaveIfCurrent")
	}
	if err := os.Chmod(filepath.Join(permState, mcpCatalogCacheDirName), 0o700); err != nil { //nolint:gosec // restoring private directory mode
		t.Fatal(err)
	}
}

func TestMCPListCachedNeverDialsSingleDeadlineAndTableExitCodes(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MESH_CONFIG_DIR", filepath.Join(root, "config"))
	stateDir := filepath.Join(root, "state")
	t.Setenv("MESH_STATE_DIR", stateDir)
	if _, _, err := identity.LoadOrCreate(stateDir); err != nil {
		t.Fatal(err)
	}
	makeRemote := func(name string) HostRecord {
		h, _, err := identity.LoadOrCreate(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		rec := HostRecord{
			ID: h.ID, MeshIdentity: h.ID, MachineName: name,
			NameRevision: 1, NameVerified: true, Endpoint: "ws://127.0.0.1:1/mesh",
		}
		if err := SaveHost(rec); err != nil {
			t.Fatal(err)
		}
		return rec
	}
	hFast := makeRemote("fast-box")
	hSlow := makeRemote("slow-box")

	// 1. --cached with an unknown --host MUST NOT dial any host (0 dials).
	var dials atomic.Int32
	recordingDial := func(ctx context.Context, _ HostRecord) (transport.Conn, error) {
		dials.Add(1)
		return nil, errors.New("should not dial in --cached mode")
	}
	_, _, err := executeCommand(t, Dependencies{DialPassive: recordingDial}, "mcp", "ls", "--cached", "--host", "nonexistent-name", "--json")
	if err == nil {
		t.Fatal("expected host-not-found error for unknown --host")
	}
	if dials.Load() != 0 {
		t.Fatalf("--cached dialed %d times during host resolution", dials.Load())
	}

	// 2. Single deadline: fast host completes while slow host blocks past --timeout 250ms.
	//    Both JSON and table modes MUST finish promptly, preserve fast-box's row, and exit 3!
	slowGate := make(chan struct{})
	defer close(slowGate)
	var startCount atomic.Int32
	timedDial := func(ctx context.Context, h HostRecord) (transport.Conn, error) {
		if h.ID == hFast.ID {
			return fakeMCPHostConn(ctx, t, h, true, []mcphost.CatalogService{{
				ID: strings.Repeat("a", 32), Name: "fast-svc", Revision: 1,
				Transport: mcphost.TransportStdio, Lifecycle: mcphost.LifecycleOnDemand,
				InstancePolicy: mcphost.PolicyShared, AccountMode: mcphost.AccountNone,
			}}, &startCount), nil
		}
		if h.ID == hSlow.ID {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-slowGate:
				return nil, errors.New("slow host closed")
			}
		}
		return nil, errors.New("local offline")
	}

	start := time.Now()
	tableOut, _, tableErr := executeCommand(t, Dependencies{DialPassive: timedDial}, "mcp", "ls", "--timeout", "250ms")
	elapsed := time.Since(start)
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("fleet mcp ls exceeded --timeout 250ms significantly: %v", elapsed)
	}
	if status, ok := StatusCode(tableErr); !ok || status != 3 {
		t.Fatalf("partial table output expected exit 3 matching JSON, got err=%v\n%s", tableErr, tableOut)
	}
	if !strings.Contains(tableOut, "fast-svc") {
		t.Fatalf("table output dropped completed fast-box row on deadline expiry:\n%s", tableOut)
	}

	// 3. All-failed table output returns exit 1 matching JSON.
	failDial := func(context.Context, HostRecord) (transport.Conn, error) {
		return nil, errors.New("offline")
	}
	_, _, failTableErr := executeCommand(t, Dependencies{DialPassive: failDial}, "mcp", "ls", "--host", hSlow.ID)
	if status, ok := StatusCode(failTableErr); !ok || status != 1 {
		t.Fatalf("all-failed table output expected exit 1 matching JSON, got err=%v", failTableErr)
	}
}

func TestResolveMCPInstanceArgPreservesFullHandleFor32CharServiceName(t *testing.T) {
	var raw [32]byte
	for i := range raw {
		raw[i] = byte(i + 1)
	}
	hostID := base64.RawURLEncoding.EncodeToString(raw[:])
	otherHostID := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	serviceID := strings.Repeat("a", 32)
	instanceID := strings.Repeat("b", 32)
	handle := "mi1." + hostID + "." + serviceID + "." + instanceID

	// A 32-character human service name must NOT be rejected when given a valid handle for its persisted serviceID.
	got, err := resolveMCPInstanceArg("mcp.connect", handle, hostID)
	if err != nil {
		t.Fatalf("valid handle for 32-char service name rejected: %v", err)
	}
	if got != handle {
		t.Fatalf("handle = %q, want full handle %q passed verbatim to owning host", got, handle)
	}

	// Wrong host handle is rejected locally.
	wrongHost := "mi1." + otherHostID + "." + serviceID + "." + instanceID
	if _, err := resolveMCPInstanceArg("mcp.connect", wrongHost, hostID); err == nil {
		t.Fatal("expected wrong-host mi1 handle to be rejected")
	}

	// Malformed mi1 handle is rejected locally.
	if _, err := resolveMCPInstanceArg("mcp.connect", "mi1.bad.handle", hostID); err == nil {
		t.Fatal("expected malformed mi1 handle to be rejected")
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

func fakeMCPHostConn(ctx context.Context, t *testing.T, host HostRecord, mcpSupported bool, views []mcphost.CatalogService, startCount *atomic.Int32) transport.Conn {
	t.Helper()
	if !mcpSupported {
		return fakeRawMCPHostConn(ctx, t, host, nil, "control.unknown")
	}
	raw, _ := json.Marshal(views)
	return fakeRawMCPHostConnWithCounter(ctx, t, host, raw, "", true, startCount)
}

func fakeRawMCPHostConn(ctx context.Context, t *testing.T, host HostRecord, rawServices json.RawMessage, errorCode string) transport.Conn {
	t.Helper()
	var counter atomic.Int32
	return fakeRawMCPHostConnWithCounter(ctx, t, host, rawServices, errorCode, errorCode != "control.unknown", &counter)
}

func fakeRawMCPHostConnWithCounter(ctx context.Context, t *testing.T, host HostRecord, rawServices json.RawMessage, errorCode string, mcpSupported bool, startCount *atomic.Int32) transport.Conn {
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
				if !handleFakeMCPControl(serverConn, host, kind, frame, rawServices, errorCode, startCount) {
					return
				}
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

func handleFakeMCPControl(serverConn transport.Conn, host HostRecord, kind string, frame protocol.Frame, rawServices json.RawMessage, errorCode string, startCount *atomic.Int32) bool {
	req, err := protocol.DecodeMCPServiceRequest(frame.Payload)
	if err != nil {
		return false
	}
	if kind == protocol.TypeMCPServiceStart {
		startCount.Add(1)
	}
	if errorCode != "" {
		reply, _ := protocol.EncodeMCP(protocol.MCPServiceResponse{
			Type: protocol.TypeError, RequestID: req.RequestID, ErrorCode: errorCode, Message: errorCode,
		})
		_ = serverConn.WriteFrame(reply)
		return true
	}
	if len(rawServices) == 0 {
		rawServices = json.RawMessage(`[]`)
	}
	raw, _ := json.Marshal(map[string]any{"hostId": host.ID, "services": rawServices})
	reply, _ := protocol.EncodeMCP(protocol.MCPServiceResponse{
		Type: protocol.TypeMCPServiceResult, RequestID: req.RequestID, Result: raw,
	})
	_ = serverConn.WriteFrame(reply)
	return true
}
