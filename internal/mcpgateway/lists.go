package mcpgateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/mcpconnect"
	"github.com/shaul/mesh/internal/mcpwire"
)

// Section 7.3 bounds.
const (
	PageSize          = 200
	maxTTL            = 5 * time.Minute
	staleFor          = 24 * time.Hour
	nativeTTL         = 5 * time.Minute
	maxListingPages   = 64
	maxListingBytes   = 8 << 20
	pageByteBudget    = 3 << 20
	refreshFanOut     = 8
	listRequestPrefix = "mesh-gw-"
)

// Listing is one service's complete upstream listing for one method, as the
// upstream sent its entries. Names and URIs are rewritten when merged.
type Listing struct {
	HostID    string            `json:"hostId"`
	ServiceID string            `json:"serviceId"`
	Method    string            `json:"method"`
	Revision  int64             `json:"revision"`
	Items     []json.RawMessage `json:"items"`
	FetchedAt time.Time         `json:"fetchedAt"`
	TTL       time.Duration     `json:"ttl"`
	Digest    string            `json:"digest"`
}

func (l *Listing) fresh(now time.Time) bool { return now.Before(l.FetchedAt.Add(l.TTL)) }
func (l *Listing) usable(now time.Time) bool {
	return now.Before(l.FetchedAt.Add(staleFor))
}

// Status is the gateway's view of one profile entry.
type Status struct {
	Alias      string        `json:"alias"`
	HostID     string        `json:"hostId"`
	ServiceID  string        `json:"serviceId"`
	Name       string        `json:"name,omitempty"`
	State      string        `json:"state"`
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

// routeView is one route's contribution to a merged listing.
type routeView struct {
	sel     Selection
	listing *Listing
	stale   bool
}

func (g *Gateway) list(ctx context.Context, req mcpwire.Message, file File, profile Profile, emit Emit) error {
	kind := kindFor(req.Method())
	cursorToken, hasCursor := stringMember(req.Params(), "cursor")
	var cursor pageCursor
	if hasCursor {
		var err error
		if cursor, err = g.signer.verify(cursorToken); err != nil || cursor.Method != kind.method || cursor.Profile != g.opts.Profile {
			return emitFault(emit, req, mcpwire.InvalidParams, "Invalid pagination cursor; list again from the start")
		}
	}
	views := g.collect(ctx, req, profile, kind, !hasCursor)
	entries, snapshot := g.merge(ctx, file, profile, kind, views)
	selection := profileDigest(profile)
	if hasCursor && (cursor.Selection != selection || cursor.Snapshot != snapshot || cursor.Offset > len(entries)) {
		return emitFault(emit, req, mcpwire.InvalidParams, "Pagination cursor no longer matches the listing; list again from the start")
	}
	page, next := paginate(entries, cursor.Offset)
	result := map[string]any{kind.field: page}
	if next > 0 {
		result["nextCursor"] = g.signer.sign(pageCursor{Version: 1, Profile: g.opts.Profile, Selection: selection, Method: kind.method, Snapshot: snapshot, Offset: next})
	}
	reply, err := g.completeReply(req, result, g.replyTTL(kind, views))
	if err != nil {
		return g.failure(agentapi.MCPMessageTooLarge, false, "", "", err)
	}
	return emit(reply, true)
}

func paginate(entries []json.RawMessage, offset int) ([]json.RawMessage, int) {
	end, size := offset, 0
	for end < len(entries) && end-offset < PageSize && size+len(entries[end]) <= pageByteBudget {
		size += len(entries[end])
		end++
	}
	if end == offset && end < len(entries) {
		end++
	}
	page := entries[offset:end]
	if page == nil {
		page = []json.RawMessage{}
	}
	if end < len(entries) {
		return page, end
	}
	return page, 0
}

// replyTTL advertises the shortest remaining freshness, or zero when any
// entry is stale.
func (g *Gateway) replyTTL(kind listKind, views []routeView) time.Duration {
	now := g.opts.Now()
	ttl := maxTTL
	if kind.method == mcpwire.MethodListTools {
		ttl = nativeTTL
	}
	for _, v := range views {
		if v.listing == nil {
			continue
		}
		if v.stale {
			return 0
		}
		ttl = min(ttl, max(v.listing.FetchedAt.Add(v.listing.TTL).Sub(now), 0))
	}
	return ttl.Truncate(time.Millisecond)
}

// collect gets every selected route's listing. refresh revalidates expired
// listings; a cursor page uses only what the first page saw.
func (g *Gateway) collect(ctx context.Context, req mcpwire.Message, profile Profile, kind listKind, refresh bool) []routeView {
	views := make([]routeView, len(profile.Services))
	catalogs := newCatalogMemo(g.opts.Catalog)
	sem := make(chan struct{}, refreshFanOut)
	var wg sync.WaitGroup
	for i, sel := range profile.Services {
		views[i].sel = sel
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			views[i].listing, views[i].stale = g.routeListing(ctx, req, sel, kind, catalogs, refresh)
		})
	}
	wg.Wait()
	return views
}

