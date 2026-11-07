package cli

import (
	"context"
	"log"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
)

type catalogSaveOutcome struct {
	err        error
	contextErr error
}

func TestCatalogSQLiteWriterContentionWithoutCancellation(t *testing.T) {
	stateDir := compactSocketTempDir(t)
	t.Setenv("MESH_STATE_DIR", stateDir)
	cache, err := OpenCatalogCache(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cache.Close(); err != nil {
			t.Error(err)
		}
	})
	host := HostRecord{MachineName: "pc", ID: "host-pc", MeshIdentity: "identity-pc"}
	old := protocol.SessionInfo{ID: "OLD1", HostID: host.ID, Command: []string{"sh"}, State: "detached", CreatedAt: time.Now().UTC()}
	if _, err := cache.SaveObserved(t.Context(), host, []protocol.SessionInfo{old}); err != nil {
		t.Fatal(err)
	}
	before, err := cache.LoadObserved(t.Context(), host)
	if err != nil {
		t.Fatal(err)
	}
	beforeHost, err := cache.store.GetHost(t.Context(), "host-pc")
	if err != nil {
		t.Fatal(err)
	}
	readLog := captureCatalogCacheLog(t)
	release := holdCatalogWriter(t, stateDir)
	outcome := make(chan catalogSaveOutcome, 1)
	observed := catalogCacheFuncs{load: cache.Load, save: func(ctx context.Context, host HostRecord, rows []protocol.SessionInfo) (bool, error) {
		_, err := cache.Save(ctx, host, rows)
		outcome <- catalogSaveOutcome{err: err, contextErr: ctx.Err()}
		return false, err
	}}
	parent, cancel := context.WithCancel(t.Context())
	finished := make(chan struct{})
	t.Cleanup(func() { cancel(); release(); <-finished })
	done := make(chan catalogCollection, 1)
	started := time.Now()
	go func() {
		defer close(finished)
		rows, err := CollectHostSessions(parent, []HostRecord{host}, HostQueryBudget{Reply: 15 * time.Second},
			func(context.Context, *HostRecord, HostQueryBudget) ([]protocol.SessionInfo, error) { return nil, nil }, observed)
		done <- catalogCollection{rows: rows, err: err}
	}()
	var result catalogCollection
	select {
	case result = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("held writer exceeded the advisory completion guard")
	}
	actual := <-outcome
	if actual.contextErr != nil || actual.err != nil {
		t.Fatalf("Save = %v, context = %v; want skipped busy write while context remains live", actual.err, actual.contextErr)
	}
	if output := readLog(); !strings.Contains(output, "catalog cache: skipped busy sessions write") || !strings.Contains(output, host.ID) {
		t.Fatalf("missing skipped-write log: %q", output)
	}
	if result.err != nil || len(result.rows) != 1 {
		t.Fatalf("collection = %#v", result)
	}
	row := result.rows[0]
	if row.Stale || row.Err != nil || len(row.Sessions) != 0 || row.CacheErr != nil {
		t.Fatalf("live authority lost or busy leaked to caller: %#v", row)
	}
	cached, err := cache.Load(t.Context(), host)
	if err != nil || len(cached) != 1 || cached[0].ID != "OLD1" || cached[0].State != "detached" {
		t.Fatalf("held writer changed old rows: %#v, %v", cached, err)
	}
	after, err := cache.LoadObserved(t.Context(), host)
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("skipped write changed sessions or facts: %#v, %v", after, err)
	}
	afterHost, err := cache.store.GetHost(t.Context(), "host-pc")
	if err != nil || !reflect.DeepEqual(afterHost, beforeHost) {
		t.Fatalf("skipped write changed host: %#v, %v", afterHost, err)
	}
	t.Logf("uncanceled writer exhaustion skipped after %v; host, rows and facts unchanged while holder remains locked", time.Since(started))
	release()
	next := old
	next.ID = "NEW1"
	ctx, stop := context.WithTimeout(t.Context(), catalogCacheWriteTimeout)
	defer stop()
	if _, err := cache.Save(ctx, host, []protocol.SessionInfo{next}); err != nil {
		t.Fatalf("Save after rollback: %v", err)
	}
	cached, err = cache.Load(t.Context(), host)
	if err != nil || len(cached) != 2 {
		t.Fatalf("history after recovery: %#v, %v", cached, err)
	}
	states := make(map[string]string, len(cached))
	for _, session := range cached {
		states[session.ID] = session.State
	}
	if states["OLD1"] != "interrupted" || states["NEW1"] != "detached" {
		t.Fatalf("recovered history = %#v", states)
	}
}

func captureCatalogCacheLog(t *testing.T) func() string {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "cache-log-*")
	if err != nil {
		t.Fatal(err)
	}
	previous := log.Writer()
	t.Cleanup(func() { log.SetOutput(previous); _ = file.Close() })
	log.SetOutput(file)
	return func() string {
		data, err := os.ReadFile(file.Name())
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
}
