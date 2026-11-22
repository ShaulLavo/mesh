package mcpgateway

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/mcpconnect"
	"github.com/shaul/mesh/internal/mcpwire"
)

// Section 7.4 bounds for one downstream listen.
const (
	maxPendingURIs  = 256
	maxRefreshEvery = 5 * time.Minute
	// authorizationProbe bounds the catalog read that classifies a lost
	// upstream listen.
	authorizationProbe = 3 * time.Second
	ackMethod          = "notifications/subscriptions/acknowledged"
)

var listChangedMethods = map[string]string{
	flagTools:     "notifications/tools/list_changed",
	flagPrompts:   "notifications/prompts/list_changed",
	flagResources: "notifications/resources/list_changed",
}

// filterFlags are the list-change filters in a stable order.
var filterFlags = []string{flagTools, flagPrompts, flagResources}

type filter struct {
	flags map[string]bool
	uris  []string
}

func parseListenFilter(raw json.RawMessage) (filter, error) {
	f := filter{flags: map[string]bool{}}
	fields, err := decodeObject(raw)
	if err != nil {
		return f, fmt.Errorf("subscriptions/listen needs a notifications object: %w", err)
	}
	for _, flag := range filterFlags {
		f.flags[flag] = string(fields[flag]) == jsonTrue
	}
	if fields["resourceSubscriptions"] != nil {
		if err := json.Unmarshal(fields["resourceSubscriptions"], &f.uris); err != nil {
			return f, fmt.Errorf("resourceSubscriptions must be a list of URIs: %w", err)
		}
	}
	return f, nil
}

func (f filter) encode() map[string]any {
	out := map[string]any{}
	for _, flag := range filterFlags {
		if f.flags[flag] {
			out[flag] = true
		}
	}
	if f.uris != nil {
		out["resourceSubscriptions"] = f.uris
	}
	return out
}

// upstreamListen is one route's part of a downstream listen.
type upstreamListen struct {
	sel     Selection
	caps    json.RawMessage
	request filter
	uris    map[string]string // original upstream URI -> downstream URI
	cancel  context.CancelFunc
	acked   chan filter
	// ack is the upstream's acknowledged filter once collectAcks accepted it
	// within the setup bound. Guarded by fanIn.mu.
	ack *filter
}

// fanIn delivers one downstream listen from its upstream parts, the profile
// watcher and bounded listing refresh. Events are coalesced so a slow client
// never stalls an upstream reader.
type fanIn struct {
	g       *Gateway
	req     mcpwire.Message
	emit    Emit
	backed  filter
	signal  chan struct{}
	mu      sync.Mutex
	kinds   map[string]bool
	updates map[string]json.RawMessage
	order   []string
	ended   error
	done    bool
}

func (g *Gateway) listen(ctx context.Context, req mcpwire.Message, profile Profile, emit Emit) error {
	requested, err := parseListenFilter(member(req.Params(), "notifications"))
	if err != nil {
		return emitFault(emit, req, mcpwire.InvalidParams, err.Error())
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	f := &fanIn{g: g, req: req, emit: emit, signal: make(chan struct{}, 1), kinds: map[string]bool{}, updates: map[string]json.RawMessage{}}
	parts := g.planListen(ctx, req, profile, requested)
	for _, part := range parts {
		partCtx, partCancel := context.WithCancel(ctx)
		part.cancel = partCancel
		if !part.wants() {
			close(part.acked)
			continue
		}
		go f.runUpstream(partCtx, part)
	}
	f.backed = f.collectAcks(ctx, requested, parts)
	if err := f.send(ackMethod, map[string]any{"notifications": f.backed.encode()}); err != nil {
		return err
	}
	polled := f.pollTargets(parts)
	return f.loop(ctx, profile, parts, polled)
}

// planListen splits the request into per-route upstream listens. A route
// gets the list-change filters its upstream can deliver and the resource
// URIs that belong to it.
func (g *Gateway) planListen(ctx context.Context, req mcpwire.Message, profile Profile, requested filter) []*upstreamListen {
	catalogs := newCatalogMemo(g.opts.Catalog)
	parts := make([]*upstreamListen, 0, len(profile.Services))
	for _, sel := range profile.Services {
		part := &upstreamListen{sel: sel, request: filter{flags: map[string]bool{}}, uris: map[string]string{}, acked: make(chan filter, 1)}
		part.caps = g.routeCapabilities(ctx, req, sel, catalogs)
		for _, flag := range filterFlags {
			if requested.flags[flag] && string(member(part.caps, capabilityForFlag(flag), keyListChanged)) == jsonTrue {
				part.request.flags[flag] = true
			}
		}
		for _, uri := range requested.uris {
			hostID, serviceID, original, err := DecodeURI(uri, false)
			if err == nil && hostID == sel.HostID && serviceID == sel.ServiceID {
				part.uris[original] = uri
				part.request.uris = append(part.request.uris, original)
			}
		}
		parts = append(parts, part)
	}
	return parts
}

func capabilityForFlag(flag string) string {
	switch flag {
	case flagTools:
		return keyTools
	case flagPrompts:
		return keyPrompts
	default:
		return keyResources
	}
}

func (p *upstreamListen) wants() bool {
	return len(p.request.uris) > 0 || slices.ContainsFunc(filterFlags, func(flag string) bool { return p.request.flags[flag] })
}

func (f *fanIn) acknowledged(p *upstreamListen, flag string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return p.ack != nil && p.ack.flags[flag]
}

func (f *fanIn) acknowledgedURI(p *upstreamListen, uri string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return p.ack != nil && slices.Contains(p.ack.uris, uri)
}

func (f *fanIn) accepted(p *upstreamListen) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return p.ack != nil
}

