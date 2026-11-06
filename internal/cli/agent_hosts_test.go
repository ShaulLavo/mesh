package cli

import (
	"context"
	"encoding/json"
	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/hostprofile"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/transport"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAgentHostsUninitializedDoesNotCreateState(t *testing.T) {
	root := t.TempDir()
	state, config := filepath.Join(root, "state"), filepath.Join(root, "config")
	t.Setenv("MESH_STATE_DIR", state)
	t.Setenv("MESH_CONFIG_DIR", config)
	stdout, stderr, err := executeCommand(t, Dependencies{}, "hosts", "--json")
	var result struct {
		Status string
		Data   struct {
			Hosts []struct {
				HostID       *string
				Reachability string
			}
		}
	}
	if decodeErr := json.Unmarshal([]byte(stdout), &result); decodeErr != nil {
		t.Fatalf("hosts did not return JSON: %q %v", stdout, decodeErr)
	}
	if code, ok := StatusCode(err); !ok || code != 3 || stderr != "" || result.Status != "partial" || len(result.Data.Hosts) != 1 || result.Data.Hosts[0].HostID != nil || result.Data.Hosts[0].Reachability != "uninitialized" {
		t.Fatalf("uninitialized: %s %q %v", stdout, stderr, err)
	}
	assertAgentAbsent(t, state, config)
}

func TestAgentHostCacheFullThenSummaryThenOffline(t *testing.T) {
	state := t.TempDir()
	t.Setenv("MESH_STATE_DIR", state)
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	if _, _, err := identity.LoadOrCreate(state); err != nil {
		t.Fatal(err)
	}
	host := HostRecord{ID: argumentIdentities(t)[1], MeshIdentity: argumentIdentities(t)[1], Endpoint: "ws://127.0.0.1:1/control/ws", MachineName: "pc", NameRevision: 1}
	if err := saveNamedTestHost(t, host); err != nil {
		t.Fatal(err)
	}
	config := t.TempDir()
	store := hostprofile.OpenHostStore(config, host.ID)
	if _, _, err := store.Set(t.Context(), 0, hostprofile.Editable{SchemaVersion: 1, Instructions: "full instructions"}); err != nil {
		t.Fatal(err)
	}
	var reads atomic.Int32
	dial := agentContextFixtureDial(t, host, config, t.TempDir(), &reads)
	first := runHostDetail(t, Dependencies{DialPassive: dial}, 0, "inspect", host.ID, "--json")
	if first.Host.Declared.Host.Profile == nil || *first.Host.Declared.Host.Profile.Instructions != "full instructions" || first.Access.Grant != "full-device" || first.Access.ObservedAt == nil {
		t.Fatal(first)
	}
	stdout, _, err := executeCommand(t, Dependencies{DialPassive: dial}, "hosts", "--json")
	if err != nil {
		t.Fatal(err, stdout)
	}
	count := reads.Load()
	offline := func(context.Context, HostRecord) (transport.Conn, error) {
		t.Error("cached read dialed")
		return nil, context.Canceled
	}
	cached := runHostDetail(t, Dependencies{DialPassive: offline}, 0, "inspect", host.ID, "--json", "--cached")
	if cached.Host.Declared.Host.Profile == nil || *cached.Host.Declared.Host.Profile.Instructions != "full instructions" || cached.Host.Declared.Host.Freshness != agentapi.FreshnessCached || cached.Access.State != "unavailable" || cached.Access.Grant != "unknown" || cached.Access.ObservedAt != nil || reads.Load() != count {
		t.Fatal(cached)
	}
	if _, _, err := store.Set(t.Context(), 1, hostprofile.Editable{SchemaVersion: 1, Instructions: "changed instructions"}); err != nil {
		t.Fatal(err)
	}
	stdout, _, err = executeCommand(t, Dependencies{DialPassive: dial}, "hosts", "--json")
	if err != nil {
		t.Fatal(err, stdout)
	}
	incomplete := runHostDetail(t, Dependencies{DialPassive: offline}, 3, "inspect", host.ID, "--json", "--cached")
	if incomplete.Host.Declared.Host.State != "unavailable" || incomplete.Host.Declared.Host.Profile != nil || incomplete.Host.Declared.Host.Revision == nil || *incomplete.Host.Declared.Host.Revision != 2 {
		t.Fatal(incomplete)
	}
	assertAgentAbsent(t, filepath.Join(state, "s"), filepath.Join(state, catalogDatabaseName))
}

func agentContextFixtureDial(t *testing.T, host HostRecord, config, state string, reads *atomic.Int32) HostDialer {
	t.Helper()
	fake := serviceRemoteDial(host, func(request protocol.Control) protocol.Control {
		if request.Type != protocol.TypeHostContext {
			t.Errorf("unexpected control %s", request.Type)
			return protocol.Control{Type: protocol.TypeError}
		}
		reads.Add(1)
		options, err := hostprofile.ParseContextRequest(request.Context)
		if err != nil {
			t.Error(err)
		}
		data, err := json.Marshal(hostprofile.ReadContext(config, state, host.ID, func(string) bool { return true }, options.Full))
		if err != nil {
			t.Error(err)
		}
		return protocol.Control{Type: protocol.TypeHostContextResult, Context: data}
	})
	return func(ctx context.Context, target HostRecord) (transport.Conn, error) {
		if target.local {
			return dialAgentHost(ctx, target)
		}
		return fake(ctx, target)
	}
}

func runHostDetail(t *testing.T, dependencies Dependencies, exit int, args ...string) agentapi.HostDetail {
	t.Helper()
	stdout, stderr, err := executeCommand(t, dependencies, args...)
	code, ok := StatusCode(err)
	if err == nil {
		code, ok = 0, true
	}
	if !ok || code != exit || stderr != "" {
		t.Fatalf("%v: stdout=%s stderr=%q err=%v", args, stdout, stderr, err)
	}
	var result struct{ Data agentapi.HostDetail }
	decoder := json.NewDecoder(strings.NewReader(stdout))
	if err := decoder.Decode(&result); err != nil {
		t.Fatal(err, stdout)
	}
	return result.Data
}

func TestAgentHostsTimeoutAndUnsupportedAreTyped(t *testing.T) {
	state := t.TempDir()
	t.Setenv("MESH_STATE_DIR", state)
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	if _, _, err := identity.LoadOrCreate(state); err != nil {
		t.Fatal(err)
	}
	host := HostRecord{ID: argumentIdentities(t)[1], MeshIdentity: argumentIdentities(t)[1], Endpoint: "ws://127.0.0.1:1/control/ws"}
	if err := SaveHost(host); err != nil {
		t.Fatal(err)
	}
	unsupported := serviceRemoteDial(host, func(protocol.Control) protocol.Control {
		return protocol.Control{Type: protocol.TypeError, ErrorCode: protocol.ErrorCodeUnknownControl, Message: "unknown context"}
	})
	stdout, _, err := executeCommand(t, Dependencies{DialPassive: unsupported}, "inspect", host.ID, "--json")
	assertHostReadError(t, stdout, err, agentapi.ProtocolUnsupported, 1)
	blocked := func(ctx context.Context, _ HostRecord) (transport.Conn, error) { <-ctx.Done(); return nil, ctx.Err() }
	stdout, _, err = executeCommand(t, Dependencies{DialPassive: blocked}, "hosts", "--json", "--timeout=100ms")
	assertHostReadError(t, stdout, err, agentapi.HostUnreachable, 3)
	assertAgentAbsent(t, filepath.Join(state, "context-cache"), filepath.Join(state, "s"))
}

func assertHostReadError(t *testing.T, stdout string, err error, want agentapi.Code, exit int) {
	t.Helper()
	if code, ok := StatusCode(err); !ok || code != exit {
		t.Fatalf("exit: %v %s", err, stdout)
	}
	var result struct{ Errors []agentapi.Error }
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	for _, diagnostic := range result.Errors {
		if diagnostic.Code == want {
			return
		}
	}
	t.Fatalf("missing %s: %s", want, stdout)
}

func TestAgentHostsKeylessAndUsageDoNotInitialize(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MESH_STATE_DIR", filepath.Join(root, "state"))
	t.Setenv("MESH_CONFIG_DIR", filepath.Join(root, "config"))
	host := HostRecord{ID: argumentIdentities(t)[1], MeshIdentity: argumentIdentities(t)[1], Endpoint: "ws://127.0.0.1:1/control/ws"}
	if err := SaveHost(host); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := executeCommand(t, Dependencies{}, "inspect", host.ID, "--json")
	assertHostReadError(t, stdout, err, agentapi.HostUninitialized, 1)
	for _, args := range [][]string{{"hosts", "--json", "--timeout=99ms"}, {"inspect", "--json"}, {"inspect", host.ID, "--json", "--timeout=61s"}, {"hosts", "--json", "--privacy"}, {"hosts", "--json", "--bogus"}} {
		checkAgentHostUsage(t, args)
	}
	stdout, _, err = executeCommand(t, Dependencies{}, "inspect", "missing", "--json", "--cached")
	assertHostReadError(t, stdout, err, agentapi.HostUnknown, 1)
	assertAgentAbsent(t, filepath.Join(root, "state"))
}

func checkAgentHostUsage(t *testing.T, args []string) {
	t.Helper()
	stdout, _, err := executeCommand(t, Dependencies{}, args...)
	assertHostReadError(t, stdout, err, agentapi.UsageInvalid, 2)
}

func TestAgentHostsCacheFailureIsPartialOnce(t *testing.T) {
	state, config := t.TempDir(), t.TempDir()
	t.Setenv("MESH_STATE_DIR", state)
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	if _, _, err := identity.LoadOrCreate(state); err != nil {
		t.Fatal(err)
	}
	host := HostRecord{ID: argumentIdentities(t)[1], MeshIdentity: argumentIdentities(t)[1], Endpoint: "ws://127.0.0.1:1/control/ws"}
	if err := SaveHost(host); err != nil {
		t.Fatal(err)
	}
	if _, _, err := hostprofile.OpenHostStore(config, host.ID).Set(t.Context(), 0, hostprofile.Editable{SchemaVersion: 1, Instructions: "notes"}); err != nil {
		t.Fatal(err)
	}
	var reads atomic.Int32
	dial := agentContextFixtureDial(t, host, config, t.TempDir(), &reads)
	runHostDetail(t, Dependencies{DialPassive: dial}, 0, "inspect", host.ID, "--json")
	cache, err := os.ReadDir(filepath.Join(state, "context-cache"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range cache {
		damageContextCacheFile(t, state, entry.Name())
	}
	stdout, _, err := executeCommand(t, Dependencies{DialPassive: dial}, "inspect", host.ID, "--json")
	assertHostReadError(t, stdout, err, agentapi.StateUnavailable, 3)
	var result struct {
		Errors []agentapi.Error
		Data   agentapi.HostDetail
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Errors) != 1 || result.Data.Host.Declared.Host.Profile == nil || *result.Data.Host.Declared.Host.Profile.Instructions != "notes" || result.Data.Access.Grant != "full-device" {
		t.Fatal(stdout)
	}
}

func damageContextCacheFile(t *testing.T, state, name string) {
	t.Helper()
	if !strings.HasSuffix(name, ".json") {
		return
	}
	if err := os.WriteFile(filepath.Join(state, "context-cache", name), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestAgentHostsRejectsInvalidObservation(t *testing.T) {
	host := HostRecord{ID: argumentIdentities(t)[1], MeshIdentity: argumentIdentities(t)[1]}
	zero := uint64(0)
	absent := hostprofile.WireSection{State: hostprofile.StateAbsent, Revision: &zero, Completeness: hostprofile.Full}
	data, err := json.Marshal(hostprofile.ContextReply{Host: absent, Fleet: absent})
	if err != nil {
		t.Fatal(err)
	}
	respond := func(request protocol.Control) protocol.Control {
		if request.Type == protocol.TypeHostInfo {
			return protocol.Control{Type: protocol.TypeHostInfoResult, Host: &protocol.HostInfo{ID: host.ID, MeshIdentity: host.MeshIdentity, Build: &release.Build{Version: "bad\x1bversion", Platform: release.Platform{OS: "linux", Arch: "amd64"}}}}
		}
		return protocol.Control{Type: protocol.TypeHostContextResult, Context: data}
	}
	dial := func(context.Context, HostRecord) (transport.Conn, error) {
		return newServiceRemoteTestConn(respond), nil
	}
	_, _, err = readAgentHostContext(t.Context(), host, true, dial)
	if err == nil {
		t.Fatal("unvalidated build observation accepted")
	}
}

func TestAgentHostsCachesValidatedNamesForCachedResolution(t *testing.T) {
	state := t.TempDir()
	t.Setenv("MESH_STATE_DIR", state)
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	if _, _, err := identity.LoadOrCreate(state); err != nil {
		t.Fatal(err)
	}
	host := HostRecord{ID: argumentIdentities(t)[1], MeshIdentity: argumentIdentities(t)[1], Endpoint: "ws://127.0.0.1:1/control/ws", MachineName: "pc", NameRevision: 1}
	if err := SaveHost(host); err != nil {
		t.Fatal(err)
	}
	var reads atomic.Int32
	dial := agentContextFixtureDial(t, host, t.TempDir(), t.TempDir(), &reads)
	runHostDetail(t, Dependencies{DialPassive: dial}, 0, "inspect", host.ID, "--json")
	offline := func(context.Context, HostRecord) (transport.Conn, error) {
		t.Error("cached resolution dialed")
		return nil, context.Canceled
	}
	detail := runHostDetail(t, Dependencies{DialPassive: offline}, 0, "inspect", "pc", "--json", "--cached")
	if detail.Host.HostID == nil || *detail.Host.HostID != host.ID || detail.Host.MachineName != "pc" {
		t.Fatal(detail)
	}
}
