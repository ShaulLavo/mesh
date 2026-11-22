package mcpgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/mcpconnect"
	"github.com/shaul/mesh/internal/mcpwire"
)

// Section 7.3 and 2.5 bounds.
const (
	PageSize          = 200
	maxTTL            = 5 * time.Minute
	staleFor          = 24 * time.Hour
	nativeTTL         = 5 * time.Minute
	maxListingPages   = 64
	maxListingBytes   = 8 << 20
	maxRetainedBytes  = 32 << 20
	pageByteBudget    = 3 << 20
	refreshFanOut     = 8
	listRequestPrefix = "mesh-gw-"
)

// Listing is one route's complete upstream listing for one method, as the
// upstream sent its entries, bound to the host scope and service state it
// was fetched under. Names and URIs are rewritten when merged.
type Listing struct {
	HostID          string            `json:"hostId"`
	ServiceID       string            `json:"serviceId"`
	Instance        string            `json:"instance,omitempty"`
	Method          string            `json:"method"`
	Params          string            `json:"params,omitempty"`
	Scope           string            `json:"scope"`
	Revision        int64             `json:"revision"`
	Account         string            `json:"account,omitempty"`
	ListingRevision string            `json:"listingRevision,omitempty"`
	Items           []json.RawMessage `json:"items"`
	// Extras are the upstream's own page-level fields and metadata.
	Extras    json.RawMessage `json:"extras,omitempty"`
	FetchedAt time.Time       `json:"fetchedAt"`
	TTL       time.Duration   `json:"ttl"`
	Digest    string          `json:"digest"`
	Bytes     int             `json:"bytes"`
}

func (l *Listing) fresh(now time.Time) bool  { return now.Before(l.FetchedAt.Add(l.TTL)) }
func (l *Listing) usable(now time.Time) bool { return now.Before(l.FetchedAt.Add(staleFor)) }

// matches reports whether the listing was fetched under the scope and
// service state the host reports now.
func (l *Listing) matches(scope string, e CatalogEntry) bool {
	return l.Scope == scope && l.Revision == e.Revision && l.Account == e.Account && l.ListingRevision == e.ListingRevision
}

// Freshness of a route's listing in a reply.
const (
	freshLive    = "live"
	freshCached  = "cached"
	freshStale   = "stale"
	freshExpired = "expired"
)

// Status is the gateway's view of one profile entry, reported on every list
// page under MetaRoutes.
type Status struct {
	Alias      string        `json:"alias"`
	HostID     string        `json:"hostId"`
	ServiceID  string        `json:"serviceId"`
	Name       string        `json:"name,omitempty"`
	State      string        `json:"state"`
	Freshness  string        `json:"freshness,omitempty"`
	Code       agentapi.Code `json:"code,omitempty"`
	Message    string        `json:"message,omitempty"`
	Revision   int64         `json:"revision,omitempty"`
	ObservedAt time.Time     `json:"observedAt"`
}

type listKind struct {
	method, field, idField, kind string
}

var (
	toolsKind     = listKind{mcpwire.MethodListTools, keyTools, keyName, KindTool}
	promptsKind   = listKind{mcpwire.MethodListPrompts, keyPrompts, keyName, KindPrompt}
	resourcesKind = listKind{mcpwire.MethodListResources, keyResources, keyURI, ""}
	templatesKind = listKind{mcpwire.MethodListResourceTemplates, "resourceTemplates", "uriTemplate", ""}
)

func kindFor(method string) listKind {
	switch method {
	case mcpwire.MethodListPrompts:
		return promptsKind
	case mcpwire.MethodListResources:
		return resourcesKind
	case mcpwire.MethodListResourceTemplates:
		return templatesKind
	default:
		return toolsKind
	}
}

// capability is the discover capability a listing needs.
func (k listKind) capability() string {
	switch k.method {
	case mcpwire.MethodListTools:
		return keyTools
	case mcpwire.MethodListPrompts:
		return keyPrompts
	default:
		return keyResources
	}
}

