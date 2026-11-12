package update

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/updateinstall"
)

type orderPeerFixture struct {
	host         Host
	store        *Store
	server       *httptest.Server
	state        atomic.Int32
	failAfterSeq atomic.Uint64
}

func orderIdentity(t *testing.T) (string, ed25519.PrivateKey) {
	t.Helper()
	public, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(public), key
}

func newOrderPeer(t *testing.T, reader string) *orderPeerFixture {
	t.Helper()
	id, key := orderIdentity(t)
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &orderPeerFixture{store: store, host: Host{ID: id, Platform: release.Platform{OS: "linux", Arch: "amd64"}}}
	auth := &transport.Authentication{Key: key, Authorize: func(id string) bool { return id == reader && f.state.Load() != 1 }}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = transport.ServeWithOptions(w, r, transport.ServeOptions{Auth: auth}, f.handle)
	}))
	t.Cleanup(f.server.Close)
	f.host.Endpoint = "ws" + strings.TrimPrefix(f.server.URL, "http")
	return f
}

func (f *orderPeerFixture) handle(ctx context.Context, conn transport.Conn) error {
	frame, err := conn.ReadFrame()
	if err != nil {
		return hostError(err)
	}
	control, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return hostError(err)
	}
	var request EvidenceRequest
	if err := DecodeEvidence(control.Update, &request); err != nil {
		return hostError(err)
	}
	if after := f.failAfterSeq.Load(); after > 0 && request.AfterSeq >= after {
		return errors.New("fixture connection interrupted between pages")
	}
	peer, ok := transport.Peer(ctx)
	if !ok {
		return errors.New("missing fixture peer")
	}
	reply, err := f.store.HostEvidence(ctx, peer.Identity, request)
	if err != nil {
		return hostError(err)
	}
	reply.ID = f.host.ID
	response := protocol.Control{Type: protocol.TypeUpdateEvidenceResult, RequestID: control.RequestID}
	if f.state.Load() == 2 {
		response.Type = protocol.TypeError
		response.ErrorCode = protocol.ErrorCodeUnknownControl
		response.Message = "unsupported"
	}
	if f.state.Load() == 3 {
		reply.ID = peer.Identity
	}
	if f.state.Load() == 4 {
		reply.Entries = []LedgerEntry{{Seq: request.AfterSeq + 1, Metadata: strings.Repeat("x", 65<<10)}}
	}
	response.Update, err = json.Marshal(reply)
	if err != nil {
		return hostError(err)
	}
	data, err := response.Encode()
	if err != nil {
		return hostError(err)
	}
	return hostError(conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: data}))
}

func evidenceEntry(n int, at time.Time) LedgerEntry {
	return LedgerEntry{Request: fmt.Sprintf("%032x", n), Generation: uint64(max(n, 0)), Version: fmt.Sprintf("v0.1.%d", n), Metadata: discoveryDigest([]byte(fmt.Sprint("metadata", n))), Binary: discoveryDigest([]byte(fmt.Sprint("binary", n))), From: discoveryDigest([]byte(fmt.Sprint("binary", n-1))), Origin: "host", CommittedAt: at}
}

func (f *orderPeerFixture) commit(t *testing.T, n int, at time.Time) {
	t.Helper()
	unlock, err := f.store.lockContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := f.store.appendLedger(evidenceEntry(n, at)); err != nil {
		t.Fatal(err)
	}
}

func orderFixture(t *testing.T) (*HostOrder, *orderPeerFixture, *orderPeerFixture, release.Build, time.Time) {
	t.Helper()
	id, key := orderIdentity(t)
	a, b := newOrderPeer(t, id), newOrderPeer(t, id)
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fleet := Fleet{Members: []Host{a.host, b.host}}
	order := &HostOrder{Store: store, Client: Client{ID: id, Key: key}, SavedFleet: func() (Fleet, error) { return fleet, nil }}
	current := release.Build{Version: "v0.1.1", Digest: evidenceEntry(1, time.Now()).Binary, Platform: a.host.Platform}
	return order, a, b, current, time.Now().UTC()
}

