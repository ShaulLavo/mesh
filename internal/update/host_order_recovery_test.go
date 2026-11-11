package update

import (
	"testing"
	"time"

	"github.com/shaul/mesh/internal/updateinstall"
)

func TestHostOrderInterruptedReadCannotExtendVerifiedPermission(t *testing.T) {
	order, a, _, current, now := orderFixture(t)
	a.commit(t, 2, now.Add(-3*time.Hour))
	policy := HostPolicy{After: []string{a.host.ID}}
	if _, err := order.Refresh(t.Context(), policy, current, now); err != nil {
		t.Fatal(err)
	}
	a.commit(t, 3, now.Add(-3*time.Hour))
	a.commit(t, 4, now.Add(-3*time.Hour))
	request := EvidenceRequest{Settled: current.Version, AfterSeq: 1, Limit: 1}
	reply, err := order.Client.Evidence(t.Context(), a.host, request)
	if err != nil || !reply.More {
		t.Fatalf("fixture partial page = %#v, %v", reply, err)
	}
	if err := order.Store.savePeerPage(t.Context(), a.host.ID, request, reply); err != nil {
		t.Fatal(err)
	}
	peer, err := order.Store.hostPeer(t.Context(), a.host.ID)
	if err != nil || !peer.Held || peer.State != EvidenceReading {
		t.Fatalf("partial page hold = %#v, %v", peer, err)
	}
	// Restart at the durable boundary after a page save and before pass completion.
	order.Store, err = OpenStore(order.Store.stateDir())
	if err != nil {
		t.Fatal(err)
	}
	a.server.Close()
	limit, err := order.Refresh(t.Context(), policy, current, now)
	if err == nil {
		t.Fatalf("incomplete pass became offline permission: limit = %#v", limit)
	}
}

func TestHostOrderFailureStillProtectsDependentSource(t *testing.T) {
	order, a, _, current, now := orderFixture(t)
	a.commit(t, 3, now.Add(-3*time.Hour))
	edge := Edge{From: current.Digest, To: evidenceEntry(2, now).Binary, Platform: current.Platform}
	failed := FailedEdge{Edge: edge, Version: "v0.1.2", Phase: updateinstall.RolledBack, At: now}
	if err := a.store.writeAuthority(failedEdgeName(edge), failed); err != nil {
		t.Fatal(err)
	}
	policy := HostPolicy{After: []string{a.host.ID}}
	limit, err := order.Refresh(t.Context(), policy, current, now)
	if err != nil || !limit.failed(edge) {
		t.Fatalf("initial failed edge = %#v, %v", limit, err)
	}
	// This is the same source cleanup recordCommit performs after a successful hop.
	if err := a.store.pruneFailedSource(current.Digest); err != nil {
		t.Fatal(err)
	}
	a.commit(t, 4, now.Add(-3*time.Hour))
	limit, err = order.Refresh(t.Context(), policy, current, now)
	if err != nil {
		t.Fatal(err)
	}
	if !limit.failed(edge) {
		t.Fatalf("dependent's unchanged source lost its failed-edge quarantine: %#v", limit)
	}
}

func TestHostOrderDroppedOfflinePredecessorDoesNotHoldNewPolicy(t *testing.T) {
	f := newHostFixture(t)
	order, a, _, _, now := orderFixture(t)
	order.Store = f.store
	f.updater.Order = order
	a.commit(t, 2, now.Add(-3*time.Hour))
	setPeerMetadata(t, a, discoveryDigest(f.files["v0.1.2/mesh-update.json"]))
	policy, err := f.store.SetHostPolicy(t.Context(), 1, HostPolicy{Mode: HostAutomatic, After: []string{a.host.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := order.Refresh(t.Context(), policy, f.current, now); err != nil {
		t.Fatal(err)
	}
	a.server.Close()
	if _, err := f.store.SetHostPolicy(t.Context(), 2, HostPolicy{Mode: HostAutomatic}); err != nil {
		t.Fatal(err)
	}
	f.tick()
	state, err := f.store.HostState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if state.Owned == nil {
		t.Fatalf("explicit removal of dead canary still holds host: phase = %s, problem = %+v", state.Phase, state.Problem)
	}
	peers, err := f.store.HostPeers(t.Context())
	if err != nil || len(peers) != 1 || peers[0].Problem == "" {
		t.Fatalf("pending retention cleanup = %#v, %v", peers, err)
	}
}
