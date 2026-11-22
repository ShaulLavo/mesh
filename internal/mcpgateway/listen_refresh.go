package mcpgateway

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/mcpconnect"
	"github.com/shaul/mesh/internal/mcpwire"
)

// hostWatches shares one listing-revision watch per host among the
// frontend's listens (PLAN section 7.3). A host that cannot deliver
// revisions falls back to bounded refresh in each listen.
type hostWatches struct {
	g     *Gateway
	mu    sync.Mutex
	hosts map[string]*hostWatch
}

type hostWatch struct {
	subs   map[*fanIn]struct{}
	cancel context.CancelFunc
}

func newHostWatches(g *Gateway) *hostWatches {
	return &hostWatches{g: g, hosts: map[string]*hostWatch{}}
}

// join subscribes f to host's watch until ctx ends.
func (w *hostWatches) join(ctx context.Context, f *fanIn, host string) {
	w.mu.Lock()
	hw := w.hosts[host]
	if hw == nil {
		// The watch outlives any one listen; the last one to leave stops it.
		watchCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		hw = &hostWatch{subs: map[*fanIn]struct{}{}, cancel: cancel}
		w.hosts[host] = hw
		go w.run(watchCtx, host, hw)
	}
	hw.subs[f] = struct{}{}
	w.mu.Unlock()
	context.AfterFunc(ctx, func() { w.leave(f, host, hw) })
}

func (w *hostWatches) leave(f *fanIn, host string, hw *hostWatch) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(hw.subs, f)
	if len(hw.subs) == 0 && w.hosts[host] == hw {
		delete(w.hosts, host)
		hw.cancel()
	}
}

func (w *hostWatches) subscribers(hw *hostWatch) []*fanIn {
	w.mu.Lock()
	defer w.mu.Unlock()
	subs := make([]*fanIn, 0, len(hw.subs))
	for f := range hw.subs {
		subs = append(subs, f)
	}
	return subs
}

func (w *hostWatches) run(ctx context.Context, host string, hw *hostWatch) {
	err := w.g.opts.Catalog.Watch(ctx, host, func(change ListingChange) {
		w.g.invalidateService(host, change.ServiceID)
		for _, f := range w.subscribers(hw) {
			f.onListing(host, change)
		}
	})
	if ctx.Err() != nil {
		return
	}
	if err == nil {
		err = ErrWatchUnsupported
	}
	w.mu.Lock()
	if w.hosts[host] == hw {
		delete(w.hosts, host)
	}
	w.mu.Unlock()
	for _, f := range w.subscribers(hw) {
		f.watchFailed(host, err)
	}
}

// invalidateService expires every route of one service on a host.
func (g *Gateway) invalidateService(host, serviceID string) {
	routes := map[routeKey]bool{}
	g.mu.Lock()
	for key := range g.listings {
		if key.route.host == host && key.route.service == serviceID {
			routes[key.route] = true
		}
	}
	g.mu.Unlock()
	for route := range routes {
		g.invalidate(Selection{HostID: route.host, ServiceID: route.service, Instance: route.instance})
	}
}

// wantsListChanges reports whether this listen backs any list change.
func (f *fanIn) wantsListChanges() bool {
	for _, flag := range filterFlags {
		if f.backed.flags[flag] {
			return true
		}
	}
	return false
}

// watchHosts joins the listing-revision watch of each host this listen
// reports list changes for.
func (f *fanIn) watchHosts(ctx context.Context) {
	if !f.wantsListChanges() {
		return
	}
	joined := map[string]bool{}
	for _, p := range f.parts {
		if joined[p.sel.HostID] {
			continue
		}
		joined[p.sel.HostID] = true
		f.g.watches.join(ctx, f, p.sel.HostID)
	}
}

// onListing receives one listing revision a host published.
func (f *fanIn) onListing(host string, change ListingChange) {
	for _, p := range f.parts {
		if p.sel.HostID == host && p.sel.ServiceID == change.ServiceID {
			f.routeChanged(p)
		}
	}
}

func (f *fanIn) routeChanged(p *listenPart) {
	for _, flag := range filterFlags {
		if f.backed.flags[flag] && member(p.caps, capabilityForFlag(flag)) != nil {
			f.mark(listChangedMethods[flag])
		}
	}
}

