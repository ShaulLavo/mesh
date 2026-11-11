package update

import (
	"context"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/updateinstall"
)

func TestHostOrderHeldAtCurrentStillObservesAuthenticatedHead(t *testing.T) {
	f := newHostFixture(t)
	f.head(2)
	f.tick()
	f.finish()
	order, peer, _, _, now := orderFixture(t)
	order.Store = f.store
	peer.commit(t, 2, now.Add(-3*time.Hour))
	setPeerMetadata(t, peer, discoveryDigest(f.files["v0.1.2/mesh-update.json"]))
	f.updater.Order = order
	if _, err := f.store.SetHostPolicy(t.Context(), 1, HostPolicy{Mode: HostAutomatic, After: []string{peer.host.ID}}); err != nil {
		t.Fatal(err)
	}
	f.head(3)
	read := f.updater.Source
	rootReads, verifiedHeads := 0, 0
	f.updater.Source = func(ctx context.Context, current release.Build) (HostSource, release.DocumentSource, error) {
		source, documents, err := read(ctx, current)
		documents = countObservedDocuments(documents, &rootReads, &verifiedHeads)
		return source, documents, err
	}
	f.tick()
	var cursor DiscoveryCursor
	if err := f.store.readAuthority("host-discovery.json", &cursor); err != nil {
		t.Fatal(err)
	}
	if cursor.Head.Tag != "v0.1.3" || verifiedHeads == 0 || rootReads != 1 {
		t.Fatalf("settled hold retained stale head: %s, signatures %d, roots %d", cursor.Head.Tag, verifiedHeads, rootReads)
	}
	state, err := f.store.HostState(t.Context())
	if err != nil || state.Owned != nil || state.Phase != HostWaiting || !state.NextAttempt.After(f.now) {
		t.Fatalf("observation released installation or lost bounded schedule: %+v %v", state, err)
	}
	f.tick()
	if rootReads != 1 {
		t.Fatal("held observation fetched on every local poll")
	}
	if got := f.installation(); got.Phase != updateinstall.Committed || got.Request.Manifest.Version != "v0.1.2" {
		t.Fatal("head observation granted a newer release")
	}
}

func countObservedDocuments(documents release.DocumentSource, roots, heads *int) release.DocumentSource {
	fetch, verify := documents.Read, documents.VerifyMetadata
	documents.Read = func(ctx context.Context, ref release.AssetRef, kind release.DocumentKind) ([]byte, error) {
		if ref.Tag == release.LatestDiscoveryTag {
			*roots++
		}
		data, err := fetch(ctx, ref, kind)
		return data, hostError(err)
	}
	documents.VerifyMetadata = func(ctx context.Context, ref release.AssetRef, data []byte) error {
		if ref.Tag == "v0.1.3" {
			*heads++
		}
		return hostError(verify(ctx, ref, data))
	}
	return documents
}
