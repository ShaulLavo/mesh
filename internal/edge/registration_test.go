package edge

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/tailnet"
)

func TestControllerPersistsBeforePublishAndIdempotentReplayDoesNotRefresh(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	edgeID, _ := testIdentity(t)
	originID, originKey := testIdentity(t)
	state := newMemoryStateStore()
	registry := testRegistry(t, ModeDirectTLS, now)
	var resolves, pins atomic.Int64
	controller := testController(t, now, edgeID, []OriginConfig{testOriginConfig(originID)}, state, registry,
		func(context.Context, OriginConfig) (netip.AddrPort, error) {
			resolves.Add(1)
			return netip.MustParseAddrPort("100.64.0.8:7337"), nil
		},
		func(context.Context, netip.AddrPort, OriginConfig) error { pins.Add(1); return nil },
	)
	snapshot := signedRegistrationSnapshot(t, edgeID, originID, originKey, 1, now, []Route{{PublicName: "app.shaulavo.dev", ServiceName: "app"}})
	response, handled, err := controller.HandleControl(context.Background(), protocol.Control{
		Type: protocol.TypeEdgeRegister, RequestID: "register-1", EdgeSnapshot: pointerProtocolSnapshot(snapshot),
	})
	if err != nil || !handled || response.Type != protocol.TypeEdgeRegistered || response.EdgeSequence != 1 {
		t.Fatalf("registration = %#v, handled %v, error %v", response, handled, err)
	}
	if resolves.Load() != 1 || pins.Load() != 1 || len(registry.Status()) != 1 || !registry.Status()[0].Online {
		t.Fatalf("registration did not publish after pin: resolves=%d pins=%d status=%#v", resolves.Load(), pins.Load(), registry.Status())
	}
	lastSeen := state.origins[originID].LastSeenAt

	// The same authenticated digest is still acked after sender expiry without
	// endpoint work or liveness refresh.
	controller.now = func() time.Time { return now.Add(time.Hour) }
	response, _, err = controller.HandleControl(context.Background(), protocol.Control{
		Type: protocol.TypeEdgeRegister, RequestID: "register-retry", EdgeSnapshot: pointerProtocolSnapshot(snapshot),
	})
	if err != nil || response.EdgeDigest == "" {
		t.Fatalf("expired idempotent retry = %#v, %v", response, err)
	}
	if resolves.Load() != 1 || pins.Load() != 1 || !state.origins[originID].LastSeenAt.Equal(lastSeen) {
		t.Fatal("idempotent retry refreshed liveness or repeated endpoint work")
	}
}

func TestControllerRestoresVerifiedClaimsOfflineAndFailsClosed(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	edgeID, _ := testIdentity(t)
	originID, originKey := testIdentity(t)
	snapshot := signedRegistrationSnapshot(t, edgeID, originID, originKey, 1, now, []Route{{PublicName: "app.shaulavo.dev", ServiceName: "app"}})
	digest, err := VerifySnapshot(snapshot, edgeID, originID)
	if err != nil {
		t.Fatal(err)
	}
	state := newMemoryStateStore()
	state.origins[originID] = StoredOrigin{Snapshot: snapshot, Digest: digest, LastSeenAt: now}
	registry := testRegistry(t, ModeDirectTLS, now)
	controller := testController(t, now, edgeID, []OriginConfig{testOriginConfig(originID)}, state, registry,
		func(context.Context, OriginConfig) (netip.AddrPort, error) {
			return netip.AddrPort{}, errors.New("must not resolve at restore")
		},
		func(context.Context, netip.AddrPort, OriginConfig) error {
			return errors.New("must not pin at restore")
		},
	)
	if controller == nil || len(registry.Status()) != 1 || registry.Status()[0].Online {
		t.Fatalf("restored status = %#v", registry.Status())
	}

	corrupt := newMemoryStateStore()
	corrupt.origins[originID] = StoredOrigin{Snapshot: snapshot, Digest: strings.Repeat("0", 64), LastSeenAt: now}
	corruptRegistry := testRegistry(t, ModeDirectTLS, now)
	if _, err := NewController(context.Background(), ControllerConfig{
		TargetID: edgeID, Origins: []OriginConfig{testOriginConfig(originID)}, State: corrupt, Registry: corruptRegistry,
		Resolve: func(context.Context, OriginConfig) (netip.AddrPort, error) { return netip.AddrPort{}, nil },
		Pin:     func(context.Context, netip.AddrPort, OriginConfig) error { return nil }, Now: func() time.Time { return now },
	}); err == nil || len(corruptRegistry.Status()) != 0 {
		t.Fatalf("corrupt persisted state error = %v, status = %#v", err, corruptRegistry.Status())
	}
}