func TestHostOrderDisjointBurstSoakAndOffline(t *testing.T) {
	order, a, b, current, now := orderFixture(t)
	policy := HostPolicy{After: []string{a.host.ID, b.host.ID}, Soak: 2 * time.Hour}
	for n := 2; n <= 22; n++ {
		peer := a
		if n%2 == 0 {
			peer = b
		}
		peer.commit(t, n, now.Add(-3*time.Hour))
	}
	a.commit(t, 23, now.Add(-10*time.Minute))
	b.commit(t, 24, now.Add(-10*time.Minute))
	limit, err := order.Refresh(t.Context(), policy, current, now)
	if err != nil || limit.Version != "v0.1.21" {
		t.Fatalf("disjoint burst limit = %#v, %v", limit, err)
	}
	a.server.Close()
	b.server.Close()
	limit, err = order.Refresh(t.Context(), policy, current, now.Add(24*time.Hour))
	if err != nil || limit.Version != "v0.1.23" {
		t.Fatalf("saved evidence after soak = %#v, %v", limit, err)
	}
	reopened, err := OpenStore(order.Store.stateDir())
	if err != nil {
		t.Fatal(err)
	}
	order.Store = reopened
	limit, err = order.Refresh(t.Context(), policy, current, now.Add(400*24*time.Hour))
	if err != nil || limit.Version != "v0.1.23" {
		t.Fatalf("offline restart changed limit = %#v, %v", limit, err)
	}
}

func TestHostOrderReadFailuresDoNotExpire(t *testing.T) {
	cases := []struct {
		name  string
		state int32
		want  EvidenceReadState
	}{{"refused", 1, EvidenceRefused}, {"unsupported", 2, EvidenceUnsupported}, {"wrong identity", 3, EvidenceInvalid}, {"oversize data", 4, EvidenceInvalid}}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) { testHostOrderReadFailure(t, test.state, test.want) })
	}
}

func testHostOrderReadFailure(t *testing.T, state int32, want EvidenceReadState) {
	order, a, b, current, now := orderFixture(t)
	a.commit(t, 2, now.Add(-time.Hour))
	b.commit(t, 3, now.Add(-time.Hour))
	policy := HostPolicy{After: []string{a.host.ID, b.host.ID}}
	if _, err := order.Refresh(t.Context(), policy, current, now); err != nil {
		t.Fatal(err)
	}
	a.state.Store(state)
	if _, err := order.Refresh(t.Context(), policy, current, now.Add(time.Hour)); err == nil {
		t.Fatal("bad read allowed progress")
	}
	peers, err := order.Store.HostPeers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if peers[0].State != want || !peers[0].Held {
		t.Fatalf("read state = %+v", peers[0])
	}
	a.server.Close()
	if _, err := order.Refresh(t.Context(), policy, current, now.Add(365*24*time.Hour)); err == nil {
		t.Fatal("offline expired prior hold")
	}
}

