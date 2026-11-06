package cli

import (
	"context"
	"errors"
	"testing"
	"time"

	"modernc.org/sqlite"

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
	if err := cache.Save(t.Context(), host, []protocol.SessionInfo{old}); err != nil {
		t.Fatal(err)
	}
	release := holdCatalogWriter(t, stateDir)
	outcome := make(chan catalogSaveOutcome, 1)
	observed := catalogCacheFuncs{load: cache.Load, save: func(ctx context.Context, host HostRecord, rows []protocol.SessionInfo) error {
		err := cache.Save(ctx, host, rows)
		outcome <- catalogSaveOutcome{err: err, contextErr: ctx.Err()}
		return err
	}}
	parent, cancel := context.WithCancel(t.Context())
	finished := make(chan struct{})
	t.Cleanup(func() { cancel(); release(); <-finished })
	done := make(chan catalogCollection, 1)
	started := time.Now()
	go func() {
		defer close(finished)
		rows, err := CollectHostSessions(parent, []HostRecord{host}, HostQueryBudget{Reply: 2 * time.Second},
			func(context.Context, *HostRecord, HostQueryBudget) ([]protocol.SessionInfo, error) { return nil, nil }, observed)
		done <- catalogCollection{rows: rows, err: err}
	}()
	var result catalogCollection
	select {
	case result = <-done:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("held writer exceeded the unchanged completion guard")
	}
	actual := <-outcome
	var busy *sqlite.Error
	if actual.contextErr != nil || !errors.As(actual.err, &busy) || busy.Code()&0xff != 5 {
		t.Fatalf("Save = %v, context = %v; want SQLite busy while Save context remains live", actual.err, actual.contextErr)
	}
	if result.err != nil || len(result.rows) != 1 {
		t.Fatalf("collection = %#v", result)
	}
	row := result.rows[0]
	if row.Stale || row.Err != nil || len(row.Sessions) != 0 || !errors.As(row.CacheErr, &busy) {
		t.Fatalf("live authority or busy diagnostic lost: %#v", row)
	}
	cached, err := cache.Load(t.Context(), host)
	if err != nil || len(cached) != 1 || cached[0].ID != "OLD1" || cached[0].State != "detached" {
		t.Fatalf("held writer changed old rows: %#v, %v", cached, err)
	}
	t.Logf("uncanceled SQLite busy after %v; old row unchanged while holder remains locked", time.Since(started))
	release()
	next := old
	next.ID = "NEW1"
	ctx, stop := context.WithTimeout(t.Context(), catalogCacheWriteTimeout)
	defer stop()
	if err := cache.Save(ctx, host, []protocol.SessionInfo{next}); err != nil {
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
