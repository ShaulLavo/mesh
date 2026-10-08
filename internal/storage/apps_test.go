package storage

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
	"github.com/shaul/mesh/db/migrations"
	"github.com/shaul/mesh/internal/edge"
	"github.com/shaul/mesh/internal/tunnel"
)

func TestAppMigrationUpgradesExistingTunnelStore(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mesh.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	owner, key := storageEdgeIdentity(t)
	target, _ := storageEdgeIdentity(t)
	claim := signedTunnelMutation(t, key, target, tunnel.Create, "existing.mesh.test", 1)
	if err := applyTestTunnel(store, claim); err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, store.db, migrations.Files)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 8); err != nil {
		t.Fatal(err)
	}
	assertMigrationVersion(t, store, 8)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	assertMigrationVersion(t, store, 10)
	restored, err := store.TunnelClaim(ctx, claim.PublicName)
	if err != nil || restored.ClaimantID != owner {
		t.Fatalf("migration lost existing tunnel: %+v, %v", restored, err)
	}
	if err := store.ReserveAppNames(ctx, []string{"7k3d.mesh.test"}, owner); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAppState(ctx, "edge", []byte(`{"private":true}`)); err != nil {
		t.Fatal(err)
	}
}

func TestAppStateAndRetiredNamesSurviveRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mesh.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	owner, _ := storageEdgeIdentity(t)
	other, _ := storageEdgeIdentity(t)
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
	if err := store.ReserveAppNames(ctx, []string{"7k3d.mesh.test"}, other); !errors.Is(err, tunnel.ErrCollision) {
		t.Fatalf("different owner = %v", err)
	}
	if err := store.SetAppNameInactive(ctx, "7k3d.mesh.test", other); !errors.Is(err, tunnel.ErrCollision) {
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
	if err := store.ReserveAppNames(ctx, []string{"7k3d.mesh.test"}, owner); !errors.Is(err, tunnel.ErrCollision) {
		t.Fatalf("retired name resurrected = %v", err)
	}
	if err := store.ReserveAppNames(ctx, []string{"apps.mesh.test"}, owner); err != nil {
		t.Fatalf("management startup retry = %v", err)
	}
}

func TestAppNamesBlockTunnelAndSnapshotEvenAfterRetirement(t *testing.T) {
	ctx := context.Background()
	store := openTunnelTestStore(t)
	target, _ := storageEdgeIdentity(t)
	owner, key := storageEdgeIdentity(t)
	now := time.Now().UTC()
	for index, name := range []string{"7k3d.mesh.test", "apps.mesh.test"} {
		if err := store.ReserveAppNames(ctx, []string{name}, owner); err != nil {
			t.Fatal(err)
		}
		assertAppPublicationBlocked(t, store, target, owner, key, name, uint64(index+1), now)
		if err := store.SetAppNameInactive(ctx, name, owner); err != nil {
			t.Fatal(err)
		}
		assertAppPublicationBlocked(t, store, target, owner, key, name, uint64(index+3), now)
	}
}

func TestAppsRespectExistingServiceAndTunnelNames(t *testing.T) {
	ctx := context.Background()
	store := openTunnelTestStore(t)
	target, _ := storageEdgeIdentity(t)
	owner, key := storageEdgeIdentity(t)
	now := time.Now().UTC()
	snapshot := storageSignedSnapshot(t, target, owner, key, 1, now, []edge.Route{{PublicName: "7k3d.mesh.test", ServiceName: "nested/path"}})
	if err := store.ApplyEdgeSnapshot(ctx, snapshot, storageSnapshotDigest(t, snapshot, target, owner), now); err != nil {
		t.Fatal(err)
	}
	mutation := signedTunnelMutation(t, key, target, tunnel.Create, "apps.mesh.test", 1)
	if err := applyTestTunnel(store, mutation); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"7k3d.mesh.test", "apps.mesh.test"} {
		if err := store.ReserveAppNames(ctx, []string{name}, owner); !errors.Is(err, tunnel.ErrCollision) {
			t.Fatalf("existing name %s = %v", name, err)
		}
	}
}