func (g *Gateway) cached(key listingKey) *Listing {
	g.mu.Lock()
	l := g.listings[key]
	g.mu.Unlock()
	if l != nil {
		return l
	}
	if stored, ok := g.opts.Cache.Load(key.route.host, key.route.service, key.method); ok {
		g.mu.Lock()
		g.listings[key] = stored
		g.mu.Unlock()
		return stored
	}
	return nil
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

// routeListing returns one route's listing and whether it is stale. A nil
// listing means the route contributes nothing; its status says why.
func (g *Gateway) routeListing(ctx context.Context, req mcpwire.Message, sel Selection, kind listKind, catalogs *catalogMemo, refresh bool) (*Listing, bool) {
	key := listingKey{routeKey{sel.HostID, sel.ServiceID}, kind.method}
	now := g.opts.Now()
	if l := g.cached(key); l != nil && (l.fresh(now) || !refresh) {
		return l, !l.fresh(now) && g.statusState(sel) != agentapi.GatewayReady
	}
	if !refresh {
		return nil, false
	}
	lock := g.refreshLock(key)
	lock.Lock()
	defer lock.Unlock()
	if l := g.cached(key); l != nil && l.fresh(g.opts.Now()) {
		return l, false
	}
	entry, err := catalogs.lookup(ctx, sel)
	if err != nil {
		return g.fallback(sel, key, err)
	}
	disc, err := g.discovery(ctx, req, sel, entry)
	if err != nil {
		return g.fallback(sel, key, err)
	}
	if member(disc, kind.capability()) == nil {
		g.setStatus(sel, entry, agentapi.GatewayReady, "", "")
		return nil, false
	}
	l, err := g.fetchListing(ctx, req, sel, kind, entry.Revision)
	if err != nil {
		return g.fallback(sel, key, err)
	}
	g.store(key, l)
	g.setStatus(sel, entry, agentapi.GatewayReady, "", "")
	return l, false
}

func (g *Gateway) store(key listingKey, l *Listing) {
	g.mu.Lock()
	g.listings[key] = l
	g.mu.Unlock()
	g.opts.Cache.Save(l)
}

// fallback applies section 7.3 after a refresh failed: an unreachable host
// keeps an eligible listing for 24 hours, marked stale; an authorization
// failure or pin change discards protected listings immediately.
func (g *Gateway) fallback(sel Selection, key listingKey, err error) (*Listing, bool) {
	var rf *mcpconnect.RouteFailure
	if !errors.As(err, &rf) {
		rf = g.failure(agentapi.ServiceFailed, false, sel.HostID, sel.Alias, err)
	}
	g.observeFailure(sel, rf)
	if !keepsCache(rf.Code) {
		return nil, false
	}
	l := g.cached(key)
	if l == nil {
		return nil, false
	}
	if !l.usable(g.opts.Now()) {
		g.setStatusCode(sel, agentapi.GatewayExpired, rf.Code, "cached listing is older than 24 hours")
		return nil, false
	}
	g.setStatusCode(sel, agentapi.GatewayStale, rf.Code, rf.Message)
	return l, true
}

// cacheKeepingCodes are the failures that say nothing about authorization,
// so an eligible cached listing stays usable as stale.
var cacheKeepingCodes = map[agentapi.Code]bool{
	agentapi.HostUnreachable: true, agentapi.ServiceUnavailable: true, agentapi.StateUnavailable: true,
	agentapi.ResourceLimit: true, agentapi.RequestOutcomeUnknown: true,
}

func keepsCache(code agentapi.Code) bool { return cacheKeepingCodes[code] }

// observeFailure records what a failure says about a route and discards
// listings that authorization no longer covers.
func (g *Gateway) observeFailure(sel Selection, rf *mcpconnect.RouteFailure) {
	state := stateForCode(rf.Code)
	g.setStatusCode(sel, state, rf.Code, rf.Message)
	if state == agentapi.GatewayUnauthorized || state == agentapi.GatewayIdentityChanged {
		g.dropHost(sel.HostID)
	}
	if state == agentapi.GatewayNotFound {
		g.dropRoute(routeKey{sel.HostID, sel.ServiceID})
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

func (g *Gateway) dropHost(hostID string) {
	g.mu.Lock()
	for key := range g.listings {
		if key.route.host == hostID {
			delete(g.listings, key)
		}
	}
	g.mu.Unlock()
	g.opts.Cache.DropHost(hostID)
}

func (g *Gateway) dropRoute(route routeKey) {
	g.mu.Lock()
	for key := range g.listings {
		if key.route == route {
			delete(g.listings, key)
		}
	}
	g.mu.Unlock()
	g.opts.Cache.DropRoute(route.host, route.service)
}

func (g *Gateway) setStatus(sel Selection, entry CatalogEntry, state, code, message string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.statuses[routeKey{sel.HostID, sel.ServiceID}] = Status{Alias: sel.Alias, HostID: sel.HostID, ServiceID: sel.ServiceID, Name: entry.Name,
		State: state, Code: agentapi.Code(code), Message: message, Revision: entry.Revision, ObservedAt: g.opts.Now()}
}

func (g *Gateway) setStatusCode(sel Selection, state string, code agentapi.Code, message string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	key := routeKey{sel.HostID, sel.ServiceID}
	prev := g.statuses[key]
	g.statuses[key] = Status{Alias: sel.Alias, HostID: sel.HostID, ServiceID: sel.ServiceID, Name: prev.Name, State: state, Code: code,
		Message: message, Revision: prev.Revision, ObservedAt: g.opts.Now()}
}

func (g *Gateway) statusState(sel Selection) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.statuses[routeKey{sel.HostID, sel.ServiceID}].State
}

// Statuses returns the gateway's view of each entry of the current profile.
func (g *Gateway) Statuses() []Status {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]Status, 0, len(g.profile.Services))
	for _, sel := range g.profile.Services {
		st, ok := g.statuses[routeKey{sel.HostID, sel.ServiceID}]
		if !ok {
			st = Status{Alias: sel.Alias, HostID: sel.HostID, ServiceID: sel.ServiceID, Name: sel.ServiceName, State: agentapi.GatewayStopped}
		}
		out = append(out, st)
	}
	return out
}

