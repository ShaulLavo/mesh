package storage

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
	"github.com/shaul/mesh/db/migrations"
	meshserve "github.com/shaul/mesh/internal/serve"
)

func TestCachedServiceScopeMigrationDiscardsOnlyAdvisoryRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mesh.db")
	database := historicalDatabase(t, path, 13)
	statements := []string{
		`INSERT INTO hosts(id,mesh_identity,last_seen_at) VALUES ('scope-host','scope-identity',1)`,
		`INSERT INTO sessions(id,host_id,command,cwd,state,created_at,last_output_sequence) VALUES ('LIVE','scope-host','["sh"]','/work','detached',1,17)`,
		`INSERT INTO services(name,kind,target,listens,local_only) VALUES ('3000','proxy','3000','[{"public":3000,"upstream":3001}]',1)`,
		`INSERT INTO cached_services(host_id,private_name,name,kind,target,healthy,problem,observed_at) VALUES ('scope-host','','3000','proxy','3000',1,'',1)`,
	}
	for _, statement := range statements {
		if _, err := database.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	preserved := []string{"SELECT * FROM hosts", "SELECT * FROM sessions", "SELECT * FROM services"}
	before := make([][][]any, len(preserved))
	for i, query := range preserved {
		before[i] = migrationRows(t, database, query)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	assertCurrentMigrationVersion(t, store)
	for i, query := range preserved {
		assertMigrationRows(t, store.db, query, before[i])
	}
	rows, err := store.ListAllCachedServices(t.Context())
	if err != nil || len(rows) != 0 {
		t.Fatalf("lossy advisory cache survived migration: %v %v", rows, err)
	}
	row := CachedService{HostID: "scope-host", Service: meshserve.Service{Name: "3000", Kind: meshserve.Proxy, Target: "3000", LocalOnly: true, Listens: []meshserve.Listen{{Public: 3000, Upstream: 3001}}}, ObservedAt: time.UnixMilli(1).UTC()}
	if err := store.ReplaceCachedServices(t.Context(), Host{ID: row.HostID, MeshIdentity: "scope-identity", LastSeenAt: row.ObservedAt}, []CachedService{row}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	rows, err = reopened.ListCachedServices(t.Context(), row.HostID)
	if err != nil || len(rows) != 1 || !rows[0].Service.LocalOnly || !slices.Equal(rows[0].Service.Listens, row.Service.Listens) {
		t.Fatalf("fresh authoritative scope was not retained across reopen: %v %v", rows, err)
	}
	for i, query := range preserved {
		assertMigrationRows(t, reopened.db, query, before[i])
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, reopened.db, migrations.Files)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(context.Background(), 13); err != nil {
		t.Fatal(err)
	}
	assertMigrationRows(t, reopened.db, "SELECT * FROM cached_services", nil)
	for i, query := range preserved {
		assertMigrationRows(t, reopened.db, query, before[i])
	}
}
