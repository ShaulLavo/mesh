package cli

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
)

func TestCatalogServiceWriterContentionSkipsAtomicSnapshot(t *testing.T) {
	state := t.TempDir()
	t.Setenv("MESH_STATE_DIR", state)
	cache, err := OpenCatalogCache(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	host := HostRecord{ID: "host-pc", MachineName: "pc", MeshIdentity: "original"}
	old := protocol.ServiceInfo{Name: "old", Kind: "proxy", Target: "3000"}
	if err := cache.SaveServices(t.Context(), host, "", []protocol.ServiceInfo{old}); err != nil {
		t.Fatal(err)
	}
	before, err := cache.LoadServices(t.Context(), host)
	if err != nil {
		t.Fatal(err)
	}
	readLog := captureCatalogCacheLog(t)
	release := holdCatalogWriter(t, state)
	next := protocol.ServiceInfo{Name: "new", Kind: "proxy", Target: "4000"}
	rows, diagnostics, err := CollectServiceCatalog(t.Context(), []HostRecord{host}, 15*time.Second,
		func(context.Context, HostRecord) (remoteServiceSnapshot, error) {
			return remoteServiceSnapshot{Host: host, PrivateName: "", Services: []protocol.ServiceInfo{next}}, nil
		}, cache)
	if err != nil || len(diagnostics) != 0 || len(rows) != 1 || !rows[0].Live || rows[0].Service.Name != "new" {
		t.Fatalf("busy cache affected live service catalog: %#v, %v, %v", rows, diagnostics, err)
	}
	if output := readLog(); !strings.Contains(output, "catalog cache: skipped busy services write") {
		t.Fatalf("missing skip log: %q", output)
	}
	after, err := cache.LoadServices(t.Context(), host)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("skipped service snapshot changed: %#v, %v", after, err)
	}
	release()
	if err := cache.SaveServices(t.Context(), host, "", []protocol.ServiceInfo{next}); err != nil {
		t.Fatal(err)
	}
	after, err = cache.LoadServices(t.Context(), host)
	if err != nil || len(after) != 1 || after[0].Service.Name != "new" {
		t.Fatalf("service snapshot did not converge: %#v, %v", after, err)
	}
	assertCatalogIntegrity(t, state)
}

func TestCatalogWriteErrorsRemainVisible(t *testing.T) {
	state := t.TempDir()
	t.Setenv("MESH_STATE_DIR", state)
	cache, err := OpenCatalogCache(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	host := HostRecord{ID: "host-pc", MachineName: "pc", MeshIdentity: "original"}
	old := protocol.SessionInfo{ID: "OLD1", HostID: host.ID, Command: []string{"sh"}, State: "detached", CreatedAt: time.Now().UTC()}
	if err := cache.SaveObserved(t.Context(), host, []protocol.SessionInfo{old}); err != nil {
		t.Fatal(err)
	}
	before, err := cache.LoadObserved(t.Context(), host)
	if err != nil {
		t.Fatal(err)
	}
	readLog := captureCatalogCacheLog(t)
	release := holdCatalogWriter(t, state)
	if err := cache.Save(t.Context(), host, []protocol.SessionInfo{{HostID: "another-host"}}); err == nil {
		t.Fatal("invalid host was skipped")
	}
	if err := cache.SaveObserved(t.Context(), host, []protocol.SessionInfo{old, old}); err == nil {
		t.Fatal("duplicate session was skipped")
	}
	if err := cache.SaveServices(t.Context(), host, "", []protocol.ServiceInfo{{Name: "invalid name"}}); err == nil {
		t.Fatal("invalid service was skipped")
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := cache.SaveObserved(canceled, host, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled write = %v", err)
	}
	release()
	db, err := sql.Open("sqlite", filepath.Join(state, catalogDatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(t.Context(), `CREATE TRIGGER reject_session BEFORE INSERT ON sessions BEGIN SELECT RAISE(ABORT, 'database is locked'); END`); err != nil {
		t.Fatal(err)
	}
	next := old
	next.ID = "NEW1"
	host.MeshIdentity = "replacement"
	if err := cache.SaveObserved(t.Context(), host, []protocol.SessionInfo{next}); err == nil || !strings.Contains(err.Error(), "database is locked") {
		t.Fatalf("non-BUSY transaction failure = %v", err)
	}
	after, err := cache.LoadObserved(t.Context(), host)
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("failed transaction changed sessions or facts: %#v, %v", after, err)
	}
	persisted, err := cache.store.GetHost(t.Context(), "host-pc")
	if err != nil || persisted.MeshIdentity != "original" {
		t.Fatalf("failed transaction changed host: %#v, %v", persisted, err)
	}
	if output := readLog(); strings.Contains(output, "skipped busy") {
		t.Fatalf("non-BUSY error logged as skip: %q", output)
	}
	corruptState := t.TempDir()
	t.Setenv("MESH_STATE_DIR", corruptState)
	if err := os.WriteFile(filepath.Join(corruptState, catalogDatabaseName), []byte("not a SQLite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if opened, err := OpenCatalogCache(t.Context()); err == nil {
		_ = opened.Close()
		t.Fatal("corrupt cache open returned success")
	}
	assertCatalogIntegrity(t, state)
}

func TestAgentSessionBusyCacheKeepsLiveObservation(t *testing.T) {
	host := sessionCacheHost(t)
	state := os.Getenv("MESH_STATE_DIR")
	cache, err := OpenCatalogCache(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	old := sessionCacheRows(host, "old")
	if err := cache.SaveObserved(t.Context(), host, old); err != nil {
		t.Fatal(err)
	}
	before, err := cache.LoadObserved(t.Context(), host)
	if err != nil {
		t.Fatal(err)
	}
	readLog := captureCatalogCacheLog(t)
	release := holdCatalogWriter(t, state)
	next := sessionCacheRows(host, "live")
	live, diagnostics := sessionCacheList(t, host, cacheLiveDial(host, next), 0, "--timeout=15s")
	if len(diagnostics) != 0 || live.Freshness != "live" || len(live.Sessions) != len(next) {
		t.Fatalf("busy cache affected agent live result: %#v, %v", live, diagnostics)
	}
	if output := readLog(); !strings.Contains(output, "catalog cache: skipped busy sessions write") {
		t.Fatalf("missing agent skip log: %q", output)
	}
	after, err := cache.LoadObserved(t.Context(), host)
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("busy agent save changed offline rows: %#v, %v", after, err)
	}
	release()
	sessionCacheList(t, host, cacheLiveDial(host, next), 0, "--timeout=15s")
	offline, _ := sessionCacheList(t, host, cacheOffline, 3)
	if !reflect.DeepEqual(offline.Sessions, live.Sessions) {
		t.Fatalf("agent snapshot did not converge: %#v, want %#v", offline.Sessions, live.Sessions)
	}
}
