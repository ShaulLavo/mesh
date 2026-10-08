package storage

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	meshserve "github.com/shaul/mesh/internal/serve"
)

func TestPrivateHostSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "mesh.db")
	want := meshserve.Service{Name: "platform", Kind: meshserve.Proxy, Target: "3301", PrivateHost: "fregat.mesh.test", Isolate: true}
	store, err := Open(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertService(ctx, want); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	got, err := store.GetService(ctx, want.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reopened service = %#v, want %#v", got, want)
	}
}
