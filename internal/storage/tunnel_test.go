package storage

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/edge"
	"github.com/shaul/mesh/internal/tunnel"
)

func TestTunnelReplayOwnershipAndRecovery(t *testing.T) {
	ctx := context.Background()
	store := openTunnelTestStore(t)
	target, _ := storageEdgeIdentity(t)
	owner, key := storageEdgeIdentity(t)
	_, otherKey := storageEdgeIdentity(t)
	create := signedTunnelMutation(t, key, target, tunnel.Create, "blog.shaulavo.dev", 1)
	if err := applyTestTunnel(store, create); err != nil {
		t.Fatal(err)
	}
	if err := applyTestTunnel(store, create); err != nil {
		t.Fatalf("exact retry: %v", err)
	}
	wrong := signedTunnelMutation(t, otherKey, target, tunnel.Release, create.PublicName, 1)
	if err := applyTestTunnel(store, wrong); !errors.Is(err, tunnel.ErrCollision) {
		t.Fatalf("wrong owner release = %v", err)
	}
	conflict := signedTunnelMutation(t, key, target, tunnel.Release, create.PublicName, 1)
	if err := applyTestTunnel(store, conflict); !errors.Is(err, tunnel.ErrSequenceConflict) {
		t.Fatalf("equal sequence with different digest = %v", err)
	}
	convergent := signedTunnelMutation(t, key, target, tunnel.Create, create.PublicName, 2)
	if err := applyTestTunnel(store, convergent); err != nil {
		t.Fatalf("same owner claim = %v", err)
	}
	release := signedTunnelMutation(t, key, target, tunnel.Release, create.PublicName, 3)
	if err := applyTestTunnel(store, release); err != nil {
		t.Fatal(err)
	}
	if err := applyTestTunnel(store, release); err != nil {
		t.Fatalf("release retry = %v", err)
	}
	if _, err := store.TunnelClaim(ctx, create.PublicName); !errors.Is(err, tunnel.ErrNotFound) {
		t.Fatalf("released claim = %v", err)
	}
	if err := applyTestTunnel(store, create); !errors.Is(err, tunnel.ErrStaleSequence) {
		t.Fatalf("post-release replay = %v", err)
	}
	recreate := signedTunnelMutation(t, key, target, tunnel.Create, create.PublicName, 4)
	if err := applyTestTunnel(store, recreate); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteTunnelClaim(ctx, create.PublicName); err != nil {
		t.Fatal(err)
	}
	version, err := store.TunnelVersion(ctx, owner)
	if err != nil || version.Sequence != 4 {
		t.Fatalf("recovery lost replay history: %+v, %v", version, err)
	}
	if err := applyTestTunnel(store, recreate); err != nil {
		t.Fatalf("recovery idempotent retry = %v", err)
	}
	if _, err := store.TunnelClaim(ctx, create.PublicName); !errors.Is(err, tunnel.ErrNotFound) {
		t.Fatalf("recovery replay recreated claim: %v", err)
	}
	badSignature := signedTunnelMutation(t, key, target, tunnel.Create, create.PublicName, 5)
	badSignature.Signature[0] ^= 1
	if err := store.ApplyTunnelMutation(ctx, badSignature, version.Digest); err == nil {
		t.Fatal("invalid signature accepted by storage")
	}
}

func TestTunnelPerKeyCapacityAllowsRelease(t *testing.T) {
	store := openTunnelTestStore(t)
	target, _ := storageEdgeIdentity(t)
	_, key := storageEdgeIdentity(t)
	for i := range tunnel.MaximumClaimsPerKey {
		mutation := signedTunnelMutation(t, key, target, tunnel.Create, fmt.Sprintf("site%d.shaulavo.dev", i), uint64(i+1))
		if err := applyTestTunnel(store, mutation); err != nil {
			t.Fatal(err)
		}
	}
	overflow := signedTunnelMutation(t, key, target, tunnel.Create, "overflow.shaulavo.dev", 33)
	if err := applyTestTunnel(store, overflow); !errors.Is(err, tunnel.ErrCapacity) {
		t.Fatalf("33rd claim = %v", err)
	}
	convergent := signedTunnelMutation(t, key, target, tunnel.Create, "site0.shaulavo.dev", 34)
	if err := applyTestTunnel(store, convergent); err != nil {
		t.Fatalf("same claim at capacity = %v", err)
	}
	release := signedTunnelMutation(t, key, target, tunnel.Release, "site0.shaulavo.dev", 35)
	if err := applyTestTunnel(store, release); err != nil {
		t.Fatalf("release at capacity = %v", err)
	}
	overflow = signedTunnelMutation(t, key, target, tunnel.Create, overflow.PublicName, 36)
	if err := applyTestTunnel(store, overflow); err != nil {
		t.Fatalf("capacity was not reclaimed: %v", err)
	}
}