// collectAcks waits within the setup bound. List-change filters are always
// backed: the gateway watches its profile and refreshes listings whose
// upstream did not acknowledge. Resource URIs are backed only when their
// upstream accepted them.
func (f *fanIn) collectAcks(ctx context.Context, requested filter, parts []*upstreamListen) filter {
	backed := filter{flags: map[string]bool{}}
	for _, flag := range filterFlags {
		backed.flags[flag] = requested.flags[flag]
	}
	if requested.uris != nil {
		backed.uris = []string{}
	}
	setup, cancel := context.WithTimeout(ctx, f.g.opts.ListenSetup)
	defer cancel()
	for _, part := range parts {
		var ack filter
		ok := false
		select {
		case ack, ok = <-part.acked:
		case <-setup.Done():
		}
		if !ok {
			part.cancel()
			continue
		}
		f.mu.Lock()
		part.ack = &ack
		f.mu.Unlock()
		for _, original := range ack.uris {
			if downstream, ok := part.uris[original]; ok {
				backed.uris = append(backed.uris, downstream)
			}
		}
	}
	return backed
}

// runUpstream holds one upstream listen and feeds its events to the fan-in.
func (f *fanIn) runUpstream(ctx context.Context, part *upstreamListen) {
	msg, err := listenRequest(f.req, part.request)
	if err != nil {
		f.end(err)
		return
	}
	acked := false
	defer func() {
		if !acked {
			close(part.acked)
		}
	}()
	err = f.g.upstream(part.sel).Forward(ctx, msg, func(raw []byte, terminal bool) error {
		if terminal {
			return f.upstreamEnded(part, raw)
		}
		if !acked {
			acked = true
			ack, _ := parseListenFilter(member(raw, keyParams, "notifications"))
			part.acked <- ack
			return nil
		}
		return f.queue(part, raw)
	})
	if err == nil || ctx.Err() != nil {
		return
	}
	if !acked {
		// A listen that never started only narrows the acknowledgment.
		_ = f.g.routeError(part.sel, err)
		return
	}
	f.end(f.g.routeError(part.sel, f.g.explainLoss(ctx, part.sel, err)))
}

// explainLoss tells a retired grant from a lost link. A host retires a
// revoked grant's streams by closing them, which reads as an interruption;
// one passive catalog read says whether authorization is what ended it.
func (g *Gateway) explainLoss(ctx context.Context, sel Selection, err error) error {
	var rf *mcpconnect.RouteFailure
	if !errors.As(err, &rf) || (rf.Code != agentapi.RequestOutcomeUnknown && rf.Code != agentapi.HostUnreachable) {
		return err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), authorizationProbe)
	defer cancel()
	_, probeErr := g.opts.Catalog.Services(ctx, sel.HostID)
	var denied *mcpconnect.RouteFailure
	if errors.As(probeErr, &denied) && (denied.Code == agentapi.AccessDenied || denied.Code == agentapi.HostAuthenticationFailed) {
		return &mcpconnect.RouteFailure{Code: agentapi.AccessDenied, Host: sel.HostID, Service: sel.Alias,
			Message: "authorization for " + sel.Alias + " was retired", Err: err}
	}
	return err
}

func listenRequest(downstream mcpwire.Message, request filter) (mcpwire.Message, error) {
	notifications, err := encode(request.encode())
	if err != nil {
		return mcpwire.Message{}, err
	}
	raw, err := setMember(downstream.Raw(), []string{keyParams, "notifications"}, notifications)
	if err != nil {
		return mcpwire.Message{}, err
	}
	msg, fault := mcpwire.Parse(raw)
	if fault != nil {
		return mcpwire.Message{}, fault
	}
	return msg, nil
}