func TestControllerRechecksFreshnessAfterPinAndClearsRoutesAfterPostCommitFailure(t *testing.T) {
	issuedAt := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	edgeID, _ := testIdentity(t)
	originID, originKey := testIdentity(t)
	currentTime := issuedAt
	state := newMemoryStateStore()
	registry := testRegistry(t, ModeDirectTLS, issuedAt)
	controller := testController(t, issuedAt, edgeID, []OriginConfig{testOriginConfig(originID)}, state, registry,
		func(context.Context, OriginConfig) (netip.AddrPort, error) {
			return netip.MustParseAddrPort("100.64.0.8:7337"), nil
		},
		func(context.Context, netip.AddrPort, OriginConfig) error {
			currentTime = issuedAt.Add(6 * time.Minute)
			return nil
		},
	)
	controller.now = func() time.Time { return currentTime }
	expiring := signedRegistrationSnapshot(t, edgeID, originID, originKey, 1, issuedAt, []Route{{PublicName: "app.shaulavo.dev", ServiceName: "app"}})
	if _, _, err := controller.HandleControl(context.Background(), protocol.Control{
		Type: protocol.TypeEdgeRegister, RequestID: "expired-during-pin", EdgeSnapshot: pointerProtocolSnapshot(expiring),
	}); err == nil || len(state.origins) != 0 {
		t.Fatalf("expired-during-pin error = %v, state = %#v", err, state.origins)
	}

	currentTime = issuedAt
	controller.pin = func(context.Context, netip.AddrPort, OriginConfig) error { return nil }
	accepted := signedRegistrationSnapshot(t, edgeID, originID, originKey, 2, issuedAt, []Route{{PublicName: "app.shaulavo.dev", ServiceName: "app"}})
	if _, _, err := controller.HandleControl(context.Background(), protocol.Control{
		Type: protocol.TypeEdgeRegister, RequestID: "accepted", EdgeSnapshot: pointerProtocolSnapshot(accepted),
	}); err != nil {
		t.Fatal(err)
	}
	if len(registry.Status()) != 1 {
		t.Fatal("accepted route did not publish")
	}
	state.failAtApplyCount = state.applyCount + 1
	withdrawn := signedRegistrationSnapshot(t, edgeID, originID, originKey, 3, issuedAt.Add(time.Second), nil)
	if _, _, err := controller.HandleControl(context.Background(), protocol.Control{
		Type: protocol.TypeEdgeRegister, RequestID: "withdraw", EdgeSnapshot: pointerProtocolSnapshot(withdrawn),
	}); err == nil || len(registry.Status()) != 0 {
		t.Fatalf("post-commit failure error = %v, public status = %#v", err, registry.Status())
	}
}

func TestControllerReconcilesAmbiguousApplyBeforeReturning(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	edgeID, _ := testIdentity(t)
	originID, key := testIdentity(t)
	state := newMemoryStateStore()
	registry := testRegistry(t, ModeDirectTLS, now)
	controller := testController(t, now, edgeID, []OriginConfig{testOriginConfig(originID)}, state, registry,
		func(context.Context, OriginConfig) (netip.AddrPort, error) {
			return netip.MustParseAddrPort("100.64.0.8:7337"), nil
		},
		func(context.Context, netip.AddrPort, OriginConfig) error { return nil },
	)
	first := signedRegistrationSnapshot(t, edgeID, originID, key, 1, now, []Route{{PublicName: "app.shaulavo.dev", ServiceName: "app"}})
	if _, _, err := controller.HandleControl(context.Background(), protocol.Control{Type: protocol.TypeEdgeRegister, RequestID: "first", EdgeSnapshot: pointerProtocolSnapshot(first)}); err != nil {
		t.Fatal(err)
	}
	state.commitThenError = true
	replacement := signedRegistrationSnapshot(t, edgeID, originID, key, 2, now.Add(time.Second), []Route{{PublicName: "new.shaulavo.dev", ServiceName: "app"}})
	request := protocol.Control{Type: protocol.TypeEdgeRegister, RequestID: "replace", EdgeSnapshot: pointerProtocolSnapshot(replacement)}
	if _, _, err := controller.HandleControl(context.Background(), request); err == nil {
		t.Fatal("ambiguous Apply error was hidden")
	}
	if len(registry.Status()) != 1 || registry.Status()[0].PublicName != "new.shaulavo.dev" || !registry.Status()[0].Online {
		t.Fatalf("ambiguous committed replacement did not converge online: %#v", registry.Status())
	}
	state.commitThenError = false
	request.RequestID = "replace-retry"
	response, _, err := controller.HandleControl(context.Background(), request)
	if err != nil || response.Type != protocol.TypeEdgeRegistered || response.EdgeSequence != 2 {
		t.Fatalf("exact retry = %#v, %v", response, err)
	}
}