func TestTunnelClaimantCapacityPreservesOwnerReleaseAndRecovery(t *testing.T) {
	ctx := context.Background()
	store := openTunnelTestStore(t)
	target, _ := storageEdgeIdentity(t)
	owner, key := storageEdgeIdentity(t)
	claim := signedTunnelMutation(t, key, target, tunnel.Create, "owner.shaulavo.dev", 1)
	if err := applyTestTunnel(store, claim); err != nil {
		t.Fatal(err)
	}
	_, err := store.db.ExecContext(ctx, `WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x < ?)
        INSERT INTO tunnel_highwater (claimant_id, sequence, digest)
        SELECT printf('%043d', x), 1, printf('%064d', x) FROM n`, tunnel.MaximumClaimants-1)
	if err != nil {
		t.Fatal(err)
	}
	_, newcomer := storageEdgeIdentity(t)
	mutation := signedTunnelMutation(t, newcomer, target, tunnel.Create, "new.shaulavo.dev", 1)
	if err := applyTestTunnel(store, mutation); !errors.Is(err, tunnel.ErrCapacity) {
		t.Fatalf("new claimant at capacity = %v", err)
	}
	release := signedTunnelMutation(t, key, target, tunnel.Release, claim.PublicName, 2)
	if err := applyTestTunnel(store, release); err != nil {
		t.Fatalf("owner release at claimant capacity = %v", err)
	}
	claim.Sequence = 3
	claim = signedTunnelMutation(t, key, target, tunnel.Create, claim.PublicName, claim.Sequence)
	if err := applyTestTunnel(store, claim); err != nil {
		t.Fatalf("existing claimant can reuse own tombstone: %v", err)
	}
	if err := store.DeleteTunnelClaim(ctx, claim.PublicName); err != nil {
		t.Fatal(err)
	}
	version, err := store.TunnelVersion(ctx, owner)
	if err != nil || version.Sequence != 3 {
		t.Fatalf("recovery tombstone = %+v, %v", version, err)
	}
}

func TestTunnelCombinedRouteCapacityInBothDirections(t *testing.T) {
	ctx := context.Background()
	store := openTunnelTestStore(t)
	target, _ := storageEdgeIdentity(t)
	origin, originKey := storageEdgeIdentity(t)
	_, key := storageEdgeIdentity(t)
	now := time.Now().UTC()
	base := storageSignedSnapshot(t, target, origin, originKey, 1, now, []edge.Route{{PublicName: "origin.shaulavo.dev", ServiceName: "app"}})
	if err := store.ApplyEdgeSnapshot(ctx, base, storageSnapshotDigest(t, base, target, origin), now); err != nil {
		t.Fatal(err)
	}
	_, err := store.db.ExecContext(ctx, `WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x < ?)
        INSERT INTO edge_routes (origin_id, public_name, service_name, wake_on_request)
        SELECT ?, 'bulk.shaulavo.dev', 'p' || x, 0 FROM n`, edge.MaximumTotalRoutes-2, origin)
	if err != nil {
		t.Fatal(err)
	}
	claim := signedTunnelMutation(t, key, target, tunnel.Create, "last.shaulavo.dev", 1)
	if err := applyTestTunnel(store, claim); err != nil {
		t.Fatalf("last combined slot = %v", err)
	}
	overflow := signedTunnelMutation(t, key, target, tunnel.Create, "overflow.shaulavo.dev", 2)
	if err := applyTestTunnel(store, overflow); !errors.Is(err, tunnel.ErrCapacity) {
		t.Fatalf("combined overflow = %v", err)
	}
	secondOrigin, secondKey := storageEdgeIdentity(t)
	snapshot := storageSignedSnapshot(t, target, secondOrigin, secondKey, 1, now, []edge.Route{{PublicName: "other.shaulavo.dev", ServiceName: "app"}})
	if err := store.ApplyEdgeSnapshot(ctx, snapshot, storageSnapshotDigest(t, snapshot, target, secondOrigin), now); !errors.Is(err, tunnel.ErrCapacity) {
		t.Fatalf("T13 combined overflow = %v", err)
	}
	release := signedTunnelMutation(t, key, target, tunnel.Release, claim.PublicName, 3)
	if err := applyTestTunnel(store, release); err != nil {
		t.Fatalf("release at global capacity = %v", err)
	}
	if err := store.ApplyEdgeSnapshot(ctx, snapshot, storageSnapshotDigest(t, snapshot, target, secondOrigin), now); err != nil {
		t.Fatalf("T13 after release = %v", err)
	}
}

func TestTunnelAndNonRootEdgePublicationRace(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mesh.db")
	first, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close() //nolint:errcheck
	second, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close() //nolint:errcheck
	target, _ := storageEdgeIdentity(t)
	origin, originKey := storageEdgeIdentity(t)
	_, key := storageEdgeIdentity(t)
	now := time.Now().UTC()
	snapshot := storageSignedSnapshot(t, target, origin, originKey, 1, now, []edge.Route{{PublicName: "race.shaulavo.dev", ServiceName: "nested/path"}})
	digest := storageSnapshotDigest(t, snapshot, target, origin)
	claim := signedTunnelMutation(t, key, target, tunnel.Create, "race.shaulavo.dev", 1)
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	workers.Go(func() { <-start; results <- first.ApplyEdgeSnapshot(ctx, snapshot, digest, now) })
	workers.Go(func() { <-start; results <- applyTestTunnel(second, claim) })
	close(start)
	workers.Wait()
	firstResult, secondResult := <-results, <-results
	if firstResult != nil && secondResult != nil || firstResult == nil && secondResult == nil {
		t.Fatalf("expected one collision winner, got %v and %v", firstResult, secondResult)
	}
	loser := errors.Join(firstResult, secondResult)
	if !errors.Is(loser, edge.ErrRouteCollision) && !errors.Is(loser, tunnel.ErrCollision) {
		t.Fatalf("loser was not rejected for hostname collision: %v", loser)
	}
}

func openTunnelTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "mesh.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func signedTunnelMutation(t *testing.T, key ed25519.PrivateKey, target string, action tunnel.Action, name string, sequence uint64) tunnel.Mutation {
	t.Helper()
	mutation, err := tunnel.Sign(key, target, action, name, sequence)
	if err != nil {
		t.Fatal(err)
	}
	return mutation
}

func applyTestTunnel(store *Store, mutation tunnel.Mutation) error {
	digest, err := tunnel.Verify(mutation, mutation.TargetID)
	if err != nil {
		return err
	}
	return store.ApplyTunnelMutation(context.Background(), mutation, digest)
}
