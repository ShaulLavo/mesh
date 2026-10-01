package cli

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"modernc.org/sqlite"

	"github.com/shaul/mesh/internal/protocol"
)

func holdCatalogWriter(t *testing.T, stateDir string) func() {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(stateDir, catalogDatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	conn, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := conn.ExecContext(t.Context(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			if _, err := conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(release)
	return release
}

func TestCatalogSQLiteWriterContentionRespectsBudget(t *testing.T) {
	for _, parentCancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "deadline", true: "parent cancellation"}[parentCancel], func(t *testing.T) {
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
			host := HostRecord{Alias: "pc", ID: "host-pc", MeshIdentity: "identity-pc"}
			if err := cache.Save(t.Context(), host, []protocol.SessionInfo{{
				ID: "OLD1", HostID: host.ID, Command: []string{"sh"}, State: "detached", CreatedAt: time.Now().UTC(),
			}}); err != nil {
				t.Fatal(err)
			}
			release := holdCatalogWriter(t, stateDir)
			parent, cancel := context.WithCancel(context.Background())
			finished := make(chan struct{})
			done := make(chan catalogCollection, 1)
			t.Cleanup(func() { cancel(); release(); <-finished })
			hosts := []HostRecord{host}
			timeout := 40 * time.Millisecond
			if parentCancel {
				hosts = append(hosts, HostRecord{Alias: "pending", ID: "host-pending"})
				timeout = 2 * time.Second
				timer := time.AfterFunc(40*time.Millisecond, cancel)
				t.Cleanup(func() { timer.Stop() })
			}
			started := time.Now()
			go func() {
				defer close(finished)
				rows, err := CollectHostSessions(parent, hosts, timeout,
					func(ctx context.Context, queried HostRecord) ([]protocol.SessionInfo, error) {
						if queried.ID == host.ID {
							return nil, nil
						}
						<-ctx.Done()
						return nil, ctx.Err()
					}, cache)
				done <- catalogCollection{rows: rows, err: err}
			}()
			var result catalogCollection
			select {
			case result = <-done:
			case <-time.After(300 * time.Millisecond):
				t.Fatalf("SQLite writer lock held collection for %v beyond its 40 ms deadline or cancellation", time.Since(started))
			}
			if parentCancel && !errors.Is(result.err, context.Canceled) {
				t.Fatalf("collection error = %v, want parent cancellation", result.err)
			}
			if !parentCancel {
				if result.err != nil || len(result.rows) != 1 {
					t.Fatalf("collection = %#v", result)
				}
				row := result.rows[0]
				var busy *sqlite.Error
				if row.Stale || row.Err != nil || len(row.Sessions) != 0 || !errors.As(row.CacheErr, &busy) || busy.Code()&0xff != 5 {
					t.Fatalf("contended save replaced live authority or hid SQLITE_BUSY: %#v", row)
				}
			}
			cached, err := cache.Load(t.Context(), host)
			if err != nil || len(cached) != 1 || cached[0].ID != "OLD1" {
				t.Fatalf("skipped save changed cached rows while writer remained locked: %#v, %v", cached, err)
			}
		})
	}
}