func (g *Gateway) cached(key listingKey) *Listing {
	g.mu.Lock()
	l := g.listings[key]
	g.mu.Unlock()
	if l != nil {
		return l
	}
	stored, ok := g.opts.Cache.Load(key.route.host, key.route.service, key.route.instance, key.method, key.params)
	if !ok {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.listings[key] == nil {
		g.retainLocked(key, stored)
	}
	return g.listings[key]
}

// store keeps a fetched listing unless the host's listings were retired
// after the fetch began (gen is the fence the fetch started under).
func (g *Gateway) store(key listingKey, l *Listing, gen uint64) bool {
	g.mu.Lock()
	if g.hostGen[key.route.host] != gen {
		g.mu.Unlock()
		return false
	}
	g.retainLocked(key, l)
	g.mu.Unlock()
	g.opts.Cache.Save(l)
	return true
}

// retainLocked keeps l in memory within the retained byte budget, evicting
// the oldest other listings; evicted ones are fetched again when needed.
func (g *Gateway) retainLocked(key listingKey, l *Listing) {
	if old := g.listings[key]; old != nil {
		g.retained -= old.Bytes
	}
	g.listings[key] = l
	g.retained += l.Bytes
	for g.retained > maxRetainedBytes {
		oldest, found := g.oldestLocked(key)
		if !found {
			return
		}
		g.retained -= g.listings[oldest].Bytes
		delete(g.listings, oldest)
	}
}

func (g *Gateway) oldestLocked(keep listingKey) (listingKey, bool) {
	var oldest listingKey
	var at time.Time
	found := false
	for k, l := range g.listings {
		if k == keep || (found && !l.FetchedAt.Before(at)) {
			continue
		}
		oldest, at, found = k, l.FetchedAt, true
	}
	return oldest, found
}

func (g *Gateway) fence(host string) uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.hostGen[host]
}

func (g *Gateway) refreshLock(key listingKey) *sync.Mutex {
	g.mu.Lock()
	defer g.mu.Unlock()
	m := g.refreshMu[key]
	if m == nil {
		m = &sync.Mutex{}
		g.refreshMu[key] = m
	}
	return m
}

// dropHost discards this caller's listings of one host, in memory and on
// disk, and fences saves that started before.
func (g *Gateway) dropHost(hostID string) {
	g.mu.Lock()
	g.hostGen[hostID]++
	delete(g.hostScope, hostID)
	for key, l := range g.listings {
		if key.route.host == hostID {
			g.retained -= l.Bytes
			delete(g.listings, key)
		}
	}
	g.mu.Unlock()
	g.opts.Cache.DropHost(hostID)
}

func (g *Gateway) dropRoute(route routeKey) {
	g.mu.Lock()
	for key, l := range g.listings {
		if key.route.host == route.host && key.route.service == route.service {
			g.retained -= l.Bytes
			delete(g.listings, key)
		}
	}
	g.mu.Unlock()
	g.opts.Cache.DropRoute(route.host, route.service)
}

// invalidate expires a route's cached listings in memory and on disk, so
// the next list revalidates them; they stay usable as a stale fallback.
func (g *Gateway) invalidate(sel Selection, methods ...string) {
	route := keyOf(sel)
	g.mu.Lock()
	for key, l := range g.listings {
		if key.route != route || (len(methods) > 0 && !slices.Contains(methods, key.method)) {
			continue
		}
		expired := *l
		expired.TTL = 0
		g.listings[key] = &expired
	}
	g.mu.Unlock()
	g.opts.Cache.Expire(route.host, route.service, route.instance, methods...)
}

// observeFailure records what a failure says about a route and discards
// listings that authorization no longer covers.
func (g *Gateway) observeFailure(sel Selection, rf *mcpconnect.RouteFailure) {
	state := stateForCode(rf.Code)
	g.setStatusCode(sel, state, rf.Code, rf.Message)
	g.opts.Log(fmt.Sprintf("mesh mcp: %s is %s (%s): %s\n", sel.Alias, state, rf.Code, rf.Message))
	switch state {
	case agentapi.GatewayUnauthorized, agentapi.GatewayIdentityChanged:
		g.dropHost(sel.HostID)
	case agentapi.GatewayNotFound:
		g.dropRoute(keyOf(sel))
	}
	if rf.Code == agentapi.CatalogStale {
		g.invalidate(sel)
	}
}

var codeStates = map[agentapi.Code]string{
	agentapi.AccessDenied:             agentapi.GatewayUnauthorized,
	agentapi.HostAuthenticationFailed: agentapi.GatewayUnauthorized,
	agentapi.HostIdentityMismatch:     agentapi.GatewayIdentityChanged,
	agentapi.RouteIdentityChanged:     agentapi.GatewayIdentityChanged,
	agentapi.ProtocolUnsupported:      agentapi.GatewayUnsupported,
	agentapi.ServiceNotFound:          agentapi.GatewayNotFound,
	agentapi.ServiceIncompatible:      agentapi.GatewayIncompatible,
	agentapi.HostUnreachable:          agentapi.GatewayOffline,
	agentapi.RouteUnknown:             agentapi.GatewayOffline,
}

func stateForCode(code agentapi.Code) string {
	if state, ok := codeStates[code]; ok {
		return state
	}
	return agentapi.GatewayFailed
}