func TestAppNameBlocksOwnerSnapshotRefreshWithoutChangingExistingRoutes(t *testing.T) {
	ctx := context.Background()
	store := openTunnelTestStore(t)
	owner, key := storageEdgeIdentity(t)
	target, _ := storageEdgeIdentity(t)
	now := time.Now().UTC()
	initial := storageSignedSnapshot(t, target, owner, key, 1, now, []edge.Route{{PublicName: "existing.mesh.test", ServiceName: "app"}})
	if err := store.ApplyEdgeSnapshot(ctx, initial, storageSnapshotDigest(t, initial, target, owner), now); err != nil {
		t.Fatal(err)
	}
	if err := store.ReserveAppNames(ctx, []string{"7k3d.mesh.test"}, owner); err != nil {
		t.Fatal(err)
	}
	refresh := storageSignedSnapshot(t, target, owner, key, 2, now.Add(time.Second), []edge.Route{
		{PublicName: "existing.mesh.test", ServiceName: "new"},
		{PublicName: "7k3d.mesh.test", ServiceName: "nested/path"},
	})
	if err := store.ApplyEdgeSnapshot(ctx, refresh, storageSnapshotDigest(t, refresh, target, owner), now.Add(time.Second)); !errors.Is(err, edge.ErrRouteCollision) {
		t.Fatalf("owner snapshot claimed app name: %v", err)
	}
	state, err := store.LoadEdgeState(ctx)
	if err != nil || len(state) != 1 {
		t.Fatalf("restored state = %+v, %v", state, err)
	}
	if state[0].Snapshot.Sequence != 1 || len(state[0].Snapshot.Routes) != 1 || state[0].Snapshot.Routes[0].ServiceName != "app" {
		t.Fatalf("rejected refresh modified prior state: %+v", state[0])
	}
}

func assertAppPublicationBlocked(t *testing.T, store *Store, target, owner string, key ed25519.PrivateKey, name string, sequence uint64, now time.Time) {
	t.Helper()
	mutation := signedTunnelMutation(t, key, target, tunnel.Create, name, sequence)
	if err := applyTestTunnel(store, mutation); !errors.Is(err, tunnel.ErrCollision) {
		t.Fatalf("tunnel at reserved %s = %v", name, err)
	}
	snapshot := storageSignedSnapshot(t, target, owner, key, sequence, now, []edge.Route{{PublicName: name, ServiceName: "nested/path"}})
	if err := store.ApplyEdgeSnapshot(context.Background(), snapshot, storageSnapshotDigest(t, snapshot, target, owner), now); !errors.Is(err, edge.ErrRouteCollision) {
		t.Fatalf("snapshot at reserved %s = %v", name, err)
	}
}

func TestAppReservationRacesOtherHostnameClaims(t *testing.T) {
	for _, contender := range []string{"app", "tunnel", "snapshot"} {
		t.Run(contender, func(t *testing.T) { raceAppReservation(t, contender) })
	}
}

