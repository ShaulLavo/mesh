package storage

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

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

func TestServiceOnDemandFieldsRoundTrip(t *testing.T) {
	store := openTestStore(t)
	want := meshserve.Service{
		Name: "5173", Kind: meshserve.Proxy, Target: "5173", LocalOnly: true,
		Listens: []meshserve.Listen{{Public: 3001, Upstream: 13001}, {Public: 5173, Upstream: 15173}},
		Demand: &meshserve.Demand{
			Command: "bun run dev:web", Cwd: "/srv/app", Env: []string{"A=1", "B=two words"},
			Idle: 90 * time.Second, ReadyTimeout: 5 * time.Second,
		},
	}
	stored, err := store.UpsertService(context.Background(), want)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Equal(want) {
		t.Fatalf("upsert returned %+v, want %+v", stored, want)
	}
	listed, err := store.ListServices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || !listed[0].Equal(want) {
		t.Fatalf("listed %+v", listed)
	}
	plain := meshserve.Service{Name: "5173", Kind: meshserve.Proxy, Target: "5173"}
	if stored, err = store.UpsertService(context.Background(), plain); err != nil || !stored.Equal(plain) {
		t.Fatalf("replacing with a plain route kept on-demand fields: %+v, %v", stored, err)
	}
}
