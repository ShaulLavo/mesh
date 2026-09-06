package edge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/tunnel"
)

type tunnelRateEntry struct {
	started time.Time
	total   int
	creates int
	durable bool
}

func (c *Controller) handleTunnelClaim(ctx context.Context, request protocol.Control) (protocol.Control, bool, error) {
	if c.tunnelState == nil || c.authorizeTunnel == nil {
		return protocol.Control{}, true, errors.New("edge: tunnel claims are not configured")
	}
	if err := validateEdgeRequestID(request.RequestID); err != nil {
		return protocol.Control{}, true, err
	}
	encoded, err := json.Marshal(request)
	if err != nil || len(encoded) > tunnel.MaximumFrameBytes || request.TunnelMutation == nil {
		return protocol.Control{}, true, errors.New("edge: tunnel claim requires a mutation frame of at most 4 KiB")
	}
	m := *request.TunnelMutation
	digest, err := tunnel.Verify(m, c.targetID)
	if err != nil {
		return protocol.Control{}, true, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if err := c.acquireCommit(ctx); err != nil {
		return protocol.Control{}, true, err
	}
	defer c.releaseCommit()
	err = c.applyTunnelClaimLocked(ctx, m, digest)
	if err != nil && !definitiveTunnelRefusal(err) {
		return protocol.Control{}, true, err
	}
	ack := &tunnel.Ack{Sequence: m.Sequence, Digest: digest}
	if err != nil {
		ack.Error = err.Error()
	}
	return protocol.Control{Type: protocol.TypeTunnelClaimed, RequestID: request.RequestID, TunnelAck: ack}, true, nil
}

func definitiveTunnelRefusal(err error) bool {
	for _, refusal := range []error{tunnel.ErrUnauthorized, tunnel.ErrCollision, tunnel.ErrStaleSequence, tunnel.ErrSequenceConflict, tunnel.ErrCapacity, tunnel.ErrActive, tunnel.ErrNotFound, tunnel.ErrRateLimited, ErrRouteCollision} {
		if errors.Is(err, refusal) {
			return true
		}
	}
	return false
}

func (c *Controller) applyTunnelClaimLocked(ctx context.Context, m tunnel.Mutation, digest string) error {
	version, err := c.tunnelState.TunnelVersion(ctx, m.ClaimantID)
	if err != nil && !errors.Is(err, tunnel.ErrNotFound) {
		return err
	}
	durable := err == nil
	exact := durable && version.Sequence == m.Sequence && version.Digest == digest
	authorized := c.authorizeTunnel(m.ClaimantID)
	if !authorized && !exact && (m.Action == tunnel.Create || !durable) {
		return tunnel.ErrUnauthorized
	}
	if err := c.admitTunnelMutation(m.ClaimantID, durable, exact || m.Action == tunnel.Release); err != nil {
		return err
	}
	if durable && version.Sequence > m.Sequence {
		return tunnel.ErrStaleSequence
	}
	if durable && version.Sequence == m.Sequence && !exact {
		return tunnel.ErrSequenceConflict
	}
	if exact {
		return nil
	}
	if m.Action == tunnel.Release {
		if err := c.checkTunnelOwnerLocked(ctx, m, durable); err != nil {
			return err
		}
	}
	if m.Action == tunnel.Release && c.activeTunnels[m.PublicName] != nil {
		return tunnel.ErrActive
	}
	if err := c.tunnelState.ApplyTunnelMutation(ctx, m, digest); err != nil {
		return err
	}
	c.tunnelRates[m.ClaimantID].durable = true
	return nil
}

func (c *Controller) checkTunnelOwnerLocked(ctx context.Context, m tunnel.Mutation, durable bool) error {
	claim, err := c.tunnelState.TunnelClaim(ctx, m.PublicName)
	if errors.Is(err, tunnel.ErrNotFound) && durable {
		return nil
	}
	if err != nil {
		return err
	}
	if claim.ClaimantID != m.ClaimantID {
		return tunnel.ErrUnauthorized
	}
	return nil
}

func (c *Controller) admitTunnelMutation(claimant string, durable, priority bool) error {
	now := c.now()
	entry := c.tunnelRates[claimant]
	if entry == nil {
		if err := c.makeTunnelRateRoom(now, durable); err != nil {
			return err
		}
		entry = &tunnelRateEntry{started: now, durable: durable}
		c.tunnelRates[claimant] = entry
	}
	if now.Sub(entry.started) >= time.Minute {
		*entry = tunnelRateEntry{started: now, durable: entry.durable || durable}
	}
	if entry.total >= 60 || !priority && entry.creates >= 52 {
		return tunnel.ErrRateLimited
	}
	entry.total++
	if !priority {
		entry.creates++
	}
	return nil
}

func (c *Controller) makeTunnelRateRoom(now time.Time, durable bool) error {
	if len(c.tunnelRates) < tunnel.MaximumRateEntries {
		return nil
	}
	for key, entry := range c.tunnelRates {
		if now.Sub(entry.started) >= time.Minute || durable && !entry.durable {
			delete(c.tunnelRates, key)
			return nil
		}
	}
	return tunnel.ErrCapacity
}

// ActivateTunnel installs the HTTP route before SSH can acknowledge the forward.
// The returned closure owns one activation, even after the same key reconnects.
func (c *Controller) ActivateTunnel(ctx context.Context, claimantID, fullName string, endpoint tunnel.Endpoint) (func(), error) {
	if c.tunnelState == nil || c.authorizeTunnel == nil || endpoint == nil {
		return nil, errors.New("edge: reverse tunnels are not configured")
	}
	if err := tunnel.ValidateHostname(fullName); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if err := c.acquireCommit(ctx); err != nil {
		return nil, err
	}
	defer c.releaseCommit()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.lifetime.Err() != nil || !c.authorizeTunnel(claimantID) {
		return nil, tunnel.ErrUnauthorized
	}
	claim, err := c.tunnelState.TunnelClaim(ctx, fullName)
	if err != nil {
		return nil, err
	}
	if claim.ClaimantID != claimantID {
		return nil, tunnel.ErrUnauthorized
	}
	if c.activeTunnels[fullName] != nil {
		return nil, fmt.Errorf("%w: %s already has an active forward", tunnel.ErrCollision, fullName)
	}
	if c.forwardsForKey(claimantID) >= tunnel.MaximumForwardsPerKey {
		return nil, fmt.Errorf("%w: at most %d active forwards per key", tunnel.ErrCapacity, tunnel.MaximumForwardsPerKey)
	}
	route := c.registry.newTunnelRoute(fullName, claimantID, endpoint)
	release := func() { c.deactivateTunnel(route) }
	route.release = release
	c.activeTunnels[fullName] = route
	c.registry.setTunnel(route)
	return release, nil
}

func (c *Controller) forwardsForKey(claimantID string) int {
	count := 0
	for _, route := range c.activeTunnels {
		if route.claimantID == claimantID {
			count++
		}
	}
	return count
}

func (c *Controller) deactivateTunnel(route *tunnelRoute) {
	// Liveness cannot wait behind durable mutations during a network failure.
	// The token's removal still goes through the shared mutation gate.
	route.active.Store(false)
	// Cleanup must not inherit the dead SSH connection's cancellation.
	_ = c.acquireCommit(context.Background())
	if c.activeTunnels[route.publicName] == route {
		delete(c.activeTunnels, route.publicName)
		c.registry.removeTunnel(route)
	}
	c.releaseCommit()
	route.close()
}

// RecoverTunnelClaim is called only after the daemon verifies its Unix socket
// trust marker. The retired owner's high-water row is deliberately untouched.
func (c *Controller) RecoverTunnelClaim(ctx context.Context, fullName string) error {
	if c.tunnelState == nil {
		return errors.New("edge: reverse tunnels are not configured")
	}
	if err := tunnel.ValidateHostname(fullName); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if err := c.acquireCommit(ctx); err != nil {
		return err
	}
	route := c.activeTunnels[fullName]
	if route != nil {
		delete(c.activeTunnels, fullName)
		c.registry.removeTunnel(route)
	}
	err := c.tunnelState.DeleteTunnelClaim(ctx, fullName)
	c.releaseCommit()
	if route != nil {
		route.close()
	}
	return err
}

// CloseTunnels withdraws every active route before the daemon closes SSH.
func (c *Controller) CloseTunnels() {
	_ = c.acquireCommit(context.Background())
	routes := make([]*tunnelRoute, 0, len(c.activeTunnels))
	for name, route := range c.activeTunnels {
		c.registry.removeTunnel(route)
		delete(c.activeTunnels, name)
		routes = append(routes, route)
	}
	c.releaseCommit()
	for _, route := range routes {
		route.close()
	}
}