func TestTailscaleResolverRejectsForeignAddressesAndPrefersIPv4(t *testing.T) {
	origin := OriginConfig{TailscaleName: "origin.example.ts.net", ControlPort: 7337}
	resolver := TailscaleResolver(func(context.Context) ([]tailnet.Peer, error) {
		return []tailnet.Peer{{Name: origin.TailscaleName, Online: true, Addrs: []string{"fd7a:115c:a1e0::8", "100.64.0.8"}}}, nil
	})
	endpoint, err := resolver(context.Background(), origin)
	if err != nil || endpoint.String() != "100.64.0.8:7337" {
		t.Fatalf("resolved endpoint = %s, %v", endpoint, err)
	}
	resolver = TailscaleResolver(func(context.Context) ([]tailnet.Peer, error) {
		return []tailnet.Peer{{Name: origin.TailscaleName, Online: true, Addrs: []string{"100.64.0.8", "203.0.113.1"}}}, nil
	})
	if _, err := resolver(context.Background(), origin); err == nil {
		t.Fatal("mixed foreign address accepted")
	}
	resolver = TailscaleResolver(func(context.Context) ([]tailnet.Peer, error) {
		return []tailnet.Peer{
			{Name: origin.TailscaleName, Online: true, Addrs: []string{"100.64.0.8"}},
			{Name: origin.TailscaleName, Online: true, Addrs: []string{"100.64.0.9"}},
		}, nil
	})
	if _, err := resolver(context.Background(), origin); err == nil {
		t.Fatal("duplicate peer name accepted")
	}
}

func TestControllerSerializesOneOriginWithoutBlockingAnother(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	edgeID, _ := testIdentity(t)
	firstID, firstKey := testIdentity(t)
	secondID, secondKey := testIdentity(t)
	firstOrigin := testOriginConfig(firstID)
	secondOrigin := testOriginConfig(secondID)
	secondOrigin.TailscaleName = "second.example.ts.net"
	entered := make(chan string, 3)
	release := make(chan struct{})
	controller := testController(t, now, edgeID, []OriginConfig{firstOrigin, secondOrigin}, newMemoryStateStore(), testRegistry(t, ModeDirectTLS, now),
		func(_ context.Context, origin OriginConfig) (netip.AddrPort, error) {
			entered <- origin.Identity
			<-release
			if origin.Identity == firstID {
				return netip.MustParseAddrPort("100.64.0.8:7337"), nil
			}
			return netip.MustParseAddrPort("100.64.0.9:7337"), nil
		},
		func(context.Context, netip.AddrPort, OriginConfig) error { return nil },
	)
	requests := []protocol.Control{
		{Type: protocol.TypeEdgeRegister, RequestID: "first", EdgeSnapshot: pointerProtocolSnapshot(signedRegistrationSnapshot(t, edgeID, firstID, firstKey, 1, now, []Route{{PublicName: "first.shaulavo.dev", ServiceName: "app"}}))},
		{Type: protocol.TypeEdgeRegister, RequestID: "second", EdgeSnapshot: pointerProtocolSnapshot(signedRegistrationSnapshot(t, edgeID, secondID, secondKey, 1, now, []Route{{PublicName: "second.shaulavo.dev", ServiceName: "app"}}))},
	}
	done := make(chan error, 2)
	for _, request := range requests {
		go func() { _, _, err := controller.HandleControl(context.Background(), request); done <- err }()
	}
	<-entered
	<-entered
	close(release)
	for range requests {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}

	// A canceled duplicate does not queue behind the in-flight endpoint probe.
	entered = make(chan string, 2)
	release = make(chan struct{})
	controller.resolve = func(_ context.Context, origin OriginConfig) (netip.AddrPort, error) {
		entered <- origin.Identity
		<-release
		return netip.MustParseAddrPort("100.64.0.8:7337"), nil
	}
	next := protocol.Control{Type: protocol.TypeEdgeRegister, RequestID: "duplicate-a", EdgeSnapshot: pointerProtocolSnapshot(signedRegistrationSnapshot(t, edgeID, firstID, firstKey, 2, now.Add(time.Second), []Route{{PublicName: "first.shaulavo.dev", ServiceName: "app"}}))}
	duplicate := next
	duplicate.RequestID = "duplicate-b"
	done = make(chan error, 1)
	go func() { _, _, err := controller.HandleControl(context.Background(), next); done <- err }()
	<-entered
	duplicateContext, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, _, err := controller.HandleControl(duplicateContext, duplicate); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("canceled duplicate error = %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	duplicate.RequestID = "duplicate-retry"
	if _, _, err := controller.HandleControl(context.Background(), duplicate); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
		t.Fatal("idempotent duplicate repeated endpoint work")
	default:
	}
}

