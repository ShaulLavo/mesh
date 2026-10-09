package storage

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/shaul/mesh/db/migrations"
)

func historicalDatabase(t *testing.T, path string, version int64) *sql.DB {
	t.Helper()
	dsn, err := sqliteDSN(path, sqliteBusyTimeout)
	if err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open(sqliteDriver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	provider, err := goose.NewProvider(goose.DialectSQLite3, database, migrations.Files)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(context.Background(), version); err != nil {
		t.Fatal(err)
	}
	return database
}

func TestPrivateHostingMigrationPreservesPrivateState(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mesh.db")
	database := historicalDatabase(t, path, 12)
	owner := storageAppIdentity(t)
	other := storageAppIdentity(t)
	source := filepath.Join(t.TempDir(), "app-source.js")
	sourceBytes := []byte("export const answer = 42;\n")
	if err := os.WriteFile(source, sourceBytes, 0600); err != nil {
		t.Fatal(err)
	}
	opaque := []byte(`{
      "apps":{"7k3d":{"root":"` + filepath.Dir(source) + `","generation":5,"leaseUntil":"2027-01-01T00:00:00Z","sourceBytes":42}},
      "owners":{"owner":{"sequence":19}}, "grants":{"browser":{"owner":"owner","until":"2027-01-01T00:00:00Z"}},
      "sourceState":{"digest":"opaque-preserved-value"}, "worker":{"pid":1234,"socket":"worker.sock"}
    }`)
	seedPrivateHostingFixture(t, database, owner, other, source, opaque)
	queries := []string{
		"SELECT id,mesh_identity,tailscale_name,last_seen_at FROM hosts ORDER BY id",
		"SELECT id,host_id,command,cwd,state,created_at,last_attached_at,exit_code,last_output_sequence FROM sessions ORDER BY id",
		"SELECT name,kind,target,isolate,listens,demand,local_only,display_name,private_host FROM services ORDER BY name",
		"SELECT host_id,private_name,name,kind,target,healthy,problem,observed_at,isolate,display_name,private_host FROM cached_services ORDER BY name",
		"SELECT key,data FROM private_app_state ORDER BY key",
	}
	before := make([][][]any, len(queries))
	for index, query := range queries {
		before[index] = migrationRows(t, database, query)
	}
	names := migrationRows(t, database, "SELECT public_name,owner_id,active FROM app_names ORDER BY public_name")
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	assertCurrentMigrationVersion(t, store)
	assertPrivateHostingSchema(t, store.db)
	for index, query := range queries {
		assertMigrationRows(t, store.db, query, before[index])
	}
	assertMigrationRows(t, store.db, "SELECT hostname,owner_id,active FROM app_names ORDER BY hostname", names)
	assertAppStateBytes(t, store, opaque)
	if err := store.ReserveAppNames(ctx, []string{"retired.mesh.test"}, owner); !errors.Is(err, ErrAppNameCollision) {
		t.Fatalf("retired name reused after upgrade: %v", err)
	}
	if err := store.ReserveAppNames(ctx, []string{"7k3d.mesh.test"}, other); !errors.Is(err, ErrAppNameCollision) {
		t.Fatalf("owner changed after upgrade: %v", err)
	}
	preserved, err := os.ReadFile(source) //nolint:gosec // source is created in this test fixture directory
	if err != nil || !bytes.Equal(preserved, sourceBytes) {
		t.Fatalf("app source changed: %q, %v", preserved, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	assertAppStateBytes(t, reopened, opaque)
	assertMigrationRows(t, reopened.db, "SELECT hostname,owner_id,active FROM app_names ORDER BY hostname", names)
}

func seedPrivateHostingFixture(t *testing.T, database *sql.DB, owner, other, source string, opaque []byte) {
	t.Helper()
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO hosts VALUES ('host-a',?,'host-a.example.ts.net',1788000000)`, []any{owner}},
		{`INSERT INTO sessions VALUES ('LIVE','host-a','["bun","run","dev"]',?,'detached',1788000000,1788000010,NULL,901)`, []any{filepath.Dir(source)}},
		{`INSERT INTO services(name,kind,target,public_name,wake_on_request,isolate,listens,demand,local_only,display_name,private_host)
          VALUES ('app','proxy','3000','former.mesh.test',1,1,'[{"public":3000,"upstream":13000}]','{"command":"bun run dev","cwd":"/work/app","env":["A=1"],"idle":90000000000}',0,'Private app','fregat.mesh.test')`, nil},
		{`INSERT INTO cached_services(host_id,private_name,name,kind,target,public_name,wake_on_request,healthy,problem,observed_at,isolate,display_name,private_host)
          VALUES ('host-a','host-a.mesh.mesh.test','app','static',?,'former.mesh.test',1,0,'origin offline',1788000000,1,'Private app','fregat.mesh.test')`, []any{filepath.Dir(source)}},
		{`INSERT INTO private_app_state(key,data) VALUES ('apps.edge',?)`, []any{opaque}},
		{`INSERT INTO private_app_state(key,data) VALUES ('apps.origin',?)`, []any{[]byte(`{"sources":{"7k3d":{"root":"` + filepath.Dir(source) + `"}},"leases":{"7k3d":5},"worker":{"pid":1234}}`)}},
		{`INSERT INTO private_app_state(key,data) VALUES ('apps.webauth',?)`, []any{[]byte(`{"sessions":{"browser":{"owner":"pinned-owner"}},"grants":{"7k3d":true}}`)}},
		{`INSERT INTO app_names VALUES ('7k3d.mesh.test',?,1),('retired.mesh.test',?,0)`, []any{owner, owner}},
		{`INSERT INTO edge_snapshots VALUES (?,?,7,?,1788000000,1788000100,1788000000,?)`, []any{owner, other, strings.Repeat("a", 64), make([]byte, 64)}},
		{`INSERT INTO edge_routes VALUES (?,'former.mesh.test','app',1)`, []any{owner}},
		{`INSERT INTO edge_outbox VALUES (?,7,?,?,1)`, []any{other, strings.Repeat("a", 64), []byte(`{"sequence":7}`)}},
		{`INSERT INTO tunnel_highwater VALUES (?,7,?)`, []any{owner, strings.Repeat("a", 64)}},
		{`INSERT INTO tunnel_claims VALUES ('tunnel.mesh.test',?)`, []any{owner}},
		{`INSERT INTO tunnel_outbox VALUES (?,?,7,NULL,NULL,NULL,NULL,NULL)`, []any{other, owner}},
	}
	for _, statement := range statements {
		executeMigrationFixture(t, database, statement.query, statement.args...)
	}
}

func executeMigrationFixture(t *testing.T, database *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := database.Exec(query, args...); err != nil {
		t.Fatalf("fixture %s: %v", query, err)
	}
}

func migrationRows(t *testing.T, database *sql.DB, query string) [][]any {
	t.Helper()
	rows, err := database.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var values [][]any
	for rows.Next() {
		values = append(values, scanMigrationRow(t, rows, len(columns)))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return values
}

func scanMigrationRow(t *testing.T, rows *sql.Rows, count int) []any {
	t.Helper()
	values := make([]any, count)
	pointers := make([]any, count)
	for index := range values {
		pointers[index] = &values[index]
	}
	if err := rows.Scan(pointers...); err != nil {
		t.Fatal(err)
	}
	return values
}

func assertMigrationRows(t *testing.T, database *sql.DB, query string, want [][]any) {
	t.Helper()
	if got := migrationRows(t, database, query); !reflect.DeepEqual(got, want) {
		t.Fatalf("migration changed %s:\ngot %#v\nwant %#v", query, got, want)
	}
}

func assertPrivateHostingSchema(t *testing.T, database *sql.DB) {
	t.Helper()
	publicTables := migrationRows(t, database, "SELECT name FROM sqlite_master WHERE type='table' AND (name LIKE 'edge_%' OR name LIKE 'tunnel_%')")
	if len(publicTables) != 0 {
		t.Fatalf("public tables retained: %v", publicTables)
	}
	for _, table := range []string{"services", "cached_services", "app_names"} {
		assertNoPublicColumns(t, database, table)
	}
}

func assertNoPublicColumns(t *testing.T, database *sql.DB, table string) {
	t.Helper()
	columns := migrationRows(t, database, "SELECT name FROM pragma_table_info('"+table+"') WHERE name IN ('public_name','wake_on_request')")
	if len(columns) != 0 {
		t.Fatalf("public columns retained in %s: %v", table, columns)
	}
}