// watchFailed ends the listen when the host retired this caller, and
// otherwise switches the host's routes to bounded refresh.
func (f *fanIn) watchFailed(host string, err error) {
	var rf *mcpconnect.RouteFailure
	if errors.As(err, &rf) && stateForCode(rf.Code) == agentapi.GatewayUnauthorized {
		f.end(&mcpconnect.RouteFailure{Code: agentapi.AccessDenied, Host: host, Message: "authorization on host " + host + " was retired", Err: err})
		return
	}
	f.mu.Lock()
	f.pollHosts[host] = true
	f.mu.Unlock()
}

func (f *fanIn) polled(host string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pollHosts[host]
}

type pollTarget struct {
	sel       Selection
	kind      listKind
	flag      string
	every     time.Duration
	next      time.Time
	digest    string
	baselined bool
}

// pollTargets lists every listing whose changes this listen reports, with
// the baseline the client may already have listed.
func (f *fanIn) pollTargets() []*pollTarget {
	var targets []*pollTarget
	for _, p := range f.parts {
		for _, flag := range filterFlags {
			if !f.backed.flags[flag] || member(p.caps, capabilityForFlag(flag)) == nil {
				continue
			}
			for _, method := range listMethodsForFlag(flag) {
				targets = append(targets, f.target(p.sel, kindFor(method), flag))
			}
		}
	}
	return targets
}

func (f *fanIn) target(sel Selection, kind listKind, flag string) *pollTarget {
	t := &pollTarget{sel: sel, kind: kind, flag: flag, every: f.g.opts.RefreshEvery}
	key := listingKey{route: keyOf(sel), method: kind.method}
	if l := f.g.cached(key); l != nil {
		t.digest, t.baselined = l.Digest, true
		t.next = f.g.opts.Now().Add(jitter(t.every))
	}
	return t
}

func (f *fanIn) pollTick() time.Duration {
	return min(max(f.g.opts.RefreshEvery/10, 10*time.Millisecond), 5*time.Second)
}

// poll refreshes the listings of hosts that cannot deliver revisions, on
// its own goroutine so refresh never delays event delivery.
func (f *fanIn) poll(ctx context.Context) {
	targets := f.pollTargets()
	if len(targets) == 0 {
		return
	}
	tick := time.NewTicker(f.pollTick())
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		for _, t := range targets {
			f.pollOne(ctx, t)
		}
	}
}

func (f *fanIn) pollOne(ctx context.Context, t *pollTarget) {
	if f.ending() || !f.polled(t.sel.HostID) {
		return
	}
	now := f.g.opts.Now()
	if t.baselined && now.Before(t.next) {
		return
	}
	digest, err := f.g.refreshDigest(ctx, f.req, t.sel, t.kind)
	if err != nil {
		f.pollFailed(t, now, err)
		return
	}
	changed := t.baselined && digest != t.digest
	t.digest, t.baselined, t.every = digest, true, f.g.opts.RefreshEvery
	t.next = now.Add(jitter(t.every))
	if changed {
		f.mark(listChangedMethods[t.flag])
	}
}

// pollFailed ends the listen when authorization was retired, and otherwise
// backs off.
func (f *fanIn) pollFailed(t *pollTarget, now time.Time, err error) {
	var rf *mcpconnect.RouteFailure
	if errors.As(err, &rf) && stateForCode(rf.Code) == agentapi.GatewayUnauthorized {
		f.end(&mcpconnect.RouteFailure{Code: agentapi.AccessDenied, Host: t.sel.HostID, Service: t.sel.Alias, Message: "authorization for " + t.sel.Alias + " was retired", Err: err})
		return
	}
	t.every = min(t.every*2, maxRefreshEvery)
	t.next = now.Add(jitter(t.every))
}

// refreshDigest reads one listing again under current authorization, keeps
// it and returns its digest.
func (g *Gateway) refreshDigest(ctx context.Context, downstream mcpwire.Message, sel Selection, kind listKind) (string, error) {
	hc, entry, err := newCatalogMemo(g).lookup(ctx, sel)
	if err != nil {
		return "", g.routeError(sel, err)
	}
	gen := g.fence(sel.HostID)
	l, err := g.fetchListing(ctx, downstream, sel, kind, &Expect{Revision: entry.Revision, Account: entry.Account})
	if err != nil {
		return "", g.routeError(sel, err)
	}
	key := listingKey{route: keyOf(sel), method: kind.method}
	l.Scope, l.ListingRevision = hc.Scope, entry.ListingRevision
	g.store(key, l, gen)
	return l.Digest, nil
}
