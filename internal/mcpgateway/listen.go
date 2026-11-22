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
	maxEarlyBytes   = 1 << 20
	maxRefreshEvery = 5 * time.Minute
	ackMethod       = "notifications/subscriptions/acknowledged"
	updatedMethod   = "notifications/resources/updated"
	// authorizationProbe bounds the catalog read that classifies a lost
	// upstream listen.
	authorizationProbe = 3 * time.Second
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

func (f filter) empty() bool {
	return len(f.uris) == 0 && !slices.ContainsFunc(filterFlags, func(flag string) bool { return f.flags[flag] })
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
	if fields["resourceSubscriptions"] == nil {
		return f, nil
	}
	if err := json.Unmarshal(fields["resourceSubscriptions"], &f.uris); err != nil {
		return f, fmt.Errorf("resourceSubscriptions must be a list of URIs: %w", err)
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

type partState int

const (
	partPending partState = iota
	partAccepted
	partDeclined
)

// listenPart is one route's part of a downstream listen.
type listenPart struct {
	sel   Selection
	caps  json.RawMessage
	scope string
	// request is what the upstream is asked for: the list-change filters it
	// can deliver and the resource URIs that belong to it.
	request filter
	uris    map[string]string // original upstream URI -> downstream URI
	cancel  context.CancelFunc
	acked   chan filter
	// Guarded by fanIn.mu.
	state partState
	ack   filter
	early []json.RawMessage
}

func (p *listenPart) wants() bool { return !p.request.empty() }

// fanIn delivers one downstream listen from its upstream parts, the hosts'
// listing revisions (or bounded refresh where a host cannot deliver them)
// and the profile watcher. Events are coalesced and charged to the shared
// byte budget, so a slow client never stalls an upstream reader.
type fanIn struct {
	g      *Gateway
	req    mcpwire.Message
	emit   Emit
	retire func()
	budget interface {
		ReserveBytes(int) bool
		ReleaseBytes(int)
	}
	backed filter
	parts  []*listenPart
	signal chan struct{}

	mu        sync.Mutex
	kinds     map[string]json.RawMessage
	updates   map[string]json.RawMessage
	order     []string
	charged   int
	pollHosts map[string]bool
	ended     error
	done      bool
}

func (g *Gateway) listen(ctx context.Context, req mcpwire.Message, profile Profile, emit Emit) error {
	requested, err := parseListenFilter(member(req.Params(), "notifications"))
	if err != nil {
		return emitFault(emit, req, mcpwire.InvalidParams, err.Error())
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	f := &fanIn{g: g, req: req, emit: emit, retire: retireOf(ctx), budget: budgetOf(ctx), signal: make(chan struct{}, 1),
		kinds: map[string]json.RawMessage{}, updates: map[string]json.RawMessage{}, pollHosts: map[string]bool{}}
	defer f.releaseAll()
	setup, setupCancel := context.WithTimeout(ctx, g.opts.ListenSetup)
	prepared := f.prepare(ctx, setup, g.routes(profile), requested)
	f.backed = f.collectAcks(setup, requested)
	setupCancel()
	prepared.Wait()
	if err := f.send(ackMethod, map[string]any{"notifications": f.backed.encode()}); err != nil {
		return err
	}
	f.admitEarly()
	f.watchHosts(ctx)
	go f.poll(ctx)
	return f.loop(ctx, profile)
}

// prepare resolves every route concurrently within the setup bound and
// starts each route's upstream listen as soon as that route is resolved, so
// one silent host neither holds the acknowledgment past the bound nor
// delays the others.
func (f *fanIn) prepare(ctx, setup context.Context, routes []Selection, requested filter) *sync.WaitGroup {
	catalogs := newCatalogMemo(f.g)
	f.parts = make([]*listenPart, len(routes))
	var wg sync.WaitGroup
	for i, sel := range routes {
		partCtx, partCancel := context.WithCancel(ctx)
		part := &listenPart{sel: sel, request: filter{flags: map[string]bool{}}, uris: map[string]string{}, acked: make(chan filter, 1), cancel: partCancel}
		f.parts[i] = part
		wg.Go(func() {
			f.g.planPart(setup, f.req, part, requested, catalogs)
			f.start(partCtx, part)
		})
	}
	return &wg
}

func (g *Gateway) planPart(setup context.Context, req mcpwire.Message, part *listenPart, requested filter, catalogs *catalogMemo) {
	sel := part.sel
	part.caps = g.routeCapabilities(setup, req, sel, catalogs)
	g.mu.Lock()
	part.scope = g.hostScope[sel.HostID]
	g.mu.Unlock()
	for _, flag := range filterFlags {
		part.request.flags[flag] = requested.flags[flag] && string(member(part.caps, capabilityForFlag(flag), keyListChanged)) == jsonTrue
	}
	for _, uri := range requested.uris {
		hostID, serviceID, original, err := DecodeURI(uri, false)
		if err != nil || hostID != sel.HostID || serviceID != sel.ServiceID {
			continue
		}
		part.uris[original] = uri
		part.request.uris = append(part.request.uris, original)
	}
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

// start runs a resolved part's upstream listen, unless the setup bound
// already declined it.
func (f *fanIn) start(ctx context.Context, part *listenPart) {
	f.mu.Lock()
	declined := part.state == partDeclined
	f.mu.Unlock()
	if declined || !part.wants() {
		close(part.acked)
		return
	}
	go f.runUpstream(ctx, part)
}

// collectAcks waits for the upstream acknowledgments within the same setup
// bound as discovery. List-change filters are always backed: hosts publish
// listing revisions, or the gateway refreshes, and it watches its profile.
// Resource URIs are backed only when their upstream accepted them. An
// upstream that acknowledged nothing is declined and never ends the listen.
func (f *fanIn) collectAcks(setup context.Context, requested filter) filter {
	backed := filter{flags: map[string]bool{}}
	for _, flag := range filterFlags {
		backed.flags[flag] = requested.flags[flag]
	}
	if requested.uris != nil {
		backed.uris = []string{}
	}
	for _, part := range f.parts {
		backed.uris = append(backed.uris, f.decide(setup, part)...)
	}
	return backed
}

// decide settles one part and returns the downstream URIs it backs.
func (f *fanIn) decide(setup context.Context, part *listenPart) []string {
	ack, ok := awaitAck(setup, part.acked)
	f.mu.Lock()
	defer f.mu.Unlock()
	if !ok || ack.empty() {
		part.state, part.early = partDeclined, nil
		part.cancel()
		return nil
	}
	part.state, part.ack = partAccepted, ack
	var uris []string
	for _, original := range ack.uris {
		if downstream, found := part.uris[original]; found {
			uris = append(uris, downstream)
		}
	}
	return uris
}

// awaitAck takes an acknowledgment that arrived within the setup bound, even
// when the bound has already passed by the time this part is examined.
func awaitAck(setup context.Context, acked <-chan filter) (filter, bool) {
	select {
	case ack, ok := <-acked:
		return ack, ok
	default:
	}
	select {
	case ack, ok := <-acked:
		return ack, ok
	case <-setup.Done():
		return filter{}, false
	}
}

// runUpstream holds one upstream listen and feeds its events to the fan-in.
func (f *fanIn) runUpstream(ctx context.Context, part *listenPart) {
	msg, err := listenRequest(f.req, part.request)
	if err != nil {
		close(part.acked)
		return
	}
	acked := false
	defer func() {
		if !acked {
			close(part.acked)
		}
	}()
	expect, err := f.g.resolveExpect(ctx, part.sel)
	if err != nil {
		return
	}
	err = f.g.opts.Upstream(part.sel, expect).Forward(ctx, msg, f.budget, func(raw []byte, terminal bool) error {
		if terminal {
			f.partEnded(ctx, part, raw, nil)
			return nil
		}
		if acked {
			return f.queuePart(part, raw)
		}
		acked = true
		ack, _ := parseListenFilter(member(raw, keyParams, "notifications"))
		part.acked <- ack
		return nil
	})
	if err != nil && ctx.Err() == nil {
		f.partEnded(ctx, part, nil, err)
	}
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

// partEnded ends the downstream listen when an accepted part ends: a
// graceful completion with one complete result, a failure with its error.
// A declined or still pending part ending changes nothing.
func (f *fanIn) partEnded(ctx context.Context, part *listenPart, raw []byte, err error) {
	f.mu.Lock()
	accepted := part.state == partAccepted
	if !accepted {
		part.state, part.early = partDeclined, nil
	}
	f.mu.Unlock()
	if !accepted {
		return
	}
	if errObj := member(raw, "error"); errObj != nil {
		err = upstreamError(part.sel, mcpwire.MethodListen, errObj)
	}
	if err != nil {
		err = f.g.routeError(part.sel, f.g.explainLoss(ctx, part, err))
	}
	f.g.opts.Log(fmt.Sprintf("mesh mcp: upstream listen of %s for %s ended: %v\n", part.sel.Alias, idText(f.req), err))
	f.end(err)
}

// explainLoss tells a retired grant from a lost link. A host retires a
// revoked grant's streams by closing them, which reads as an interruption;
// one passive catalog read says whether authorization ended it, including
// when the device was already approved again under a new grant.
func (g *Gateway) explainLoss(ctx context.Context, part *listenPart, err error) error {
	var rf *mcpconnect.RouteFailure
	if !errors.As(err, &rf) || (rf.Code != agentapi.RequestOutcomeUnknown && rf.Code != agentapi.HostUnreachable) {
		return err
	}
	probe, cancel := context.WithTimeout(context.WithoutCancel(ctx), authorizationProbe)
	defer cancel()
	hc, probeErr := g.opts.Catalog.Services(probe, part.sel.HostID)
	var denied *mcpconnect.RouteFailure
	retired := errors.As(probeErr, &denied) && stateForCode(denied.Code) == agentapi.GatewayUnauthorized
	if retired || (probeErr == nil && part.scope != "" && hc.Scope != part.scope) {
		return &mcpconnect.RouteFailure{Code: agentapi.AccessDenied, Host: part.sel.HostID, Service: part.sel.Alias,
			Message: "authorization for " + part.sel.Alias + " was retired", Err: err}
	}
	return err
}

// queuePart takes one upstream event. Before the downstream acknowledgment
// an accepted part's events wait in its early buffer; a declined part's
// events are dropped.
func (f *fanIn) queuePart(part *listenPart, raw []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch part.state {
	case partDeclined:
		return nil
	case partPending:
		return f.holdEarlyLocked(part, raw)
	case partAccepted:
		return f.takeLocked(part, raw)
	}
	return nil
}

func (f *fanIn) holdEarlyLocked(part *listenPart, raw []byte) error {
	size := 0
	for _, e := range part.early {
		size += len(e)
	}
	if size+len(raw) > maxEarlyBytes || !f.reserveLocked(len(raw)) {
		f.endLocked(&mcpconnect.RouteFailure{Code: agentapi.ResourceLimit, Retryable: true, Message: "too many subscription events before the acknowledgment"})
		return nil
	}
	part.early = append(part.early, append([]byte(nil), raw...))
	return nil
}

// admitEarly moves accepted parts' early events into the queue, after the
// downstream acknowledgment went out.
func (f *fanIn) admitEarly() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, part := range f.parts {
		early := part.early
		part.early = nil
		for _, raw := range early {
			f.releaseLocked(len(raw))
			_ = f.takeLocked(part, raw)
		}
	}
	f.wake()
}

// takeLocked coalesces one accepted event. List changes merge by kind and
// keep the upstream's own fields; resource updates keep the latest per URI.
func (f *fanIn) takeLocked(part *listenPart, raw []byte) error {
	if f.done {
		return nil
	}
	method, _ := stringMember(raw, "method")
	if flag, ok := flagFor(method); ok {
		return f.takeListChangeLocked(part, flag, raw)
	}
	if method != updatedMethod {
		return nil
	}
	original, _ := stringMember(raw, keyParams, keyURI)
	downstream, ok := part.uris[original]
	if !ok || !slices.Contains(part.ack.uris, original) {
		return nil
	}
	rewritten, err := setMember(raw, []string{keyParams, keyURI}, quoted(downstream))
	if err != nil {
		return err
	}
	return f.updateLocked(downstream, rewritten)
}

func flagFor(method string) (string, bool) {
	for flag, kind := range listChangedMethods {
		if kind == method {
			return flag, true
		}
	}
	return "", false
}

func (f *fanIn) takeListChangeLocked(part *listenPart, flag string, raw []byte) error {
	if !part.ack.flags[flag] {
		return nil
	}
	f.g.invalidate(part.sel, listMethodsForFlag(flag)...)
	f.markLocked(listChangedMethods[flag], raw)
	return nil
}

func listMethodsForFlag(flag string) []string {
	switch flag {
	case flagTools:
		return []string{mcpwire.MethodListTools}
	case flagPrompts:
		return []string{mcpwire.MethodListPrompts}
	default:
		return []string{mcpwire.MethodListResources, mcpwire.MethodListResourceTemplates}
	}
}

// markLocked records one list change. raw is the upstream notification, or
// nil for a change the gateway found itself.
func (f *fanIn) markLocked(kind string, raw []byte) {
	if f.done {
		return
	}
	previous, exists := f.kinds[kind]
	if exists && raw == nil {
		return
	}
	stripped := stripSubscription(raw)
	if !f.reserveLocked(len(stripped) - len(previous)) {
		f.endLocked(&mcpconnect.RouteFailure{Code: agentapi.ResourceLimit, Retryable: true, Message: "subscription buffer is full"})
		return
	}
	f.kinds[kind] = stripped
	f.wake()
}

func stripSubscription(raw []byte) json.RawMessage {
	if raw == nil {
		return nil
	}
	stripped, err := setMember(raw, []string{keyParams, keyMeta, mcpwire.SubscriptionID}, nil)
	if err != nil {
		return nil
	}
	return stripped
}

func (f *fanIn) mark(kind string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.markLocked(kind, nil)
}

func (f *fanIn) updateLocked(uri string, raw []byte) error {
	if len(raw) > mcpwire.MaxMessage {
		f.endLocked(&mcpconnect.RouteFailure{Code: agentapi.MCPMessageTooLarge, Message: "a rewritten resource update exceeds the 3.5 MiB message limit"})
		return nil
	}
	previous, exists := f.updates[uri]
	if !exists && len(f.order) >= maxPendingURIs {
		f.endLocked(&mcpconnect.RouteFailure{Code: agentapi.ResourceLimit, Retryable: true, Message: "too many pending resource updates for a slow client"})
		return nil
	}
	if !f.reserveLocked(len(raw) - len(previous)) {
		f.endLocked(&mcpconnect.RouteFailure{Code: agentapi.ResourceLimit, Retryable: true, Message: "subscription buffer is full"})
		return nil
	}
	if !exists {
		f.order = append(f.order, uri)
	}
	f.updates[uri] = raw
	f.wake()
	return nil
}

func (f *fanIn) reserveLocked(n int) bool {
	if n <= 0 {
		f.releaseLocked(-n)
		return true
	}
	if f.budget != nil && !f.budget.ReserveBytes(n) {
		return false
	}
	f.charged += n
	return true
}

func (f *fanIn) releaseLocked(n int) {
	if n <= 0 {
		return
	}
	f.charged -= n
	if f.budget != nil {
		f.budget.ReleaseBytes(n)
	}
}

func (f *fanIn) releaseAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releaseLocked(f.charged)
}

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
}

func (f *fanIn) endLocked(reason error) {
	if f.done {
		return
	}
	f.done, f.ended = true, reason
	var rf *mcpconnect.RouteFailure
	if errors.As(reason, &rf) && rf.Code == agentapi.AccessDenied {
		// Revocation fences delivery: nothing queued before it goes out,
		// here or in the frontend's output queue.
		f.kinds, f.updates, f.order = map[string]json.RawMessage{}, map[string]json.RawMessage{}, nil
		f.releaseLocked(f.charged)
		go f.retire()
	}
	f.wake()
}

func (f *fanIn) ending() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.done
}

// loop delivers coalesced events until the listen ends, the client cancels,
// or the profile changes the routes this listen covers.
func (f *fanIn) loop(ctx context.Context, profile Profile) error {
	tick := time.NewTicker(f.g.opts.ProfileEvery)
	defer tick.Stop()
	defer func() {
		for _, p := range f.parts {
			p.cancel()
		}
	}()
	routes := routesDigest(f.g.routes(profile))
	native := profileDigest(profile)
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("listen cancelled: %w", ctx.Err())
		case <-f.signal:
		case <-tick.C:
			native = f.checkProfile(routes, native)
		}
		finished, err := f.flush()
		if finished || err != nil {
			return err
		}
	}
}

