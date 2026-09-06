package edge

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/tunnel"
)

func TestTunnelMutationRateReservesEightFrames(t *testing.T) {
	now := time.Now()
	c := &Controller{now: func() time.Time { return now }, tunnelRates: make(map[string]*tunnelRateEntry)}
	for i := 0; i < 52; i++ {
		if err := c.admitTunnelMutation("key", true, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.admitTunnelMutation("key", true, false); !errors.Is(err, tunnel.ErrRateLimited) {
		t.Fatalf("53rd create = %v", err)
	}
	for i := 0; i < 8; i++ {
		if err := c.admitTunnelMutation("key", true, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.admitTunnelMutation("key", true, true); !errors.Is(err, tunnel.ErrRateLimited) {
		t.Fatalf("61st frame = %v", err)
	}
	now = now.Add(time.Minute)
	if err := c.admitTunnelMutation("key", true, false); err != nil {
		t.Fatal(err)
	}
}

func TestTunnelRateStateCapPreservesOwnerRelease(t *testing.T) {
	now := time.Now()
	c := &Controller{now: func() time.Time { return now }, tunnelRates: make(map[string]*tunnelRateEntry)}
	for i := 0; i < tunnel.MaximumRateEntries; i++ {
		if err := c.admitTunnelMutation(fmt.Sprint(i), false, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.admitTunnelMutation("new", false, false); !errors.Is(err, tunnel.ErrCapacity) {
		t.Fatalf("new key at cap = %v", err)
	}
	if err := c.admitTunnelMutation("owner", true, true); err != nil {
		t.Fatalf("owner release at rate cap: %v", err)
	}
	if len(c.tunnelRates) != tunnel.MaximumRateEntries {
		t.Fatalf("rate entries = %d", len(c.tunnelRates))
	}
	for _, entry := range c.tunnelRates {
		entry.durable = true
	}
	if err := c.admitTunnelMutation("new-owner", true, true); !errors.Is(err, tunnel.ErrCapacity) {
		t.Fatalf("full durable rate map = %v", err)
	}
	if err := c.admitTunnelMutation("owner", true, true); err != nil {
		t.Fatal(err)
	}
}

type claimTestState struct {
	claims   map[string]tunnel.Claim
	versions map[string]tunnel.Ack
	applyErr error
}

func (s *claimTestState) TunnelClaim(_ context.Context, name string) (tunnel.Claim, error) {
	claim, ok := s.claims[name]
	if !ok {
		return tunnel.Claim{}, tunnel.ErrNotFound
	}
	return claim, nil
}
func (s *claimTestState) TunnelVersion(_ context.Context, key string) (tunnel.Ack, error) {
	v, ok := s.versions[key]
	if !ok {
		return tunnel.Ack{}, tunnel.ErrNotFound
	}
	return v, nil
}
func (s *claimTestState) ApplyTunnelMutation(_ context.Context, m tunnel.Mutation, digest string) error {
	if s.applyErr != nil {
		return s.applyErr
	}
	s.versions[m.ClaimantID] = tunnel.Ack{Sequence: m.Sequence, Digest: digest}
	if m.Action == tunnel.Release {
		delete(s.claims, m.PublicName)
		return nil
	}
	s.claims[m.PublicName] = tunnel.Claim{PublicName: m.PublicName, ClaimantID: m.ClaimantID}
	return nil
}
func (s *claimTestState) DeleteTunnelClaim(_ context.Context, name string) error {
	delete(s.claims, name)
	return nil
}

func claimTestController(t *testing.T) (*Controller, *claimTestState, string, ed25519.PrivateKey, *bool) {
	t.Helper()
	id, _ := testIdentity(t)
	_, key := testIdentity(t)
	allowed := true
	state := &claimTestState{claims: make(map[string]tunnel.Claim), versions: make(map[string]tunnel.Ack)}
	c := &Controller{targetID: id, tunnelState: state, authorizeTunnel: func(string) bool { return allowed }, now: time.Now, timeout: time.Second,
		commitGate: make(chan struct{}, 1), lifetime: context.Background(), activeTunnels: make(map[string]*tunnelRoute), tunnelRates: make(map[string]*tunnelRateEntry)}
	return c, state, id, key, &allowed
}

func claimTestSend(t *testing.T, c *Controller, m tunnel.Mutation) tunnel.Ack {
	t.Helper()
	response, handled, err := c.HandleControl(context.Background(), protocol.Control{Type: protocol.TypeTunnelClaim, RequestID: "claim-test", TunnelMutation: &m})
	if err != nil || !handled || response.TunnelAck == nil {
		t.Fatalf("claim response = %#v, %v, %v", response, handled, err)
	}
	return *response.TunnelAck
}

func claimTestSign(t *testing.T, key ed25519.PrivateKey, id string, action tunnel.Action, sequence uint64) tunnel.Mutation {
	t.Helper()
	m, err := tunnel.Sign(key, id, action, "blog.shaulavo.dev", sequence)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestTunnelClaimsRevocationReplayAndRefusalReceipts(t *testing.T) {
	c, state, id, key, allowed := claimTestController(t)
	create := claimTestSign(t, key, id, tunnel.Create, 1)
	ack := claimTestSend(t, c, create)
	if ack.Error != "" || ack.Sequence != 1 || len(state.claims) != 1 {
		t.Fatalf("create = %#v", ack)
	}
	*allowed = false
	if retry := claimTestSend(t, c, create); retry != ack {
		t.Fatalf("revoked exact retry = %#v, want %#v", retry, ack)
	}
	if refused := claimTestSend(t, c, claimTestSign(t, key, id, tunnel.Create, 2)); !strings.Contains(refused.Error, "authorized") {
		t.Fatalf("revoked new create = %#v", refused)
	}
	_, other := testIdentity(t)
	wrong := claimTestSign(t, other, id, tunnel.Release, 1)
	if ack := claimTestSend(t, c, wrong); !strings.Contains(ack.Error, "authorized") {
		t.Fatalf("wrong owner = %#v", ack)
	}
	release := claimTestSign(t, key, id, tunnel.Release, 2)
	ack = claimTestSend(t, c, release)
	if ack.Error != "" || len(state.claims) != 0 || len(state.versions) != 1 {
		t.Fatalf("revoked owner release = %#v", ack)
	}
	if retry := claimTestSend(t, c, release); retry != ack {
		t.Fatalf("release retry = %#v, want %#v", retry, ack)
	}
	*allowed = true
	if ack := claimTestSend(t, c, create); !strings.Contains(ack.Error, "stale") {
		t.Fatalf("post-release replay = %#v", ack)
	}
	conflict := claimTestSign(t, key, id, tunnel.Create, 2)
	if ack := claimTestSend(t, c, conflict); !strings.Contains(ack.Error, "different digest") {
		t.Fatalf("conflicting sequence = %#v", ack)
	}
}

func TestTunnelAmbiguousStorageFailureHasNoReceipt(t *testing.T) {
	c, state, id, key, _ := claimTestController(t)
	state.applyErr = errors.New("commit outcome unavailable")
	m := claimTestSign(t, key, id, tunnel.Create, 1)
	response, _, err := c.HandleControl(context.Background(), protocol.Control{Type: protocol.TypeTunnelClaim, RequestID: "ambiguous", TunnelMutation: &m})
	if err == nil || response.TunnelAck != nil {
		t.Fatalf("ambiguous result = %#v, %v", response, err)
	}
}

func TestTunnelFrameBoundAndCrossEdgeRefuseBeforeState(t *testing.T) {
	c, state, id, key, _ := claimTestController(t)
	m := claimTestSign(t, key, id, tunnel.Create, 1)
	request := protocol.Control{Type: protocol.TypeTunnelClaim, RequestID: "oversize", TunnelMutation: &m, Output: make([]byte, tunnel.MaximumFrameBytes)}
	if _, _, err := c.HandleControl(context.Background(), request); err == nil {
		t.Fatal("accepted oversized frame")
	}
	otherID, _ := testIdentity(t)
	m = claimTestSign(t, key, otherID, tunnel.Create, 1)
	request = protocol.Control{Type: protocol.TypeTunnelClaim, RequestID: "cross-edge", TunnelMutation: &m}
	if _, _, err := c.HandleControl(context.Background(), request); err == nil {
		t.Fatal("accepted another edge's mutation")
	}
	if len(state.claims) != 0 || len(state.versions) != 0 {
		t.Fatal("invalid frame mutated durable state")
	}
}
