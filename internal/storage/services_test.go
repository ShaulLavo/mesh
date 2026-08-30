package storage

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	meshserve "github.com/shaul/mesh/internal/serve"
)

func TestStoreServiceLifecycleSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "mesh.db")
	root := t.TempDir()
	want := meshserve.Service{
		Name:          "blog/assets",
		Kind:          meshserve.Files,
		Target:        root,
		PublicName:    "blog.shaulavo.dev",
		WakeOnRequest: true,
	}

	store, err := Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.UpsertService(ctx, want)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("upserted service = %#v, want %#v", got, want)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	got, err = store.GetService(ctx, want.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reopened service = %#v, want %#v", got, want)
	}
	services, err := store.ListServices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(services, []meshserve.Service{want}) {
		t.Fatalf("listed services = %#v", services)
	}

	updated := want
	updated.Kind = meshserve.Static
	updated.PublicName = "site.shaulavo.dev"
	updated.WakeOnRequest = false
	if _, err := store.UpsertService(ctx, updated); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteService(ctx, updated.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetService(ctx, updated.Name); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted service error = %v, want sql.ErrNoRows", err)
	}
	services, err = store.ListServices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if services == nil || len(services) != 0 {
		t.Fatalf("services after delete = %#v, want non-nil empty slice", services)
	}
}

func TestStoreRejectsInvalidServicesBeforeWriting(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	root := t.TempDir()
	invalid := []meshserve.Service{
		{Name: "", Kind: meshserve.Static, Target: root},
		{Name: "../escape", Kind: meshserve.Static, Target: root},
		{Name: "bad", Kind: meshserve.Kind("other"), Target: root},
		{Name: "bad", Kind: meshserve.Proxy, Target: "0"},
		{Name: "bad", Kind: meshserve.Proxy, Target: "65536"},
		{Name: "bad", Kind: meshserve.Proxy, Target: "3000", PublicName: "example.com"},
	}
	for _, service := range invalid {
		if _, err := store.UpsertService(ctx, service); err == nil {
			t.Fatalf("invalid service %#v succeeded", service)
		}
	}
	services, err := store.ListServices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(services) != 0 {
		t.Fatalf("invalid writes persisted services: %#v", services)
	}
}