func TestHostOrderFailedIntermediateAndAttention(t *testing.T) {
	order, a, b, current, now := orderFixture(t)
	a.commit(t, 3, now.Add(-time.Hour))
	b.commit(t, 4, now.Add(-time.Hour))
	failed := FailedEdge{Edge: Edge{From: current.Digest, To: evidenceEntry(2, now).Binary, Platform: current.Platform}, Version: "v0.1.2", Phase: updateinstall.RolledBack, At: now}
	if err := a.store.writeAuthority(failedEdgeName(failed.Edge), failed); err != nil {
		t.Fatal(err)
	}
	policy := HostPolicy{After: []string{a.host.ID, b.host.ID}}
	limit, err := order.Refresh(t.Context(), policy, current, now)
	if err != nil || !limit.failed(failed.Edge) || limit.Version != "v0.1.3" {
		t.Fatalf("failed intermediate = %#v, %v", limit, err)
	}
	wrong := failed.Edge
	wrong.Platform.Arch = "arm64"
	if limit.failed(wrong) {
		t.Fatal("failed edge crossed platform")
	}
	if err := b.store.writeAuthority("host-update.json", HostUpdate{Schema: 1, Phase: HostAttention, Problem: &HostProblem{Kind: "rollback", Detail: "operator action required"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := order.Refresh(t.Context(), policy, current, now); err == nil {
		t.Fatal("attention permitted dependent")
	}
}

func TestHostEvidenceAcknowledgementPruningAndDrop(t *testing.T) {
	order, a, _, current, now := orderFixture(t)
	policy := HostPolicy{After: []string{a.host.ID}, Soak: 2 * time.Hour}
	for n := 2; n <= 24; n++ {
		a.commit(t, n, now.Add(-time.Hour))
	}
	a.commit(t, 25, now.Add(-3*time.Hour))
	if _, err := order.Refresh(t.Context(), policy, current, now); err != nil {
		t.Fatal(err)
	}
	entries, _, err := a.store.HostLedger(t.Context(), 0, 64)
	if err != nil || len(entries) != 24 {
		t.Fatalf("burst retention = %d, %v", len(entries), err)
	}
	current.Version = "v0.1.22"
	if _, err := order.Refresh(t.Context(), policy, current, now.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	entries, _, err = a.store.HostLedger(t.Context(), 0, 64)
	if err != nil || len(entries) != 3 || entries[0].Version != "v0.1.23" {
		t.Fatalf("settled retention = %#v, %v", entries, err)
	}
	dependents, err := a.store.HostDependents(t.Context())
	if err != nil || len(dependents) != 1 || dependents[0].Retained != 2 {
		t.Fatalf("dependents = %#v, %v", dependents, err)
	}
	if err := order.ReleaseDropped(t.Context(), HostPolicy{}); err != nil {
		t.Fatal(err)
	}
	entries, _, err = a.store.HostLedger(t.Context(), 0, 64)
	if err != nil || len(entries) != 1 || entries[0].Version != "v0.1.25" {
		t.Fatalf("drop retained latest = %#v, %v", entries, err)
	}
	a.commit(t, 2, now.Add(-time.Hour))
	entries, _, err = a.store.HostLedger(t.Context(), 0, 64)
	if err != nil || len(entries) != 1 || entries[0].Version != "v0.1.25" {
		t.Fatal("pruned request was resurrected")
	}
	peers, err := order.Store.HostPeers(t.Context())
	if err != nil || len(peers) != 0 {
		t.Fatalf("drop retained local peer = %#v, %v", peers, err)
	}
}

func TestHostEvidenceMalformedReply(t *testing.T) {
	entry := evidenceEntry(2, time.Now())
	entry.Seq = 1
	id, _ := orderIdentity(t)
	base := EvidenceReply{Schema: 1, ID: id, Mode: HostManual, Phase: HostIdle, Entries: []LedgerEntry{entry}}
	cases := []func(*EvidenceReply){func(r *EvidenceReply) { r.Entries[0].Metadata = "bad" }, func(r *EvidenceReply) { r.Entries[0].Workers = -1 }, func(r *EvidenceReply) { r.More = true; r.Entries = nil }, func(r *EvidenceReply) { r.FailedNext = "../x" }, func(r *EvidenceReply) { r.Adoption = &HostAdoption{Schema: 2} }, func(r *EvidenceReply) { r.Entries = make([]LedgerEntry, 65) }}
	for _, change := range cases {
		candidate := base
		candidate.Entries = slicesCloneEntries(base.Entries)
		change(&candidate)
		if err := validateEvidenceReply(id, EvidenceRequest{Limit: 64}, candidate); err == nil {
			t.Fatal("accepted malformed evidence")
		}
	}
}

func slicesCloneEntries(entries []LedgerEntry) []LedgerEntry {
	return append([]LedgerEntry(nil), entries...)
}

func TestHostUpdaterUsesPredecessorLimitAndMandatoryProofs(t *testing.T) {
	f := newHostFixture(t)
	order, a, b, _, now := orderFixture(t)
	order.Store = f.store
	a.commit(t, 2, now.Add(-3*time.Hour))
	b.commit(t, 3, now.Add(-3*time.Hour))
	// Use exact controlled publisher pins, including a newer predecessor.
	setPeerMetadata(t, a, discoveryDigest(f.files["v0.1.2/"+release.UpdateMetadataAssetName]))
	setPeerMetadata(t, b, discoveryDigest(f.files["v0.1.3/"+release.UpdateMetadataAssetName]))
	f.updater.Order = order
	if _, err := f.store.SetHostPolicy(t.Context(), 1, HostPolicy{Mode: HostAutomatic, After: []string{a.host.ID, b.host.ID}, Soak: 2 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	f.tick()
	if got := f.installation(); got.Request.Manifest.Version != "v0.1.2" || got.Phase != updateinstall.Granted {
		t.Fatalf("target limit = %s %s", got.Request.Manifest.Version, got.Phase)
	}
	f.finish()
	f.tick()
	if f.current.Version != "v0.1.2" {
		t.Fatalf("dependent skipped canary limit: %s", f.current.Version)
	}
}

func setPeerMetadata(t *testing.T, peer *orderPeerFixture, pin string) {
	t.Helper()
	var entry LedgerEntry
	if err := peer.store.readAuthority(ledgerName(1), &entry); err != nil {
		t.Fatal(err)
	}
	entry.Metadata = pin
	if err := peer.store.writeAuthority(ledgerName(1), entry); err != nil {
		t.Fatal(err)
	}
}

func TestHostOrderPagesKeepOldSoakedCommitAcrossRestart(t *testing.T) {
	order, a, _, current, now := orderFixture(t)
	a.commit(t, 2, now.Add(-3*time.Hour))
	for n := 3; n <= 282; n++ {
		a.commit(t, n, now.Add(-10*time.Minute))
	}
	policy := HostPolicy{After: []string{a.host.ID}, Soak: 2 * time.Hour}
	if _, err := order.Refresh(t.Context(), policy, current, now); err == nil {
		t.Fatal("incomplete pages allowed progress")
	}
	reopened, err := OpenStore(order.Store.stateDir())
	if err != nil {
		t.Fatal(err)
	}
	order.Store = reopened
	limit, err := order.Refresh(t.Context(), policy, current, now)
	if err != nil || limit.Version != "v0.1.2" {
		t.Fatalf("paged wake lost old soaked release = %#v, %v", limit, err)
	}
	limit, err = order.Refresh(t.Context(), policy, current, now.Add(2*time.Hour))
	if err != nil || limit.Version != "v0.1.282" {
		t.Fatalf("paged burst did not soak = %#v, %v", limit, err)
	}
}

func TestHostLimitedDiscoveryDoesNotReadNewerPeerFormat(t *testing.T) {
	f := newHostFixture(t)
	source, err := f.documents.Current(t.Context(), f.current)
	if err != nil {
		t.Fatal(err)
	}
	documents := f.documents.Source()
	read := documents.Read
	documents.Read = func(ctx context.Context, ref release.AssetRef, kind release.DocumentKind) ([]byte, error) {
		if ref.Tag == "v99.0.0" {
			t.Fatal("older reader requested future predecessor metadata")
		}
		return read(ctx, ref, kind)
	}
	step, _, err := hostLimitedDiscoveryPass(t.Context(), f.current, source, DiscoveryCursor{}, documents, &HostLimit{Version: "v99.0.0", Metadata: strings.Repeat("f", 64)})
	if err != nil || step.Next == nil || step.Next.Manifest.Version != "v0.1.2" {
		t.Fatalf("compatible next hop = %#v, %v", step.Next, err)
	}
}

func TestHostUpdaterLimitedTargetStillRequiresSignatureAndRollback(t *testing.T) {
	for _, kind := range []string{"pin", "signature", "rollback"} {
		t.Run(kind, func(t *testing.T) { testLimitedTargetProof(t, kind) })
	}
}

func testLimitedTargetProof(t *testing.T, kind string) {
	f := newHostFixture(t)
	order, a, _, _, now := orderFixture(t)
	order.Store = f.store
	a.commit(t, 2, now.Add(-3*time.Hour))
	pin := discoveryDigest(f.files["v0.1.2/"+release.UpdateMetadataAssetName])
	if kind == "pin" {
		pin = strings.Repeat("e", 64)
	}
	setPeerMetadata(t, a, pin)
	if kind == "signature" {
		f.files["v0.1.2/mesh-update.sig.json"] = []byte(`{}`)
	}
	if kind == "rollback" {
		removeHostRollback(f, "v0.1.2")
	}
	f.updater.Order = order
	if _, err := f.store.SetHostPolicy(t.Context(), 1, HostPolicy{Mode: HostAutomatic, After: []string{a.host.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := f.updater.Tick(t.Context()); err == nil {
		t.Fatalf("%s accepted limited target", kind)
	}
	state, err := f.store.HostState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if state.Owned != nil {
		t.Fatal("invalid limited target reserved installation")
	}
}

func removeHostRollback(f *hostFixture, version string) {
	for path := range f.files {
		if strings.HasPrefix(path, version+"/") && strings.HasSuffix(path, ".rollback.json") {
			delete(f.files, path)
		}
	}
}

func TestHostEvidenceManualHistoryDoesNotGrantOrBlockPinnedEntry(t *testing.T) {
	order, a, _, current, now := orderFixture(t)
	a.commit(t, 2, now.Add(-3*time.Hour))
	var entry LedgerEntry
	if err := a.store.readAuthority(ledgerName(1), &entry); err != nil {
		t.Fatal(err)
	}
	entry.Origin, entry.Metadata = "manual", ""
	if err := a.store.writeAuthority(ledgerName(1), entry); err != nil {
		t.Fatal(err)
	}
	policy := HostPolicy{After: []string{a.host.ID}}
	if _, err := order.Refresh(t.Context(), policy, current, now); err == nil {
		t.Fatal("unpinned manual entry granted permission")
	}
	a.commit(t, 3, now.Add(-3*time.Hour))
	limit, err := order.Refresh(t.Context(), policy, current, now)
	if err != nil || limit.Version != "v0.1.3" {
		t.Fatalf("pinned successor blocked by manual history = %#v, %v", limit, err)
	}
}

func TestHostUpdaterHoldsFailedPeerIntermediateEdge(t *testing.T) {
	f := newHostFixture(t)
	order, a, _, _, now := orderFixture(t)
	order.Store = f.store
	a.commit(t, 3, now.Add(-3*time.Hour))
	setPeerMetadata(t, a, discoveryDigest(f.files["v0.1.3/"+release.UpdateMetadataAssetName]))
	edge := Edge{From: f.current.Digest, To: f.builds["v0.1.2"].Digest, Platform: f.current.Platform}
	failure := FailedEdge{Edge: edge, Version: "v0.1.2", Phase: updateinstall.RolledBack, At: now}
	if err := a.store.writeAuthority(failedEdgeName(edge), failure); err != nil {
		t.Fatal(err)
	}
	f.updater.Order = order
	if _, err := f.store.SetHostPolicy(t.Context(), 1, HostPolicy{Mode: HostAutomatic, After: []string{a.host.ID}}); err != nil {
		t.Fatal(err)
	}
	f.tick()
	state, err := f.store.HostState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if state.Owned != nil || state.Problem == nil || !strings.Contains(state.Problem.Detail, "failed edge") {
		t.Fatalf("failed intermediate staged: %#v", state)
	}
	cursor := DiscoveryCursor{}
	if err := f.store.readAuthority("host-discovery.json", &cursor); err != nil {
		t.Fatal(err)
	}
	if len(cursor.Route.Failed) != 1 {
		t.Fatal("failed hop was not saved for route search")
	}
	retained, err := f.store.retainHostRouteFailures(cursor.Route.Failed, nil)
	if err != nil || len(retained) != 0 {
		t.Fatalf("dropped predecessor retained quarantine = %#v, %v", retained, err)
	}
}

func TestHostOrderHoldDuringDownloadFencesStagedCommit(t *testing.T) {
	f := newHostFixture(t)
	order, a, _, _, now := orderFixture(t)
	order.Store = f.store
	a.commit(t, 2, now.Add(-3*time.Hour))
	setPeerMetadata(t, a, discoveryDigest(f.files["v0.1.2/"+release.UpdateMetadataAssetName]))
	f.updater.Order = order
	if _, err := f.store.SetHostPolicy(t.Context(), 1, HostPolicy{Mode: HostAutomatic, After: []string{a.host.ID}}); err != nil {
		t.Fatal(err)
	}
	blocked := &blockedHostArchive{base: f.documents.Client.HTTPClient.Transport, entered: make(chan struct{}), resume: make(chan struct{})}
	release := sync.OnceFunc(func() { close(blocked.resume) })
	t.Cleanup(release)
	client := *f.documents.Client.HTTPClient
	client.Transport = blocked
	f.settings.Client.HTTPClient = &client
	engine, err := updateinstall.New(f.settings)
	if err != nil {
		t.Fatal(err)
	}
	f.engine = engine
	done := make(chan error, 1)
	go func() { done <- f.updater.Tick(t.Context()) }()
	select {
	case <-blocked.entered:
	case err := <-done:
		t.Fatalf("updater did not reach archive download: %v", err)
	}
	peer, err := f.store.hostPeer(t.Context(), a.host.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.recordPeerRead(t.Context(), peer, now, evidenceError(EvidenceRefused, errors.New("device access withdrawn"))); err != nil {
		t.Fatal(err)
	}
	release()
	if err := <-done; err == nil {
		t.Fatal("download crossed predecessor hold fence")
	}
	if got := f.installation(); got.Phase != updateinstall.Accepted {
		t.Fatalf("staged after hold: %s", got.Phase)
	}
}