// cacheKeepingCodes are the failures that say nothing about authorization,
// so an eligible cached listing stays usable as stale.
var cacheKeepingCodes = map[agentapi.Code]bool{
	agentapi.HostUnreachable: true, agentapi.ServiceUnavailable: true, agentapi.StateUnavailable: true,
	agentapi.ResourceLimit: true, agentapi.RequestOutcomeUnknown: true,
}

func (g *Gateway) setStatus(sel Selection, entry CatalogEntry, freshness string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.statuses[keyOf(sel)] = Status{Alias: sel.Alias, HostID: sel.HostID, ServiceID: sel.ServiceID, Name: entry.Name,
		State: agentapi.GatewayReady, Freshness: freshness, Revision: entry.Revision, ObservedAt: g.opts.Now()}
}

func (g *Gateway) setStatusCode(sel Selection, state string, code agentapi.Code, message string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	key := keyOf(sel)
	prev := g.statuses[key]
	freshness := ""
	switch state {
	case agentapi.GatewayStale:
		freshness = freshStale
	case agentapi.GatewayExpired:
		freshness = freshExpired
	}
	g.statuses[key] = Status{Alias: sel.Alias, HostID: sel.HostID, ServiceID: sel.ServiceID, Name: prev.Name, State: state, Freshness: freshness,
		Code: code, Message: message, Revision: prev.Revision, ObservedAt: g.opts.Now()}
}

// routeStatuses returns the state of each selected route, in profile order.
func (g *Gateway) routeStatuses(profile Profile) []Status {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]Status, 0, len(profile.Services))
	for _, sel := range profile.Services {
		st, ok := g.statuses[keyOf(sel)]
		if !ok {
			st = Status{Alias: sel.Alias, HostID: sel.HostID, ServiceID: sel.ServiceID, Name: sel.ServiceName, State: agentapi.GatewayStopped}
		}
		st.Alias = sel.Alias
		out = append(out, st)
	}
	return out
}

// catalogMemo reads each host's catalog at most once per request.
type catalogMemo struct {
	g     *Gateway
	mu    sync.Mutex
	hosts map[string]*catalogResult
}

type catalogResult struct {
	once    sync.Once
	catalog HostCatalog
	err     error
}

func newCatalogMemo(g *Gateway) *catalogMemo {
	return &catalogMemo{g: g, hosts: map[string]*catalogResult{}}
}

// lookup returns the host's catalog and the selection's entry, recording
// the caller's current scope on the host.
func (m *catalogMemo) lookup(ctx context.Context, sel Selection) (HostCatalog, CatalogEntry, error) {
	m.mu.Lock()
	r := m.hosts[sel.HostID]
	if r == nil {
		r = &catalogResult{}
		m.hosts[sel.HostID] = r
	}
	m.mu.Unlock()
	r.once.Do(func() {
		r.catalog, r.err = m.g.opts.Catalog.Services(ctx, sel.HostID)
		if r.err == nil {
			m.g.observeScope(sel.HostID, r.catalog.Scope)
		}
	})
	if r.err != nil {
		return HostCatalog{}, CatalogEntry{}, r.err
	}
	for _, e := range r.catalog.Entries {
		if e.ServiceID == sel.ServiceID {
			return r.catalog, e, catalogStateError(sel, e)
		}
	}
	return r.catalog, CatalogEntry{}, &mcpconnect.RouteFailure{Code: agentapi.ServiceNotFound, Host: sel.HostID, Service: sel.Alias,
		Message: fmt.Sprintf("service %s (%s) is no longer published on its host; run mesh mcp profile repair", sel.Alias, sel.ServiceID)}
}

// observeScope records the caller's scope on a host. A changed scope means
// the grant was replaced, so every listing of the old scope is retired.
func (g *Gateway) observeScope(hostID, scope string) {
	g.mu.Lock()
	previous, known := g.hostScope[hostID]
	g.mu.Unlock()
	if known && previous != scope {
		g.dropHost(hostID)
	}
	g.mu.Lock()
	g.hostScope[hostID] = scope
	g.mu.Unlock()
}

func catalogStateError(sel Selection, e CatalogEntry) error {
	switch e.State {
	case agentapi.GatewayIncompatible:
		return &mcpconnect.RouteFailure{Code: agentapi.ServiceIncompatible, Host: sel.HostID, Service: sel.Alias, Message: "service " + e.Name + " does not speak MCP 2026-07-28"}
	case agentapi.GatewayFailed:
		return &mcpconnect.RouteFailure{Code: agentapi.ServiceFailed, Host: sel.HostID, Service: sel.Alias, Message: "service " + e.Name + " failed; read mesh mcp logs"}
	default:
		return nil
	}
}

func profileDigest(profile Profile) string {
	raw, _ := json.Marshal(profile)
	return digest(string(raw))
}