func TestControlPinnerRejectsNoncanonicalPaths(t *testing.T) {
	for _, value := range []string{"", "mesh", "/a/../mesh", "/mesh?x=1", "/m%65sh", "/mesh#fragment"} {
		if err := validateControlPath(value); err == nil {
			t.Fatalf("invalid WebSocket path %q accepted", value)
		}
	}
}

func testController(t *testing.T, now time.Time, edgeID string, origins []OriginConfig, state StateStore, registry *Registry, resolve ResolveOrigin, pin PinOrigin) *Controller {
	t.Helper()
	controller, err := NewController(context.Background(), ControllerConfig{
		TargetID: edgeID, Origins: origins, State: state, Registry: registry, Resolve: resolve, Pin: pin,
		Now: func() time.Time { return now }, RequestTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return controller
}

func testOriginConfig(identity string) OriginConfig {
	return OriginConfig{
		Identity: identity, DisplayAlias: "Desktop", TailscaleName: "origin.example.ts.net",
		ControlPort: 7337, WebSocketPath: "/mesh",
	}
}

func signedRegistrationSnapshot(t *testing.T, edgeID, originID string, key ed25519.PrivateKey, sequence uint64, issuedAt time.Time, routes []Route) Snapshot {
	t.Helper()
	snapshot, err := SignSnapshot(NewSnapshot(edgeID, originID, sequence, issuedAt, issuedAt.Add(5*time.Minute), routes), key, issuedAt)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func pointerProtocolSnapshot(snapshot Snapshot) *protocol.EdgeSnapshot {
	encoded := snapshotToProtocol(snapshot)
	return &encoded
}

type memoryStateStore struct {
	mu               sync.Mutex
	origins          map[string]StoredOrigin
	failAtApplyCount int
	applyCount       int
	commitThenError  bool
}

func newMemoryStateStore() *memoryStateStore {
	return &memoryStateStore{origins: make(map[string]StoredOrigin)}
}

func (s *memoryStateStore) EdgeSnapshotVersion(_ context.Context, originID string) (SnapshotVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, exists := s.origins[originID]
	if !exists {
		return SnapshotVersion{}, ErrSnapshotNotFound
	}
	return SnapshotVersion{Sequence: stored.Snapshot.Sequence, Digest: stored.Digest}, nil
}

func (s *memoryStateStore) ApplyEdgeSnapshot(_ context.Context, snapshot Snapshot, digest string, receivedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for originID, stored := range s.origins {
		if originID == snapshot.OriginID {
			continue
		}
		for _, existing := range stored.Snapshot.Routes {
			for _, candidate := range snapshot.Routes {
				if existing.PublicName == candidate.PublicName && existing.ServiceName == candidate.ServiceName {
					return ErrRouteCollision
				}
			}
		}
	}
	s.origins[snapshot.OriginID] = StoredOrigin{Snapshot: cloneSnapshot(snapshot), Digest: digest, LastSeenAt: receivedAt}
	s.applyCount++
	if s.commitThenError {
		return errors.New("injected ambiguous Apply error")
	}
	return nil
}

func (s *memoryStateStore) LoadEdgeState(context.Context) ([]StoredOrigin, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failAtApplyCount > 0 && s.applyCount >= s.failAtApplyCount {
		return nil, errors.New("injected load failure")
	}
	result := make([]StoredOrigin, 0, len(s.origins))
	for _, stored := range s.origins {
		stored.Snapshot = cloneSnapshot(stored.Snapshot)
		result = append(result, stored)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Snapshot.OriginID < result[j].Snapshot.OriginID })
	return result, nil
}