// upstreamEnded turns an upstream's end into the downstream end: a graceful
// completion ends the listen with one complete result; an error ends it
// with that error.
func (f *fanIn) upstreamEnded(part *upstreamListen, raw []byte) error {
	if errObj := member(raw, "error"); errObj != nil {
		f.end(f.g.routeError(part.sel, upstreamError(part.sel, mcpwire.MethodListen, errObj)))
		return nil
	}
	f.end(nil)
	return nil
}

// queue coalesces one upstream event. List changes merge by kind; resource
// updates keep the latest per URI.
func (f *fanIn) queue(part *upstreamListen, raw []byte) error {
	if !f.accepted(part) {
		// Not part of the downstream acknowledgment, so never delivered.
		return nil
	}
	method, _ := stringMember(raw, "method")
	for flag, kind := range listChangedMethods {
		if method == kind && f.acknowledged(part, flag) {
			f.g.invalidate(part.sel, listMethodForFlag(flag))
			f.mark(kind)
			return nil
		}
	}
	if method != "notifications/resources/updated" {
		return nil
	}
	original, _ := stringMember(raw, keyParams, keyURI)
	downstream, ok := part.uris[original]
	if !ok || !f.acknowledgedURI(part, original) {
		return nil
	}
	rewritten, err := setMember(raw, []string{keyParams, keyURI}, quoted(downstream))
	if err != nil {
		return err
	}
	return f.update(downstream, rewritten)
}

func listMethodForFlag(flag string) string {
	switch flag {
	case flagTools:
		return mcpwire.MethodListTools
	case flagPrompts:
		return mcpwire.MethodListPrompts
	default:
		return mcpwire.MethodListResources
	}
}

func (f *fanIn) mark(kind string) {
	f.mu.Lock()
	if !f.done {
		f.kinds[kind] = true
	}
	f.mu.Unlock()
	f.wake()
}

func (f *fanIn) update(uri string, raw []byte) error {
	f.mu.Lock()
	defer f.wakeAfter()
	defer f.mu.Unlock()
	if f.done {
		return nil
	}
	if _, ok := f.updates[uri]; !ok {
		if len(f.order) >= maxPendingURIs {
			f.endLocked(&mcpconnect.RouteFailure{Code: agentapi.ResourceLimit, Retryable: true, Message: "too many pending resource updates for a slow client"})
			return nil
		}
		f.order = append(f.order, uri)
	}
	f.updates[uri] = raw
	return nil
}

func (f *fanIn) wakeAfter() { f.wake() }

func (f *fanIn) wake() {
	select {
	case f.signal <- struct{}{}:
	default:
	}
}

// end records the first reason the downstream listen ends. A nil reason is
// a graceful completion.
func (f *fanIn) end(reason error) {
	f.mu.Lock()
	f.endLocked(reason)
	f.mu.Unlock()
	f.wake()
}

func (f *fanIn) endLocked(reason error) {
	if f.done {
		return
	}
	f.done, f.ended = true, reason
	// Revocation fences delivery: nothing queued before it goes out.
	var rf *mcpconnect.RouteFailure
	if errors.As(reason, &rf) && rf.Code == agentapi.AccessDenied {
		f.kinds, f.updates, f.order = map[string]bool{}, map[string]json.RawMessage{}, nil
	}
}

type pollTarget struct {
	sel    Selection
	method string
	kind   string
	every  time.Duration
	next   time.Time
	digest string
}

// pollTargets lists the routes whose list changes the gateway must find by
// refreshing, because no upstream listener acknowledged them.
func (f *fanIn) pollTargets(parts []*upstreamListen) []*pollTarget {
	var targets []*pollTarget
	now := f.g.opts.Now()
	for _, part := range parts {
		sel := part.sel
		for _, flag := range filterFlags {
			if !f.backed.flags[flag] || f.acknowledged(part, flag) || member(part.caps, capabilityForFlag(flag)) == nil {
				continue
			}
			method := listMethodForFlag(flag)
			t := &pollTarget{sel: sel, method: method, kind: listChangedMethods[flag], every: f.g.opts.RefreshEvery}
			t.next = now.Add(jitter(t.every))
			if l := f.g.cached(listingKey{routeKey{sel.HostID, sel.ServiceID}, method}); l != nil {
				t.digest = l.Digest
			}
			targets = append(targets, t)
		}
	}
	return targets
}

