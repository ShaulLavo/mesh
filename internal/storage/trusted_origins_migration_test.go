package storage

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

func TestTrustedOriginsMigrationDefaultsAndRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mesh.db")
	database := historicalDatabase(t, path, 13)
	executeMigrationFixture(t, database, `INSERT INTO hosts VALUES ('host-a','identity-a','pc.example.ts.net',1788000000)`)
	executeMigrationFixture(t, database, `INSERT INTO services(name,kind,target,isolate,listens,demand,local_only,display_name,private_host) VALUES ('api','proxy','3000',0,'','',0,'','api.mesh.test')`)
	executeMigrationFixture(t, database, `INSERT INTO cached_services(host_id,private_name,name,kind,target,healthy,problem,observed_at,isolate,display_name,private_host) VALUES ('host-a','pc.mesh.mesh.test','api','proxy','3000',1,'',1788000000,0,'','api.mesh.test')`)
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if got := migrationRows(t, store.db, "SELECT trusted_origins FROM services UNION ALL SELECT trusted_origins FROM cached_services"); !reflect.DeepEqual(got, [][]any{{""}, {""}}) {
		t.Fatalf("migration defaults = %#v", got)
	}
	trusted := []string{"https://bun-demo.sprockt.dev"}
	encoded := ` ["https://bun-demo.sprockt.dev"]`
	if _, err := store.db.Exec(`UPDATE services SET trusted_origins = ?`, encoded); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE cached_services SET trusted_origins = ?`, encoded); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	service, err := store.GetService(context.Background(), "api")
	if err != nil || !reflect.DeepEqual(service.TrustedOrigins, trusted) {
		t.Fatalf("service trusted origins = %#v, %v", service.TrustedOrigins, err)
	}
	cached, err := store.ListCachedServices(context.Background(), "host-a")
	if err != nil || len(cached) != 1 || !reflect.DeepEqual(cached[0].Service.TrustedOrigins, trusted) {
		t.Fatalf("cached trusted origins = %#v, %v", cached, err)
	}
	if _, err := store.db.Exec(`UPDATE services SET trusted_origins = '["https://bun-demo.sprockt.dev/"]'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetService(context.Background(), "api"); err == nil {
		t.Fatal("malformed stored trusted origins were accepted")
	}
	if _, err := store.db.Exec(`UPDATE services SET trusted_origins = 'null'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetService(context.Background(), "api"); err == nil || errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("null stored trusted origins error = %v", err)
	}
	_ = store.Close()
}
