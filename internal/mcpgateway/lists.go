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
	entries, snapshot, extras := g.merge(ctx, file, profile, kind, views)
	// Allocations of hx_ names advance the file, so the cursor binds the
	// revision the listing ends at.
	revision := g.fileRevision(file)
	if hasCursor && (cursor.Revision != revision || cursor.Snapshot != snapshot || cursor.Offset > len(entries)) {
		return emitFault(emit, req, mcpwire.InvalidParams, "Pagination cursor no longer matches the listing; list again from the start")
	}
	page, next := paginate(entries, cursor.Offset)
	result := map[string]any{kind.field: page}
	if next > 0 {
		result["nextCursor"] = g.signer.sign(pageCursor{Version: 1, Profile: g.opts.Profile, Revision: revision, Method: kind.method, Snapshot: snapshot, Offset: next})
	}
	meta := map[string]any{MetaRoutes: g.routeStatuses(profile)}
	if len(extras) > 0 {
		meta[MetaUpstreams] = extras
	}
	result[keyMeta] = meta
	reply, err := g.completeReply(req, result, g.replyTTL(kind, views))
	if err != nil {
		return g.failure(agentapi.MCPMessageTooLarge, false, "", "", err)
	}
	return emit(reply, true)
}

func (g *Gateway) fileRevision(file File) uint64 {
	if current, err := g.opts.Store.Load(); err == nil {
		return current.Revision
	}
	return file.Revision
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

// collect gets every usable route's listing. Every request, a cursor page
// included, rechecks each host's catalog so authorization and service state
// are current. refresh fetches stale listings; a cursor page only reuses
// what the first page saw.
func (g *Gateway) collect(ctx context.Context, req mcpwire.Message, profile Profile, kind listKind, refresh bool) []routeView {
	routes := g.routes(profile)
	views := make([]routeView, len(routes))
	catalogs := newCatalogMemo(g)
	sem := make(chan struct{}, refreshFanOut)
	var wg sync.WaitGroup
	for i, sel := range routes {
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

// listParams digests the downstream request's unowned list parameters.
func listParams(req mcpwire.Message) string {
	fields, err := decodeObject(req.Params())
	if err != nil {
		return ""
	}
	delete(fields, "cursor")
	delete(fields, keyMeta)
	if len(fields) == 0 {
		return ""
	}
	raw, _ := encode(fields)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}

// routeListing returns one route's listing and whether it is stale. A nil
// listing means the route contributes nothing; its status says why.
func (g *Gateway) routeListing(ctx context.Context, req mcpwire.Message, sel Selection, kind listKind, catalogs *catalogMemo, refresh bool) (*Listing, bool) {
	key := listingKey{keyOf(sel), kind.method, listParams(req)}
	hc, entry, err := catalogs.lookup(ctx, sel)
	if err != nil {
		return g.fallback(sel, key, err)
	}
	now := g.opts.Now()
	if l := g.cached(key); l != nil && l.matches(hc.Scope, entry) && l.usable(now) && (l.fresh(now) || !refresh) {
		g.setStatus(sel, entry, freshCached)
		return l, false
	}
	if !refresh {
		return nil, false
	}
	lock := g.refreshLock(key)
	lock.Lock()
	defer lock.Unlock()
	if l := g.cached(key); l != nil && l.matches(hc.Scope, entry) && l.fresh(g.opts.Now()) {
		return l, false
	}
	gen := g.fence(sel.HostID)
	expect := &Expect{Revision: entry.Revision, Account: entry.Account}
	disc, err := g.discovery(ctx, req, sel, hc, entry)
	if err != nil {
		return g.fallback(sel, key, err)
	}
	if member(disc, kind.capability()) == nil {
		g.setStatus(sel, entry, freshLive)
		return nil, false
	}
	l, err := g.fetchListing(ctx, req, sel, kind, expect)
	if err != nil {
		return g.fallback(sel, key, err)
	}
	l.Params, l.Scope, l.ListingRevision = key.params, hc.Scope, entry.ListingRevision
	if !g.store(key, l, gen) {
		return nil, false
	}
	g.setStatus(sel, entry, freshLive)
	return l, false
}

// fallback applies section 7.3 after a lookup or refresh failed: an
// unreachable host keeps a listing of the caller's last scope for 24 hours,
// marked stale, then expired; an authorization failure or pin change
// discards protected listings immediately.
func (g *Gateway) fallback(sel Selection, key listingKey, err error) (*Listing, bool) {
	var rf *mcpconnect.RouteFailure
	if !errors.As(err, &rf) {
		rf = g.failure(agentapi.ServiceFailed, false, sel.HostID, sel.Alias, err)
	}
	g.observeFailure(sel, rf)
	if !cacheKeepingCodes[rf.Code] {
		return nil, false
	}
	l := g.cached(key)
	g.mu.Lock()
	scope := g.hostScope[sel.HostID]
	g.mu.Unlock()
	if l == nil || scope == "" || l.Scope != scope {
		return nil, false
	}
	if !l.usable(g.opts.Now()) {
		g.setStatusCode(sel, agentapi.GatewayExpired, rf.Code, "the cached listing is older than 24 hours and the host is unreachable")
		return nil, false
	}
	g.setStatusCode(sel, agentapi.GatewayStale, rf.Code, rf.Message)
	return l, true
}

var listRequestCounter atomic.Uint64

// listRequest builds an upstream request that carries the downstream
// request's parameters and metadata, minus the gateway's own cursor and
// progress token.
func listRequest(downstream mcpwire.Message, method, cursor string) (mcpwire.Message, error) {
	params, err := decodeObject(downstream.Params())
	if err != nil {
		params = object{}
	}
	delete(params, "cursor")
	meta, err := decodeObject(params[keyMeta])
	if err != nil {
		meta = object{}
	}
	delete(meta, "progressToken")
	meta[mcpwire.VersionKey] = quoted(mcpwire.Version)
	if meta[mcpwire.ClientCapsKey] == nil {
		meta[mcpwire.ClientCapsKey] = json.RawMessage(`{}`)
	}
	if params[keyMeta], err = encode(meta); err != nil {
		return mcpwire.Message{}, err
	}
	if cursor != "" {
		params["cursor"] = quoted(cursor)
	}
	if method == mcpwire.MethodDiscover {
		params = object{keyMeta: params[keyMeta]}
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

// exchange sends one gateway-originated request, resolved against expect,
// and returns its terminal result object. Notifications for it are dropped.
func (g *Gateway) exchange(ctx context.Context, sel Selection, expect *Expect, req mcpwire.Message) (json.RawMessage, error) {
	var terminal []byte
	err := g.opts.Upstream(sel, expect).Forward(ctx, req, budgetOf(ctx), func(raw []byte, last bool) error {
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

// fetchListing reads every page of one upstream listing within its bounds,
// charging the pages to the request's byte budget while it assembles them.
// Duplicate names in one listing make it malformed.
func (g *Gateway) fetchListing(ctx context.Context, downstream mcpwire.Message, sel Selection, kind listKind, expect *Expect) (*Listing, error) {
	l := &Listing{HostID: sel.HostID, ServiceID: sel.ServiceID, Instance: sel.Instance, Method: kind.method, Revision: expect.Revision,
		Account: expect.Account, FetchedAt: g.opts.Now(), TTL: maxTTL}
	page := listingPage{seen: map[string]bool{}, budget: budgetOf(ctx)}
	defer page.release()
	for range maxListingPages {
		req, err := listRequest(downstream, kind.method, page.cursor)
		if err != nil {
			return nil, err
		}
		result, err := g.exchange(ctx, sel, expect, req)
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
			l.Digest, l.Bytes = digestItems(l.Items), page.size
			return l, nil
		}
	}
	return nil, g.failure(agentapi.ResourceLimit, false, sel.HostID, sel.Alias, fmt.Errorf("%s has more than %d pages", kind.method, maxListingPages))
}

var errListingTooLarge = errors.New("listing exceeds its byte limit")

// listingPage accumulates one upstream listing across its pages.
type listingPage struct {
	seen     map[string]bool
	size     int
	reserved int
	cursor   string
	budget   interface {
		ReserveBytes(int) bool
		ReleaseBytes(int)
	}
}

func (p *listingPage) release() {
	if p.budget != nil && p.reserved > 0 {
		p.budget.ReleaseBytes(p.reserved)
	}
}

// ownedPageFields are the page fields the gateway itself produces.
var ownedPageFields = []string{"resultType", "ttlMs", "cacheScope", "nextCursor"}

func (p *listingPage) add(l *Listing, kind listKind, result json.RawMessage) error {
	items, err := decodeArray(member(result, kind.field))
	if err != nil {
		return fmt.Errorf("%s: %w", kind.method, err)
	}
	pageBytes := 0
	for _, item := range items {
		id, _ := stringMember(item, kind.idField)
		if p.seen[id] {
			return fmt.Errorf("%s lists %q twice; the listing is malformed", kind.method, id)
		}
		p.seen[id] = true
		pageBytes += len(item)
	}
	if p.size+pageBytes > maxListingBytes || (p.budget != nil && !p.budget.ReserveBytes(pageBytes)) {
		return fmt.Errorf("%s: %w (%d bytes)", kind.method, errListingTooLarge, maxListingBytes)
	}
	p.size += pageBytes
	if p.budget != nil {
		p.reserved += pageBytes
	}
	if l.Extras == nil {
		l.Extras = pageExtras(result, kind.field)
	}
	l.Items = append(l.Items, items...)
	p.cursor, _ = stringMember(result, "nextCursor")
	return nil
}

// pageExtras keeps the upstream page's own fields and metadata, which the
// merged result cannot carry inline.
func pageExtras(result json.RawMessage, field string) json.RawMessage {
	fields, err := decodeObject(result)
	if err != nil {
		return nil
	}
	delete(fields, field)
	for _, owned := range ownedPageFields {
		delete(fields, owned)
	}
	if meta, err := decodeObject(fields[keyMeta]); err == nil {
		delete(meta, mcpwire.ServerInfoKey)
		fields[keyMeta], _ = encode(meta)
		if len(meta) == 0 {
			delete(fields, keyMeta)
		}
	}
	if len(fields) == 0 {
		return nil
	}
	raw, _ := encode(fields)
	return raw
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

func digestItems(items []json.RawMessage) string {
	h := sha256.New()
	for _, item := range items {
		h.Write(item)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// upstreamExtras is one route's page-level fields in a merged result.
type upstreamExtras struct {
	Alias     string          `json:"alias"`
	HostID    string          `json:"hostId"`
	ServiceID string          `json:"serviceId"`
	Fields    json.RawMessage `json:"fields"`
}

// merge lists native entries first, then each selected service in profile
// order with its entries in upstream order. The snapshot digest names
// exactly this merged listing and the state each part was fetched under.
func (g *Gateway) merge(ctx context.Context, file File, profile Profile, kind listKind, views []routeView) ([]json.RawMessage, string, []upstreamExtras) {
	var entries []json.RawMessage
	var extras []upstreamExtras
	snapshot := sha256.New()
	_, _ = fmt.Fprintf(snapshot, "%s\x00", kind.method)
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
		if v.listing.Extras != nil {
			extras = append(extras, upstreamExtras{Alias: v.sel.Alias, HostID: v.sel.HostID, ServiceID: v.sel.ServiceID, Fields: v.listing.Extras})
		}
		l := v.listing
		_, _ = fmt.Fprintf(snapshot, "%s\x00%s\x00%s\x00%s\x00%d\x00%s\x00%s\x00%s\x00%t\x00", v.sel.HostID, v.sel.ServiceID, v.sel.Instance,
			l.Scope, l.Revision, l.Account, l.ListingRevision, l.Digest, v.stale)
	}
	if entries == nil {
		entries = []json.RawMessage{}
	}
	return entries, hex.EncodeToString(snapshot.Sum(nil))[:32], extras
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
// passes through. An entry whose URI is not a valid absolute URI or
// template is left out.
func (g *Gateway) rewriteEntries(ctx context.Context, file File, kind listKind, v routeView) ([]json.RawMessage, error) {
	meta, err := encode(routeMeta{HostID: v.sel.HostID, ServiceID: v.sel.ServiceID, Alias: v.sel.Alias, Revision: v.listing.Revision, Stale: v.stale})
	if err != nil {
		return nil, err
	}
	out := make([]json.RawMessage, 0, len(v.listing.Items))
	for _, item := range v.listing.Items {
		original, _ := stringMember(item, kind.idField)
		if !g.exposable(kind, v.sel, original) {
			continue
		}
		exposed, err := g.expose(ctx, file, v.sel, kind, original)
		if err != nil {
			return nil, err
		}
		if item, err = setMember(item, []string{kind.idField}, quoted(exposed)); err != nil {
			return nil, err
		}
		if item, err = setMember(item, []string{keyMeta, MetaRoute}, meta); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, nil
}

func (g *Gateway) exposable(kind listKind, sel Selection, original string) bool {
	switch kind.method {
	case mcpwire.MethodListResources:
		return validResourceURI(original, false)
	case mcpwire.MethodListResourceTemplates:
		return validResourceURI(original, true)
	default:
		return sel.Allows(original)
	}
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
