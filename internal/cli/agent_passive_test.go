package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

func TestAgentPassiveMissingPaths(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	t.Setenv("MESH_STATE_DIR", state)
	t.Setenv("MESH_CONFIG_DIR", filepath.Join(root, "config"))
	dial := func(context.Context, HostRecord) (transport.Conn, error) {
		t.Error("keyless resolver dialed")
		return nil, context.Canceled
	}
	_, err := resolveAgentHost(t.Context(), "unknown", nil, false, dial)
	var failure *agentapi.Failure
	if !errors.As(err, &failure) || failure.Err.Code != agentapi.HostUnknown {
		t.Fatalf("unknown target: %v", err)
	}
	if _, err := List(); err != nil {
		t.Fatal(err)
	}
	cache, err := openAgentCatalogCache(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	assertAgentAbsent(t, state, filepath.Join(root, "config"))
}

func TestAgentPassiveExistingIdentityDoesNotCreateOptionalPaths(t *testing.T) {
	state := t.TempDir()
	t.Setenv("MESH_STATE_DIR", state)
	owner, _, err := identity.LoadOrCreate(state)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(state, "identity.key")) //nolint:gosec // compare private fixture bytes without printing
	if err != nil {
		t.Fatal(err)
	}
	host, err := resolveAgentHost(t.Context(), owner.ID, nil, true, nil)
	if err != nil || !host.local {
		t.Fatalf("local identity: %+v %v", host, err)
	}
	if _, err := List(); err != nil {
		t.Fatal(err)
	}
	cache, err := openAgentCatalogCache(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if conn, err := dialAgentHost(ctx, host); err == nil {
		_ = conn.Close()
		t.Fatal("missing daemon dial succeeded")
	}
	after, err := os.ReadFile(filepath.Join(state, "identity.key")) //nolint:gosec // compare private fixture bytes without printing
	if err != nil || string(before) != string(after) {
		t.Fatal("identity changed")
	}
	assertAgentAbsent(t, filepath.Join(state, "s"), filepath.Join(state, catalogDatabaseName), filepath.Join(state, "machine-name.json"))
}

func assertAgentAbsent(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("created optional path %s: %v", path, err)
		}
	}
}

func TestAgentPassiveCachedResolutionRejectsAmbiguityAndSession(t *testing.T) {
	state := filepath.Join(t.TempDir(), "missing")
	t.Setenv("MESH_STATE_DIR", state)
	ids := argumentIdentities(t)
	hosts := []HostRecord{{ID: ids[0], MeshIdentity: ids[0], MachineName: "session", NameRevision: 1}, {ID: ids[1], MeshIdentity: ids[1], MachineName: "session", NameRevision: 2}}
	host, err := resolveAgentHost(t.Context(), ids[0], hosts, true, nil)
	if err != nil || host.ID != ids[0] {
		t.Fatalf("exact identity: %+v %v", host, err)
	}
	_, err = resolveAgentHost(t.Context(), "session", hosts, true, nil)
	var failure *agentapi.Failure
	if !errors.As(err, &failure) || failure.Err.Code != agentapi.HostAmbiguous {
		t.Fatalf("ambiguous name: %v", err)
	}
	if _, err := resolveAgentHost(t.Context(), "7K3D", hosts, true, nil); err == nil {
		t.Fatal("accepted session as host")
	}
	assertAgentAbsent(t, state)
}

func TestAgentPassiveNameRefreshDoesNotWriteCaches(t *testing.T) {
	state := t.TempDir()
	t.Setenv("MESH_STATE_DIR", state)
	config := filepath.Join(t.TempDir(), "missing")
	t.Setenv("MESH_CONFIG_DIR", config)
	if _, _, err := identity.LoadOrCreate(state); err != nil {
		t.Fatal(err)
	}
	id := argumentIdentities(t)[1]
	host := HostRecord{ID: id, MeshIdentity: id, MachineName: "retained", NameRevision: 1}
	dial := serviceRemoteDial(host, func(protocol.Control) protocol.Control {
		t.Error("refresh read a non-host control")
		return protocol.Control{}
	})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	resolved, err := resolveAgentHost(ctx, host.MachineName, []HostRecord{host}, true, nil)
	if err != nil || resolved.ID != id {
		t.Fatalf("cached name: %v", err)
	}
	_, err = resolveAgentHost(ctx, "unknown", []HostRecord{host}, false, dial)
	if err == nil {
		t.Fatal("unknown name resolved")
	}
	assertAgentAbsent(t, config, filepath.Join(state, catalogDatabaseName), filepath.Join(state, "s"))
}
