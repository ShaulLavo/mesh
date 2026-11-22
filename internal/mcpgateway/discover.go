package mcpgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/mcpwire"
)

const (
	discoverTTL = time.Minute
	// startsPerHost bounds concurrent first-use discoveries on one host,
	// each of which may start an on-demand instance, so a wide profile does
	// not launch every worker on a host at once.
	startsPerHost = 2
)

// startSlot waits for one of the host's discovery slots.
func (g *Gateway) startSlot(ctx context.Context, hostID string) (func(), error) {
	g.mu.Lock()
	slots := g.starts[hostID]
	if slots == nil {
		slots = make(chan struct{}, startsPerHost)
		g.starts[hostID] = slots
	}
	g.mu.Unlock()
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("wait to discover a service on %s: %w", hostID, ctx.Err())
	}
}

// discovery returns a route's upstream capabilities, fetching server/discover
// when the cached copy is expired or from another scope or service state.
// Discovery starts an on-demand service: it is the first use section 7.3
// allows.
func (g *Gateway) discovery(ctx context.Context, downstream mcpwire.Message, sel Selection, hc HostCatalog, entry CatalogEntry) (json.RawMessage, error) {
	key := listingKey{route: keyOf(sel), method: mcpwire.MethodDiscover}
	if l := g.cached(key); l != nil && l.fresh(g.opts.Now()) && l.matches(hc.Scope, entry) && len(l.Items) == 1 {
		return l.Items[0], nil
	}
	gen := g.fence(sel.HostID)
	req, err := listRequest(downstream, mcpwire.MethodDiscover, "")
	if err != nil {
		return nil, err
	}
	release, err := g.startSlot(ctx, sel.HostID)
	if err != nil {
		return nil, err
	}
	result, err := g.exchange(ctx, sel, &Expect{Revision: entry.Revision, Account: entry.Account}, req)
	release()
	if err != nil {
		return nil, err
	}
	caps := member(result, "capabilities")
	if caps == nil {
		caps = json.RawMessage(`{}`)
	}
	ttl := maxTTL
	if t, ok := ttlOf(result); ok {
		ttl = min(ttl, t)
	}
	items := []json.RawMessage{caps}
	g.store(key, &Listing{HostID: sel.HostID, ServiceID: sel.ServiceID, Instance: sel.Instance, Method: mcpwire.MethodDiscover,
		Scope: hc.Scope, Revision: entry.Revision, Account: entry.Account, ListingRevision: entry.ListingRevision,
		Items: items, FetchedAt: g.opts.Now(), TTL: ttl, Digest: digestItems(items), Bytes: len(caps)}, gen)
	return caps, nil
}

// routeCapabilities resolves one route's capabilities for the downstream
// discover result, falling back to a stale cached copy when the host is
// unreachable.
func (g *Gateway) routeCapabilities(ctx context.Context, req mcpwire.Message, sel Selection, catalogs *catalogMemo) json.RawMessage {
	key := listingKey{route: keyOf(sel), method: mcpwire.MethodDiscover}
	hc, entry, err := catalogs.lookup(ctx, sel)
	if err == nil {
		var caps json.RawMessage
		if caps, err = g.discovery(ctx, req, sel, hc, entry); err == nil {
			return caps
		}
	}
	if l, _ := g.fallback(sel, key, err); l != nil && len(l.Items) == 1 {
		return l.Items[0]
	}
	return nil
}

// discover advertises only what the selected routes actually carry. Tools
// are always present (native tools, and profile changes are watched); the
// other primitives appear when a selected service offers them, and resource
// subscriptions only when one supports them upstream. Tasks stay masked.
func (g *Gateway) discover(ctx context.Context, req mcpwire.Message, profile Profile, emit Emit) error {
	routes := g.routes(profile)
	caps := make([]json.RawMessage, len(routes))
	catalogs := newCatalogMemo(g)
	var wg sync.WaitGroup
	sem := make(chan struct{}, refreshFanOut)
	for i, sel := range routes {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			caps[i] = g.routeCapabilities(ctx, req, sel, catalogs)
		})
	}
	wg.Wait()
	result := map[string]any{"capabilities": mergeCapabilities(caps), "instructions": g.instructions(routes)}
	reply, err := g.completeReply(req, result, discoverTTL)
	if err != nil {
		return g.failure(agentapi.InternalError, false, "", "", err)
	}
	return emit(reply, true)
}
func mergeCapabilities(routes []json.RawMessage) map[string]any {
	merged := map[string]any{keyTools: map[string]bool{keyListChanged: true}}
	subscribe := false
	for _, caps := range routes {
		if member(caps, keyPrompts) != nil {
			merged[keyPrompts] = map[string]bool{keyListChanged: true}
		}
		if member(caps, keyResources) != nil {
			merged[keyResources] = map[string]bool{keyListChanged: true}
			subscribe = subscribe || string(member(caps, keyResources, "subscribe")) == jsonTrue
		}
		if member(caps, "completions") != nil {
			merged["completions"] = map[string]any{}
		}
	}
	if subscribe {
		merged[keyResources] = map[string]bool{keyListChanged: true, "subscribe": true}
	}
	return merged
}
