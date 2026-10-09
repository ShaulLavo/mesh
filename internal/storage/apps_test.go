package storage

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

func TestAppStateAndRetiredNamesSurviveRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mesh.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	owner := storageAppIdentity(t)
	other := storageAppIdentity(t)
	state := []byte(`{"apps":[{"id":"7k3d","private":true}]}`)
	missing, err := store.LoadAppState(ctx, "edge")
	if err != nil || missing != nil {
		t.Fatalf("missing state = %q, %v", missing, err)
	}
	if err := store.SaveAppState(ctx, "edge", state); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAppState(ctx, "edge", []byte("partial JSON")); err == nil {
		t.Fatal("partial state accepted")
	}
	for _, name := range []string{"7k3d.mesh.test", "apps.mesh.test"} {
		if err := store.ReserveAppNames(ctx, []string{name}, owner); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.ReserveAppNames(ctx, []string{"7k3d.mesh.test"}, owner); err != nil {
		t.Fatalf("active owner retry = %v", err)
	}
	if err := store.ReserveAppNames(ctx, []string{"7k3d.mesh.test"}, other); !errors.Is(err, ErrAppNameCollision) {
		t.Fatalf("different owner = %v", err)
	}
	if err := store.SetAppNameInactive(ctx, "7k3d.mesh.test", other); !errors.Is(err, ErrAppNameCollision) {
		t.Fatalf("different owner retirement = %v", err)
	}
	if err := store.SetAppNameInactive(ctx, "7k3d.mesh.test", owner); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAppNameInactive(ctx, "7k3d.mesh.test", owner); err != nil {
		t.Fatalf("retirement retry = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadAppState(ctx, "edge")
	if err != nil || !bytes.Equal(loaded, state) {
		t.Fatalf("restored state = %q, %v", loaded, err)
	}
	exists, err := store.AppNameExists(ctx, "7k3d.mesh.test")
	if err != nil || !exists {
		t.Fatalf("retired name disappeared: %v, %v", exists, err)
	}
	if err := store.ReserveAppNames(ctx, []string{"7k3d.mesh.test"}, owner); !errors.Is(err, ErrAppNameCollision) {
		t.Fatalf("retired name resurrected = %v", err)
	}
	if err := store.ReserveAppNames(ctx, []string{"apps.mesh.test"}, owner); err != nil {
		t.Fatalf("management startup retry = %v", err)
	}
}

func TestAppStorageRejectsInvalidBoundaryValues(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	owner := storageAppIdentity(t)
	for _, name := range []string{"../apps", "APPS.mesh.test", "https://7k3d.mesh.test"} {
		if err := store.ReserveAppNames(ctx, []string{name}, owner); err == nil {
			t.Fatalf("invalid name %q accepted", name)
		}
	}
	if err := store.ReserveAppNames(ctx, []string{"7k3d.mesh.test"}, fmt.Sprintf("%043d", 1)); err == nil {
		t.Fatal("noncanonical identity accepted")
	}
	if err := store.SaveAppState(ctx, "", []byte(`{}`)); err == nil {
		t.Fatal("empty state key accepted")
	}
}

func TestAllocatedAppStateAndNameCommitTogether(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	owner := storageAppIdentity(t)
	other := storageAppIdentity(t)
	name := "7k3d.mesh.test"
	initial := []byte(`{"apps":{"7k3d":{"visibility":"private"}}}`)
	if err := store.ReserveAppNamesAndState(ctx, []string{name}, owner, "apps.edge", initial); err != nil {
		t.Fatal(err)
	}
	exists, err := store.AppNameExists(ctx, name)
	if err != nil || !exists {
		t.Fatalf("allocated name missing: %v, %v", exists, err)
	}
	loaded, err := store.LoadAppState(ctx, "apps.edge")
	if err != nil || !bytes.Equal(loaded, initial) {
		t.Fatalf("allocated state missing: %q, %v", loaded, err)
	}
	updated := []byte(`{"apps":{"7k3d":{"visibility":"private","generation":2}}}`)
	if err := store.ReserveAppNamesAndState(ctx, []string{name}, owner, "apps.edge", updated); err != nil {
		t.Fatalf("exact owner name retry: %v", err)
	}
	if err := store.ReserveAppNamesAndState(ctx, []string{name}, other, "apps.edge", initial); !errors.Is(err, ErrAppNameCollision) {
		t.Fatalf("wrong owner overwrote allocation: %v", err)
	}
	assertAppStateBytes(t, store, updated)
	if err := store.SetAppNameInactive(ctx, name, owner); err != nil {
		t.Fatal(err)
	}
	if err := store.ReserveAppNamesAndState(ctx, []string{name}, owner, "apps.edge", initial); !errors.Is(err, ErrAppNameCollision) {
		t.Fatalf("retired name resurrected: %v", err)
	}
	assertAppStateBytes(t, store, updated)
}

func TestAllocatedAppStateFailureRollsBackNewName(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	owner := storageAppIdentity(t)
	initial := []byte(`{"apps":{}}`)
	if err := store.SaveAppState(ctx, "apps.edge", initial); err != nil {
		t.Fatal(err)
	}
	_, err := store.db.ExecContext(ctx, `CREATE TRIGGER reject_app_state BEFORE UPDATE ON private_app_state
        WHEN NEW.key = 'apps.edge' BEGIN SELECT RAISE(FAIL, 'state persistence failed'); END`)
	if err != nil {
		t.Fatal(err)
	}
	name := "7k3d.mesh.test"
	if err := store.ReserveAppNamesAndState(ctx, []string{name}, owner, "apps.edge", []byte(`{"apps":{"7k3d":{}}}`)); err == nil {
		t.Fatal("allocation state write unexpectedly succeeded")
	}
	exists, err := store.AppNameExists(ctx, name)
	if err != nil || exists {
		t.Fatalf("failed state write leaked app name: %v, %v", exists, err)
	}
	assertAppStateBytes(t, store, initial)
	if _, err := store.db.ExecContext(ctx, "DROP TRIGGER reject_app_state"); err != nil {
		t.Fatal(err)
	}
	if err := store.ReserveAppNamesAndState(ctx, []string{name}, owner, "apps.edge", []byte(`{"apps":{"7k3d":{}}}`)); err != nil {
		t.Fatalf("failed allocation blocked retry: %v", err)
	}
}

func assertAppStateBytes(t *testing.T, store *Store, want []byte) {
	t.Helper()
	got, err := store.LoadAppState(context.Background(), "apps.edge")
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("app state = %q want %q, error=%v", got, want, err)
	}
}

func TestAppAliasConflictRollsBackNamesAndState(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	owner := storageAppIdentity(t)
	other := storageAppIdentity(t)
	if err := store.ReserveAppNames(ctx, []string{"7k3d.old.test"}, other); err != nil {
		t.Fatal(err)
	}
	if err := store.ReserveAppNamesAndState(ctx, []string{"7k3d.mesh.test", "7k3d.old.test"}, owner, "edge", []byte(`{"apps":[]}`)); !errors.Is(err, ErrAppNameCollision) {
		t.Fatalf("conflict: %v", err)
	}
	exists, err := store.AppNameExists(ctx, "7k3d.mesh.test")
	if err != nil || exists {
		t.Fatalf("partial primary reservation: %v %v", exists, err)
	}
	state, err := store.LoadAppState(ctx, "edge")
	if err != nil || state != nil {
		t.Fatalf("partial state publication: %s %v", state, err)
	}
}

func TestAppAliasAdoptionConflictRollsBackWholeSet(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	owner := storageAppIdentity(t)
	other := storageAppIdentity(t)
	if err := store.ReserveAppNames(ctx, []string{"7k3d.old.test"}, other); err != nil {
		t.Fatal(err)
	}
	if err := store.ReserveAppNames(ctx, []string{"7k3d.mesh.test", "7k3d.old.test"}, owner); !errors.Is(err, ErrAppNameCollision) {
		t.Fatalf("alias conflict: %v", err)
	}
	exists, err := store.AppNameExists(ctx, "7k3d.mesh.test")
	if err != nil || exists {
		t.Fatalf("partial adoption: %v %v", exists, err)
	}
}

func storageAppIdentity(t *testing.T) string {
	t.Helper()
	public, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(public)
}

func TestAppReservationRacesOtherOwners(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mesh.db")
	first, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	owner := storageAppIdentity(t)
	other := storageAppIdentity(t)
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() { <-start; results <- first.ReserveAppNames(ctx, []string{"7k3d.mesh.test"}, owner) }()
	go func() { <-start; results <- second.ReserveAppNames(ctx, []string{"7k3d.mesh.test"}, other) }()
	close(start)
	firstResult, secondResult := <-results, <-results
	if (firstResult == nil) == (secondResult == nil) {
		t.Fatalf("expected one hostname winner: %v, %v", firstResult, secondResult)
	}
	if loser := errors.Join(firstResult, secondResult); !errors.Is(loser, ErrAppNameCollision) {
		t.Fatalf("loser was not collision: %v", loser)
	}
}

func TestAppCapacityCleanupRetainsNameTombstones(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	owner := storageAppIdentity(t)
	_, err := store.db.ExecContext(ctx, `WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x < 8191)
        INSERT INTO app_names(hostname, owner_id, active) SELECT 'bulk' || x || '.mesh.test', ?, 1 FROM n`, owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReserveAppNames(ctx, []string{"7k3d.mesh.test"}, owner); err != nil {
		t.Fatalf("last slot: %v", err)
	}
	if err := store.ReserveAppNames(ctx, []string{"7k3d.mesh.test"}, owner); err != nil {
		t.Fatalf("retry at capacity: %v", err)
	}
	if err := store.ReserveAppNames(ctx, []string{"overflow.mesh.test"}, owner); !errors.Is(err, ErrAppNameCapacity) {
		t.Fatalf("overflow: %v", err)
	}
	if err := store.SetAppNameInactive(ctx, "7k3d.mesh.test", owner); err != nil {
		t.Fatal(err)
	}
	if err := store.ReserveAppNames(ctx, []string{"7k3d.mesh.test"}, owner); !errors.Is(err, ErrAppNameCollision) {
		t.Fatalf("retired name reused: %v", err)
	}
	if err := store.ReserveAppNames(ctx, []string{"overflow.mesh.test"}, owner); err != nil {
		t.Fatalf("retirement did not free capacity: %v", err)
	}
}

func TestAllocatedAppCollisionLeavesStateUnchanged(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	owner := storageAppIdentity(t)
	other := storageAppIdentity(t)
	initial := []byte(`{"apps":{}}`)
	if err := store.SaveAppState(ctx, "apps.edge", initial); err != nil {
		t.Fatal(err)
	}
	if err := store.ReserveAppNames(ctx, []string{"7k3d.mesh.test"}, other); err != nil {
		t.Fatal(err)
	}
	if err := store.ReserveAppNamesAndState(ctx, []string{"new.mesh.test", "7k3d.mesh.test"}, owner, "apps.edge", []byte(`{"apps":{"7k3d":{}}}`)); !errors.Is(err, ErrAppNameCollision) {
		t.Fatalf("existing owner collision: %v", err)
	}
	assertAppStateBytes(t, store, initial)
	exists, err := store.AppNameExists(ctx, "new.mesh.test")
	if err != nil || exists {
		t.Fatalf("collision leaked new name: %v, %v", exists, err)
	}
}