func raceAppReservation(t *testing.T, contender string) {
	t.Helper()
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
	owner, _ := storageEdgeIdentity(t)
	other, key := storageEdgeIdentity(t)
	target, _ := storageEdgeIdentity(t)
	name := "7k3d.mesh.test"
	now := time.Now().UTC()
	mutation := signedTunnelMutation(t, key, target, tunnel.Create, name, 1)
	snapshot := storageSignedSnapshot(t, target, other, key, 1, now, []edge.Route{{PublicName: name, ServiceName: "nested/path"}})
	digest := storageSnapshotDigest(t, snapshot, target, other)
	action := func() error { return second.ReserveAppNames(ctx, []string{name}, other) }
	if contender == "tunnel" {
		action = func() error { return applyTestTunnel(second, mutation) }
	}
	if contender == "snapshot" {
		action = func() error { return second.ApplyEdgeSnapshot(ctx, snapshot, digest, now) }
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() { <-start; results <- first.ReserveAppNames(ctx, []string{name}, owner) }()
	go func() { <-start; results <- action() }()
	close(start)
	firstResult, secondResult := <-results, <-results
	if (firstResult == nil) == (secondResult == nil) {
		t.Fatalf("expected one hostname winner: %v, %v", firstResult, secondResult)
	}
	loser := errors.Join(firstResult, secondResult)
	if !errors.Is(loser, tunnel.ErrCollision) && !errors.Is(loser, edge.ErrRouteCollision) {
		t.Fatalf("loser was not collision: %v", loser)
	}
}

func TestAppCapacityIsSharedAndCleanupReclaimsOnlyActiveCapacity(t *testing.T) {
	ctx := context.Background()
	store := openTunnelTestStore(t)
	owner, key := storageEdgeIdentity(t)
	target, _ := storageEdgeIdentity(t)
	_, err := store.db.ExecContext(ctx, `WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x < ?)
        INSERT INTO app_names(public_name, owner_id, active) SELECT 'bulk' || x || '.mesh.test', ?, 1 FROM n`, edge.MaximumTotalRoutes-1, owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReserveAppNames(ctx, []string{"7k3d.mesh.test"}, owner); err != nil {
		t.Fatalf("last slot = %v", err)
	}
	if err := store.ReserveAppNames(ctx, []string{"7k3d.mesh.test"}, owner); err != nil {
		t.Fatalf("idempotent reserve at capacity = %v", err)
	}
	if err := store.ReserveAppNames(ctx, []string{"overflow.mesh.test"}, owner); !errors.Is(err, tunnel.ErrCapacity) {
		t.Fatalf("app overflow = %v", err)
	}
	mutation := signedTunnelMutation(t, key, target, tunnel.Create, "tunnel.mesh.test", 1)
	if err := applyTestTunnel(store, mutation); !errors.Is(err, tunnel.ErrCapacity) {
		t.Fatalf("tunnel bypassed app capacity: %v", err)
	}
	now := time.Now().UTC()
	snapshot := storageSignedSnapshot(t, target, owner, key, 1, now, []edge.Route{{PublicName: "service.mesh.test", ServiceName: "app"}})
	if err := store.ApplyEdgeSnapshot(ctx, snapshot, storageSnapshotDigest(t, snapshot, target, owner), now); !errors.Is(err, tunnel.ErrCapacity) {
		t.Fatalf("snapshot bypassed app capacity: %v", err)
	}
	if err := store.SetAppNameInactive(ctx, "7k3d.mesh.test", owner); err != nil {
		t.Fatalf("cleanup at capacity = %v", err)
	}
	if err := applyTestTunnel(store, mutation); err != nil {
		t.Fatalf("cleanup did not free active slot: %v", err)
	}
	if err := store.ReserveAppNames(ctx, []string{"7k3d.mesh.test"}, owner); !errors.Is(err, tunnel.ErrCollision) {
		t.Fatalf("retired name reused = %v", err)
	}
	if err := store.DeleteTunnelClaim(ctx, mutation.PublicName); err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyEdgeSnapshot(ctx, snapshot, storageSnapshotDigest(t, snapshot, target, owner), now); err != nil {
		t.Fatalf("snapshot after cleanup = %v", err)
	}
	if err := store.ReserveAppNames(ctx, []string{"overflow.mesh.test"}, owner); !errors.Is(err, tunnel.ErrCapacity) {
		t.Fatalf("app ignored service capacity: %v", err)
	}
}

func TestAppStorageRejectsInvalidBoundaryValues(t *testing.T) {
	store := openTunnelTestStore(t)
	ctx := context.Background()
	owner, _ := storageEdgeIdentity(t)
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
	store := openTunnelTestStore(t)
	owner, _ := storageEdgeIdentity(t)
	other, _ := storageEdgeIdentity(t)
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
	updated := []byte(`{"apps":{"7k3d":{"visibility":"public"}}}`)
	if err := store.ReserveAppNamesAndState(ctx, []string{name}, owner, "apps.edge", updated); err != nil {
		t.Fatalf("exact owner name retry: %v", err)
	}
	if err := store.ReserveAppNamesAndState(ctx, []string{name}, other, "apps.edge", initial); !errors.Is(err, tunnel.ErrCollision) {
		t.Fatalf("wrong owner overwrote allocation: %v", err)
	}
	assertAppStateBytes(t, store, updated)
	if err := store.SetAppNameInactive(ctx, name, owner); err != nil {
		t.Fatal(err)
	}
	if err := store.ReserveAppNamesAndState(ctx, []string{name}, owner, "apps.edge", initial); !errors.Is(err, tunnel.ErrCollision) {
		t.Fatalf("retired name resurrected: %v", err)
	}
	assertAppStateBytes(t, store, updated)
}

func TestAllocatedAppStateFailureRollsBackNewName(t *testing.T) {
	ctx := context.Background()
	store := openTunnelTestStore(t)
	owner, _ := storageEdgeIdentity(t)
	initial := []byte(`{"apps":{}}`)
	if err := store.SaveAppState(ctx, "apps.edge", initial); err != nil {
		t.Fatal(err)
	}
	_, err := store.db.ExecContext(ctx, `CREATE TRIGGER reject_app_state BEFORE UPDATE ON app_state
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

func TestAllocatedAppCollisionLeavesStateUnchanged(t *testing.T) {
	ctx := context.Background()
	store := openTunnelTestStore(t)
	owner, key := storageEdgeIdentity(t)
	target, _ := storageEdgeIdentity(t)
	initial := []byte(`{"apps":{}}`)
	if err := store.SaveAppState(ctx, "apps.edge", initial); err != nil {
		t.Fatal(err)
	}
	name := "7k3d.mesh.test"
	claim := signedTunnelMutation(t, key, target, tunnel.Create, name, 1)
	if err := applyTestTunnel(store, claim); err != nil {
		t.Fatal(err)
	}
	if err := store.ReserveAppNamesAndState(ctx, []string{name}, owner, "apps.edge", []byte(`{"apps":{"7k3d":{}}}`)); !errors.Is(err, tunnel.ErrCollision) {
		t.Fatalf("existing tunnel collision = %v", err)
	}
	assertAppStateBytes(t, store, initial)
	exists, err := store.AppNameExists(ctx, name)
	if err != nil || exists {
		t.Fatalf("collision left app name: %v, %v", exists, err)
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
	store := openTunnelTestStore(t)
	owner, _ := storageEdgeIdentity(t)
	other, _ := storageEdgeIdentity(t)
	if err := store.ReserveAppNames(ctx, []string{"7k3d.old.test"}, other); err != nil {
		t.Fatal(err)
	}
	if err := store.ReserveAppNamesAndState(ctx, []string{"7k3d.mesh.test", "7k3d.old.test"}, owner, "edge", []byte(`{"apps":[]}`)); !errors.Is(err, tunnel.ErrCollision) {
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
	store := openTunnelTestStore(t)
	owner, _ := storageEdgeIdentity(t)
	other, _ := storageEdgeIdentity(t)
	if err := store.ReserveAppNames(ctx, []string{"7k3d.old.test"}, other); err != nil {
		t.Fatal(err)
	}
	if err := store.ReserveAppNames(ctx, []string{"7k3d.mesh.test", "7k3d.old.test"}, owner); !errors.Is(err, tunnel.ErrCollision) {
		t.Fatalf("alias conflict: %v", err)
	}
	exists, err := store.AppNameExists(ctx, "7k3d.mesh.test")
	if err != nil || exists {
		t.Fatalf("partial adoption: %v %v", exists, err)
	}
}
