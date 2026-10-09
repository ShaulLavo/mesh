package apps

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/webauth"
	_ "modernc.org/sqlite"
)

func TestStoredReservedAppRestartAndCleanup(t *testing.T) {
	for _, action := range []string{"delete", "expiry"} {
		t.Run(action, func(t *testing.T) {
			f := newAppFixture(t)
			record := Record{ID: "mesh", Owner: identityFor(f.ownerKey), Kind: "server", Status: "active", Cleanup: "pending", Generation: 1, ExpiresAt: f.now.Add(time.Hour), LeaseUntil: f.now.Add(LeaseTTL)}
			workspace := filepath.Join(f.root, "apps", "mesh", "source")
			if err := os.MkdirAll(workspace, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(workspace, "index.html"), []byte("legacy"), 0600); err != nil {
				t.Fatal(err)
			}
			f.workers.labels = map[string]string{"app mesh": "legacy-worker"}
			f.edge.state.Apps[record.ID] = record
			edgeRaw, err := json.Marshal(f.edge.state)
			if err != nil {
				t.Fatal(err)
			}
			if err = f.edgeStore.SaveAppState(context.Background(), "apps.edge", edgeRaw); err != nil {
				t.Fatal(err)
			}
			if err = f.edgeStore.ReserveAppNames(context.Background(), []string{"mesh." + Domain()}, record.Owner); err != nil {
				t.Fatal(err)
			}
			f.origin.state.Apps[record.ID] = localApp{Record: record, Root: workspace, Phase: "running", Session: "legacy-worker"}
			if err = f.origin.persist(context.Background()); err != nil {
				t.Fatal(err)
			}
			f.edge.Close()
			f.origin.Close()
			f.edge = f.openEdge(t)
			config := f.origin.config
			config.Exchange = f.edge.Exchange
			f.origin, err = NewOrigin(context.Background(), config)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(f.origin.Close)
			inspected, err := f.origin.Handle(context.Background(), Request{Action: "inspect", ID: "mesh"})
			if err != nil || inspected.App == nil {
				t.Fatalf("legacy inspect failed: %v", err)
			}
			if action == "delete" {
				_, err = f.origin.Handle(context.Background(), Request{Action: "delete", ID: "mesh"})
			} else {
				f.now = record.ExpiresAt
				err = f.origin.Sync(context.Background())
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = os.Stat(filepath.Join(f.root, "apps", "mesh")); !os.IsNotExist(err) {
				t.Fatalf("legacy workspace remains: %v", err)
			}
			if _, alive, err := f.workers.Find(context.Background(), "app mesh"); err != nil || alive {
				t.Fatal("legacy worker remains")
			}
			if _, exists := f.origin.state.Apps["mesh"]; exists {
				t.Fatal("legacy origin record remains")
			}
			final, _, err := f.edge.lookup(context.Background(), "mesh", false)
			if err != nil || final.Cleanup != "complete" {
				t.Fatalf("cleanup incomplete: %#v %v", final, err)
			}
			if ValidID("mesh") {
				t.Fatal("new allocation still allows reserved ID")
			}
			for range 16 {
				next := &registryMutation{state: registryState{Apps: map[string]Record{}, Owners: map[string]ownerState{}}}
				allocated, err := f.edge.allocate(context.Background(), next, record.Owner, "static")
				if err != nil || allocated.App == nil || !ValidID(allocated.App.ID) {
					t.Fatalf("new allocation accepted a reserved ID: %#v %v", allocated, err)
				}
			}
		})
	}
}

type countedSQLiteState struct {
	db     *sql.DB
	writes atomic.Int64
}

func (s *countedSQLiteState) SaveAppState(ctx context.Context, key string, raw []byte) error {
	s.writes.Add(1)
	_, err := s.db.ExecContext(ctx, `INSERT INTO state(key,data) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET data=excluded.data`, key, raw)
	if err != nil {
		return fmt.Errorf("save test auth: %w", err)
	}
	return nil
}

func (s *countedSQLiteState) LoadAppState(ctx context.Context, key string) ([]byte, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, "SELECT data FROM state WHERE key=?", key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load test auth: %w", err)
	}
	return raw, nil
}

func TestCanonicalBogusTicketHasUniformReadOnlyWork(t *testing.T) {
	f := newAppFixture(t)
	store, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if _, err = store.Exec("CREATE TABLE state(key TEXT PRIMARY KEY,data BLOB NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	counted := &countedSQLiteState{db: store}
	f.edge.auth, err = webauth.New(counted, func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	var baseline string
	for _, state := range []string{"private", "expired", "unknown"} {
		f.edge.mu.Lock()
		f.edge.state.Apps = map[string]Record{}
		if state != "unknown" {
			record := Record{ID: "7k3d", Status: "active", ExpiresAt: f.now.Add(time.Hour)}
			if state == "expired" {
				record.Status = "expired"
			}
			f.edge.state.Apps[record.ID] = record
		}
		f.edge.publishRuntime(f.edge.state, nil)
		f.edge.mu.Unlock()
		before := counted.writes.Load()
		started := time.Now()
		for range 64 {
			request := httptest.NewRequest(http.MethodGet, URL("7k3d")+"/?mesh_view="+strings.Repeat("A", 43), nil)
			request.AddCookie(&http.Cookie{Name: webauth.ViewNonceCookie, Value: strings.Repeat("A", 43), Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
			response := httptest.NewRecorder()
			if !f.edge.ServeHost(response, request, "7k3d."+Domain()) || response.Code != http.StatusForbidden {
				t.Fatal("unexpected refusal")
			}
			if baseline == "" {
				baseline = response.Body.String()
			} else if baseline != response.Body.String() {
				t.Fatal("refusal differs")
			}
		}
		elapsed := time.Since(started)
		t.Logf("%s: 64 requests in %s; durable writes=%d", state, elapsed, counted.writes.Load()-before)
		if writes := counted.writes.Load() - before; writes != 0 {
			t.Errorf("%s bogus tickets performed %d durable writes", state, writes)
		}
	}
}
