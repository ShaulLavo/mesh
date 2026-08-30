package storage

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/edge"
)

func TestEdgeSnapshotsAreAtomicIdempotentAndRetainClaims(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "mesh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() //nolint:errcheck
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	edgeID, _ := storageEdgeIdentity(t)
	firstID, firstKey := storageEdgeIdentity(t)
	secondID, secondKey := storageEdgeIdentity(t)
	first := storageSignedSnapshot(t, edgeID, firstID, firstKey, 1, now, []edge.Route{
		{PublicName: "app.shaulavo.dev", ServiceName: "app"},
		{PublicName: "app.shaulavo.dev", ServiceName: "app/v2", WakeOnRequest: true},
	})
	firstDigest := storageSnapshotDigest(t, first, edgeID, firstID)
	receivedAt := now.Add(time.Second)
	if err := store.ApplyEdgeSnapshot(ctx, first, firstDigest, receivedAt); err != nil {
		t.Fatal(err)
	}

	// A lost acknowledgement remains recoverable after sender expiry and does
	// not refresh edge-observed liveness.
	if err := store.ApplyEdgeSnapshot(ctx, first, firstDigest, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	state, err := store.LoadEdgeState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(state) != 1 || !state[0].LastSeenAt.Equal(receivedAt) || len(state[0].Snapshot.Routes) != 2 {
		t.Fatalf("restored state = %#v", state)
	}
	if got, err := edge.VerifySnapshot(state[0].Snapshot, edgeID, firstID); err != nil || got != firstDigest {
		t.Fatalf("restored digest = %q, error = %v", got, err)
	}

	lower := storageSignedSnapshot(t, edgeID, firstID, firstKey, 1, now.Add(time.Second), nil)
	lowerDigest := storageSnapshotDigest(t, lower, edgeID, firstID)
	if err := store.ApplyEdgeSnapshot(ctx, lower, lowerDigest, now.Add(2*time.Second)); !errors.Is(err, edge.ErrSequenceConflict) {
		t.Fatalf("equal-different error = %v", err)
	}

	colliding := storageSignedSnapshot(t, edgeID, secondID, secondKey, 1, now, []edge.Route{{PublicName: "app.shaulavo.dev", ServiceName: "app"}})
	if err := store.ApplyEdgeSnapshot(ctx, colliding, storageSnapshotDigest(t, colliding, edgeID, secondID), now); !errors.Is(err, edge.ErrRouteCollision) {
		t.Fatalf("collision error = %v", err)
	}
	state, err = store.LoadEdgeState(ctx)
	if err != nil || len(state) != 1 {
		t.Fatalf("collision partially changed state: %#v, %v", state, err)
	}

	withdrawn := storageSignedSnapshot(t, edgeID, firstID, firstKey, 2, now.Add(time.Second), nil)
	if err := store.ApplyEdgeSnapshot(ctx, withdrawn, storageSnapshotDigest(t, withdrawn, edgeID, firstID), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyEdgeSnapshot(ctx, colliding, storageSnapshotDigest(t, colliding, edgeID, secondID), now.Add(2*time.Second)); err != nil {
		t.Fatalf("released claim remained blocked: %v", err)
	}
	state, err = store.LoadEdgeState(ctx)
	if err != nil || len(state) != 2 || len(state[0].Snapshot.Routes)+len(state[1].Snapshot.Routes) != 1 {
		t.Fatalf("released state = %#v, error = %v", state, err)
	}
}

func TestEdgeOutboxConsumesSequencesAndMatchesExactAcknowledgement(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "mesh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() //nolint:errcheck
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	edgeID, _ := storageEdgeIdentity(t)
	originID, key := storageEdgeIdentity(t)
	first := storageSignedSnapshot(t, edgeID, originID, key, 1, now, nil)
	firstRecord := edge.OutboxRecord{Snapshot: first, Digest: storageSnapshotDigest(t, first, edgeID, originID)}
	if err := store.SaveEdgeOutbox(ctx, firstRecord); err != nil {
		t.Fatal(err)
	}

	var successes int
	var mutex sync.Mutex
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			second := storageSignedSnapshot(t, edgeID, originID, key, 2, now.Add(time.Second), nil)
			err := store.SaveEdgeOutbox(ctx, edge.OutboxRecord{Snapshot: second, Digest: storageSnapshotDigest(t, second, edgeID, originID)})
			if err == nil {
				mutex.Lock()
				successes++
				mutex.Unlock()
			}
		}()
	}
	group.Wait()
	if successes != 1 {
		t.Fatalf("same next sequence successes = %d, want 1", successes)
	}
	record, err := store.LoadEdgeOutbox(ctx, edgeID)
	if err != nil || record.Snapshot.Sequence != 2 || record.Acknowledged {
		t.Fatalf("outbox = %#v, error = %v", record, err)
	}
	if err := store.AcknowledgeEdgeOutbox(ctx, edgeID, 1, firstRecord.Digest); err == nil {
		t.Fatal("stale acknowledgement accepted")
	}
	if err := store.AcknowledgeEdgeOutbox(ctx, edgeID, record.Snapshot.Sequence, record.Digest); err != nil {
		t.Fatal(err)
	}
	record, err = store.LoadEdgeOutbox(ctx, edgeID)
	if err != nil || !record.Acknowledged {
		t.Fatalf("acknowledged outbox = %#v, error = %v", record, err)
	}
}

func storageSignedSnapshot(t *testing.T, edgeID, originID string, key ed25519.PrivateKey, sequence uint64, issuedAt time.Time, routes []edge.Route) edge.Snapshot {
	t.Helper()
	snapshot, err := edge.SignSnapshot(edge.NewSnapshot(edgeID, originID, sequence, issuedAt, issuedAt.Add(5*time.Minute), routes), key, issuedAt)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func storageSnapshotDigest(t *testing.T, snapshot edge.Snapshot, edgeID, originID string) string {
	t.Helper()
	digest, err := edge.VerifySnapshot(snapshot, edgeID, originID)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func storageEdgeIdentity(t *testing.T) (string, ed25519.PrivateKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(publicKey), privateKey
}