// jitter spreads refreshes over half to one and a half intervals so many
// frontends do not poll a host in step.
func jitter(d time.Duration) time.Duration {
	var b [4]byte
	_, _ = rand.Read(b[:])
	// A 32-bit fraction of d keeps the arithmetic within int64.
	fraction := time.Duration(binary.LittleEndian.Uint32(b[:]))
	return d/2 + d/(1<<32)*fraction
}

// loop delivers coalesced events until the listen ends, the client cancels,
// or the profile drops a route this listen depends on.
func (f *fanIn) loop(ctx context.Context, profile Profile, parts []*upstreamListen, polled []*pollTarget) error {
	tick := time.NewTicker(f.g.opts.ProfileEvery)
	defer tick.Stop()
	defer func() {
		for _, p := range parts {
			p.cancel()
		}
	}()
	digest := profileDigest(profile)
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("listen cancelled: %w", ctx.Err())
		case <-f.signal:
		case <-tick.C:
			digest = f.checkProfile(digest, parts)
			f.poll(ctx, polled)
		}
		finished, err := f.flush()
		if finished || err != nil {
			return err
		}
	}
}

// checkProfile notices local selection changes. Removing a route this listen
// holds ends the listen; any other change is a list change for the client.
func (f *fanIn) checkProfile(digest string, parts []*upstreamListen) string {
	_, profile, err := f.g.current()
	if err != nil {
		return digest
	}
	next := profileDigest(profile)
	if next == digest {
		return digest
	}
	for _, p := range parts {
		if _, still := profile.Lookup(p.sel.HostID, p.sel.ServiceID); !still && f.accepted(p) {
			f.end(nil)
			return next
		}
	}
	for _, flag := range filterFlags {
		if f.backed.flags[flag] {
			f.mark(listChangedMethods[flag])
		}
	}
	return next
}

// poll refreshes due listings with backoff and reports changed ones.
func (f *fanIn) poll(ctx context.Context, targets []*pollTarget) {
	now := f.g.opts.Now()
	for _, t := range targets {
		if now.Before(t.next) {
			continue
		}
		l, err := f.g.fetchListing(ctx, f.req, t.sel, kindFor(t.method), 0)
		if err != nil {
			t.every = min(t.every*2, maxRefreshEvery)
			t.next = now.Add(jitter(t.every))
			continue
		}
		t.every = f.g.opts.RefreshEvery
		t.next = now.Add(jitter(t.every))
		if t.digest != "" && l.Digest != t.digest {
			f.g.invalidate(t.sel, t.method)
			f.mark(t.kind)
		}
		t.digest = l.Digest
	}
}

// flush writes pending events in a stable order and, once the listen has
// ended, its single terminal reply.
func (f *fanIn) flush() (bool, error) {
	f.mu.Lock()
	kinds, updates, order, done, reason := f.kinds, f.updates, f.order, f.done, f.ended
	f.kinds, f.updates, f.order = map[string]bool{}, map[string]json.RawMessage{}, nil
	f.mu.Unlock()
	for _, flag := range filterFlags {
		if kinds[listChangedMethods[flag]] {
			if err := f.send(listChangedMethods[flag], map[string]any{}); err != nil {
				return true, err
			}
		}
	}
	for _, uri := range order {
		if err := f.emit(f.tag(updates[uri]), false); err != nil {
			return true, err
		}
	}
	if !done {
		return false, nil
	}
	if reason != nil {
		return true, reason
	}
	reply, err := f.g.completeReply(f.req, map[string]any{}, 0)
	if err != nil {
		return true, err
	}
	return true, f.emit(reply, true)
}

func (f *fanIn) tag(raw []byte) []byte {
	id, _ := f.req.ID().MarshalJSON()
	tagged, err := setMember(raw, []string{keyParams, keyMeta, mcpwire.SubscriptionID}, id)
	if err != nil {
		return raw
	}
	return tagged
}

func (f *fanIn) send(method string, params map[string]any) error {
	raw, err := encode(map[string]any{"jsonrpc": "2.0", "method": method, keyParams: params})
	if err != nil {
		return err
	}
	return f.emit(f.tag(raw), false)
}

// invalidate marks a route's cached listing as needing revalidation while
// keeping it usable as a stale fallback.
func (g *Gateway) invalidate(sel Selection, method string) {
	key := listingKey{routeKey{sel.HostID, sel.ServiceID}, method}
	g.cached(key)
	g.mu.Lock()
	defer g.mu.Unlock()
	if l := g.listings[key]; l != nil {
		expired := *l
		expired.TTL = 0
		g.listings[key] = &expired
	}
}