func routesDigest(routes []Selection) string {
	raw, _ := json.Marshal(routes)
	return digest(string(raw))
}

// checkProfile notices local selection changes. Any change to the routes,
// including an instance pin, reports the list changes and ends the listen,
// so the client listens again over exactly the current routes; a change to
// native groups only reports a tools change.
func (f *fanIn) checkProfile(routes, native string) string {
	_, profile, err := f.g.current()
	if err != nil {
		return native
	}
	next := profileDigest(profile)
	if next == native {
		return native
	}
	for _, flag := range filterFlags {
		if f.backed.flags[flag] {
			f.mark(listChangedMethods[flag])
		}
	}
	if routesDigest(f.g.routes(profile)) != routes {
		f.end(nil)
	}
	return next
}

// flush writes pending events in a stable order and, once the listen has
// ended, its single terminal reply. Bytes stay charged until written.
func (f *fanIn) flush() (bool, error) {
	f.mu.Lock()
	kinds, updates, order, done, reason := f.kinds, f.updates, f.order, f.done, f.ended
	f.kinds, f.updates, f.order = map[string]json.RawMessage{}, map[string]json.RawMessage{}, nil
	f.mu.Unlock()
	if err := f.flushKinds(kinds); err != nil {
		return true, err
	}
	for _, uri := range order {
		raw := updates[uri]
		err := f.emit(f.tag(raw), false)
		f.release(len(raw))
		if err != nil {
			return true, err
		}
	}
	if !done {
		return false, nil
	}
	f.g.opts.Log(fmt.Sprintf("mesh mcp: listen %s ended: %v\n", idText(f.req), reason))
	if reason != nil {
		return true, reason
	}
	reply, err := f.g.completeReply(f.req, map[string]any{}, 0)
	if err != nil {
		return true, err
	}
	return true, f.emit(reply, true)
}

func (f *fanIn) flushKinds(kinds map[string]json.RawMessage) error {
	for _, flag := range filterFlags {
		kind := listChangedMethods[flag]
		raw, ok := kinds[kind]
		if !ok {
			continue
		}
		f.release(len(raw))
		if err := f.sendChange(kind, raw); err != nil {
			return err
		}
	}
	return nil
}

func (f *fanIn) release(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releaseLocked(n)
}

// sendChange forwards an upstream list change with its own fields, or a
// plain one the gateway found.
func (f *fanIn) sendChange(kind string, raw json.RawMessage) error {
	if raw == nil {
		return f.send(kind, map[string]any{})
	}
	return f.emit(f.tag(raw), false)
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

// jitter spreads refreshes over half to one and a half intervals so many
// frontends do not poll a host in step.
func jitter(d time.Duration) time.Duration {
	var b [4]byte
	_, _ = rand.Read(b[:])
	// A 32-bit fraction of d keeps the arithmetic within int64.
	fraction := time.Duration(binary.LittleEndian.Uint32(b[:]))
	return d/2 + d/(1<<32)*fraction
}

func idText(req mcpwire.Message) string {
	raw, _ := req.ID().MarshalJSON()
	return string(raw)
}