// catalogMemo reads each host's catalog at most once per merged request.
type catalogMemo struct {
	catalog Catalog
	mu      sync.Mutex
	hosts   map[string]*catalogResult
}

type catalogResult struct {
	once    sync.Once
	entries []CatalogEntry
	err     error
}

func newCatalogMemo(c Catalog) *catalogMemo {
	return &catalogMemo{catalog: c, hosts: map[string]*catalogResult{}}
}

func (m *catalogMemo) lookup(ctx context.Context, sel Selection) (CatalogEntry, error) {
	m.mu.Lock()
	r := m.hosts[sel.HostID]
	if r == nil {
		r = &catalogResult{}
		m.hosts[sel.HostID] = r
	}
	m.mu.Unlock()
	r.once.Do(func() { r.entries, r.err = m.catalog.Services(ctx, sel.HostID) })
	if r.err != nil {
		return CatalogEntry{}, r.err
	}
	for _, e := range r.entries {
		if e.ServiceID == sel.ServiceID {
			return e, catalogStateError(sel, e)
		}
	}
	return CatalogEntry{}, &mcpconnect.RouteFailure{Code: agentapi.ServiceNotFound, Host: sel.HostID, Service: sel.Alias,
		Message: fmt.Sprintf("service %s (%s) is no longer published on its host; run mesh mcp profile repair", sel.Alias, sel.ServiceID)}
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

var listRequestCounter atomic.Uint64

// listRequest builds an upstream list or discover request that carries the
// downstream client's declared version, capabilities and info.
func listRequest(downstream mcpwire.Message, method, cursor string) (mcpwire.Message, error) {
	meta := object{mcpwire.VersionKey: quoted(mcpwire.Version), mcpwire.ClientCapsKey: json.RawMessage(`{}`)}
	for _, key := range []string{mcpwire.ClientCapsKey, mcpwire.ClientInfoKey} {
		if value := member(downstream.Params(), keyMeta, key); value != nil {
			meta[key] = value
		}
	}
	params := map[string]any{keyMeta: meta}
	if cursor != "" {
		params["cursor"] = cursor
	}
	raw, err := encode(map[string]any{"jsonrpc": "2.0", "id": listRequestPrefix + strconv.FormatUint(listRequestCounter.Add(1), 10), "method": method, keyParams: params})
	if err != nil {
		return mcpwire.Message{}, err
	}
	msg, fault := mcpwire.Parse(raw)
	if fault != nil {
		return mcpwire.Message{}, fault
	}
	return msg, nil
}

// exchange sends one gateway-originated request and returns its terminal
// result object. Notifications for it are dropped.
func (g *Gateway) exchange(ctx context.Context, sel Selection, req mcpwire.Message) (json.RawMessage, error) {
	var terminal []byte
	err := g.upstream(sel).Forward(ctx, req, func(raw []byte, last bool) error {
		if last {
			terminal = append([]byte(nil), raw...)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("%s to %s: %w", req.Method(), sel.Alias, err)
	}
	if errObj := member(terminal, "error"); errObj != nil {
		return nil, upstreamError(sel, req.Method(), errObj)
	}
	result := member(terminal, "result")
	if result == nil {
		return nil, g.failure(agentapi.ServiceFailed, false, sel.HostID, sel.Alias, errors.New("upstream reply has no result"))
	}
	if kind, _ := stringMember(result, "resultType"); kind != "complete" {
		return nil, g.failure(agentapi.ServiceFailed, false, sel.HostID, sel.Alias, fmt.Errorf("upstream %s needs input; listings cannot", req.Method()))
	}
	return result, nil
}

func upstreamError(sel Selection, method string, errObj json.RawMessage) error {
	message, _ := stringMember(errObj, "message")
	meshCode, ok := stringMember(errObj, "data", "meshCode")
	code := agentapi.ServiceFailed
	retryable := false
	if ok {
		code = agentapi.Code(meshCode)
		retryable = string(member(errObj, "data", "retryable")) == jsonTrue
	}
	return &mcpconnect.RouteFailure{Code: code, Retryable: retryable, Host: sel.HostID, Service: sel.Alias,
		Message: agentapi.CleanMessage(fmt.Sprintf("%s from %s: %s", method, sel.Alias, message))}
}

// fetchListing reads every page of one upstream listing within its bounds.
// Duplicate names in one listing make it malformed.
func (g *Gateway) fetchListing(ctx context.Context, downstream mcpwire.Message, sel Selection, kind listKind, revision int64) (*Listing, error) {
	l := &Listing{HostID: sel.HostID, ServiceID: sel.ServiceID, Method: kind.method, Revision: revision, FetchedAt: g.opts.Now(), TTL: maxTTL}
	page := listingPage{seen: map[string]bool{}}
	for range maxListingPages {
		req, err := listRequest(downstream, kind.method, page.cursor)
		if err != nil {
			return nil, err
		}
		result, err := g.exchange(ctx, sel, req)
		if err != nil {
			return nil, err
		}
		if ttl, ok := ttlOf(result); ok {
			l.TTL = min(l.TTL, ttl)
		}
		if err := page.add(l, kind, result); err != nil {
			code := agentapi.ServiceFailed
			if errors.Is(err, errListingTooLarge) {
				code = agentapi.ResourceLimit
			}
			return nil, g.failure(code, false, sel.HostID, sel.Alias, err)
		}
		if page.cursor == "" {
			l.Digest = digestItems(l.Items)
			return l, nil
		}
	}
	return nil, g.failure(agentapi.ResourceLimit, false, sel.HostID, sel.Alias, fmt.Errorf("%s has more than %d pages", kind.method, maxListingPages))
}

var errListingTooLarge = errors.New("listing exceeds its byte limit")

// listingPage accumulates one upstream listing across its pages.
type listingPage struct {
	seen   map[string]bool
	size   int
	cursor string
}

func (p *listingPage) add(l *Listing, kind listKind, result json.RawMessage) error {
	items, err := decodeArray(member(result, kind.field))
	if err != nil {
		return fmt.Errorf("%s: %w", kind.method, err)
	}
	for _, item := range items {
		id, _ := stringMember(item, kind.idField)
		if p.seen[id] {
			return fmt.Errorf("%s lists %q twice; the listing is malformed", kind.method, id)
		}
		p.seen[id] = true
		p.size += len(item)
	}
	if p.size > maxListingBytes {
		return fmt.Errorf("%s: %w (%d bytes)", kind.method, errListingTooLarge, maxListingBytes)
	}
	l.Items = append(l.Items, items...)
	p.cursor, _ = stringMember(result, "nextCursor")
	return nil
}

func ttlOf(result json.RawMessage) (time.Duration, bool) {
	raw := member(result, "ttlMs")
	if raw == nil {
		return 0, false
	}
	ms, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil || ms < 0 {
		return 0, false
	}
	return time.Duration(min(ms, maxTTL.Milliseconds())) * time.Millisecond, true
}

func profileDigest(profile Profile) string {
	raw, _ := json.Marshal(profile)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:16])
}

func digestItems(items []json.RawMessage) string {
	h := sha256.New()
	for _, item := range items {
		h.Write(item)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// merge lists native entries first, then each selected service in profile
// order with its entries in upstream order. The snapshot digest names
// exactly this merged listing.
func (g *Gateway) merge(ctx context.Context, file File, profile Profile, kind listKind, views []routeView) ([]json.RawMessage, string) {
	var entries []json.RawMessage
	snapshot := sha256.New()
	_, _ = fmt.Fprintf(snapshot, "%s\x00%s\x00", kind.method, profileDigest(profile))
	if kind.method == mcpwire.MethodListTools {
		entries = g.opts.Native.Tools(profile.Native)
		_, _ = fmt.Fprintf(snapshot, "native\x00%d\x00", len(entries))
	}
	hostedTools := 0
	for _, v := range views {
		if v.listing == nil {
			continue
		}
		rewritten, err := g.rewriteEntries(ctx, file, kind, v)
		if err != nil {
			g.setStatusCode(v.sel, agentapi.GatewayFailed, agentapi.ServiceFailed, err.Error())
			continue
		}
		if kind.method == mcpwire.MethodListTools && hostedTools+len(rewritten) > MaxExposedTools {
			g.setStatusCode(v.sel, agentapi.GatewayFailed, agentapi.ResourceLimit, fmt.Sprintf("profile would expose more than %d tools", MaxExposedTools))
			continue
		}
		hostedTools += len(rewritten)
		entries = append(entries, rewritten...)
		_, _ = fmt.Fprintf(snapshot, "%s\x00%s\x00%s\x00%t\x00", v.sel.HostID, v.sel.ServiceID, v.listing.Digest, v.stale)
	}
	if entries == nil {
		entries = []json.RawMessage{}
	}
	return entries, hex.EncodeToString(snapshot.Sum(nil))[:32]
}

type routeMeta struct {
	HostID    string `json:"hostId"`
	ServiceID string `json:"serviceId"`
	Alias     string `json:"alias"`
	Revision  int64  `json:"revision"`
	Stale     bool   `json:"stale"`
}

// rewriteEntries applies the profile filters and exposes each entry under
// its gateway name or URI, with Mesh route metadata. Every other field
// passes through.
func (g *Gateway) rewriteEntries(ctx context.Context, file File, kind listKind, v routeView) ([]json.RawMessage, error) {
	meta, err := encode(routeMeta{HostID: v.sel.HostID, ServiceID: v.sel.ServiceID, Alias: v.sel.Alias, Revision: v.listing.Revision, Stale: v.stale})
	if err != nil {
		return nil, err
	}
	out := make([]json.RawMessage, 0, len(v.listing.Items))
	for _, item := range v.listing.Items {
		original, _ := stringMember(item, kind.idField)
		if kind.kind != "" && !v.sel.Allows(original) {
			continue
		}
		exposed, err := g.expose(ctx, file, v.sel, kind, original)
		if err != nil {
			return nil, err
		}
		item, err = setMember(item, []string{kind.idField}, quoted(exposed))
		if err != nil {
			return nil, err
		}
		if item, err = setMember(item, []string{keyMeta, MetaRoute}, meta); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, nil
}

func (g *Gateway) expose(ctx context.Context, file File, sel Selection, kind listKind, original string) (string, error) {
	if kind.kind == "" {
		return EncodeURI(sel.HostID, sel.ServiceID, original)
	}
	id := Identity{HostID: sel.HostID, ServiceID: sel.ServiceID, Kind: kind.kind, Name: original}
	name, fits, err := EncodedName(id)
	if err != nil || fits {
		return name, err
	}
	if existing, ok := file.OverflowName(id); ok {
		return existing, nil
	}
	return g.opts.Store.ExposedName(ctx, id)
}
