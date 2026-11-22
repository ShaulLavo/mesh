package mcpgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/mcpconnect"
	"github.com/shaul/mesh/internal/mcpwire"
	"github.com/shaul/mesh/internal/transport"
)

// Mesh-owned metadata keys of the gateway (PLAN section 2.6).
const (
	// MetaRoute is the route metadata on every hosted entry.
	MetaRoute = mcpconnect.MetaPrefix + "route"
	// MetaRoutes is the state of every selected route on each list page.
	MetaRoutes = mcpconnect.MetaPrefix + "routes"
	// MetaUpstreams carries each upstream's own page-level fields and
	// metadata, which cannot be merged into one result.
	MetaUpstreams = mcpconnect.MetaPrefix + "upstreams"
)

// Emit receives one MCP message for the downstream client. The last call
// carries the terminal response.
type Emit = func(raw []byte, terminal bool) error

// Native serves the profile's native Mesh tools.
type Native interface {
	// Tools returns the native tool definitions of the profile's groups in
	// their stable order. Nil groups select every native tool.
	Tools(groups []string) []json.RawMessage
	Has(groups []string, name string) bool
	Call(ctx context.Context, req mcpwire.Message, emit Emit) error
}

// Upstream forwards one MCP request to a selected hosted service over H3.
type Upstream = mcpconnect.Forwarder

// Expect is the service state a request was resolved against. The owning
// host refuses the request when the service moved on.
type Expect struct {
	Revision int64
	Account  string
}

// CatalogEntry is one hosted service in a host's passive catalog.
type CatalogEntry struct {
	ServiceID string
	Name      string
	Revision  int64
	Account   string
	// ListingRevision changes whenever the owning host sees the service's
	// listings change.
	ListingRevision string
	// State is the hosted service state: ready, stopped, starting,
	// uncertain, incompatible or failed.
	State string
}

// HostCatalog is one host's passive catalog as this caller sees it.
type HostCatalog struct {
	// Scope names the caller's authenticated scope on the host, grant
	// incarnation included. Cached listings are bound to it.
	Scope   string
	Entries []CatalogEntry
}

// ListingChange is one listing revision a host published.
type ListingChange struct {
	ServiceID string
	Account   string
	Token     string
}

// ErrWatchUnsupported means a host cannot deliver listing revisions; the
// gateway refreshes that host's listings itself.
var ErrWatchUnsupported = errors.New("host cannot watch listing revisions")

// Catalog reads one host's passive catalog. It never starts a service.
// Errors are *mcpconnect.RouteFailure values carrying the host's code.
type Catalog interface {
	Services(ctx context.Context, hostID string) (HostCatalog, error)
	// Watch delivers the host's listing revisions until ctx ends or the
	// watch fails.
	Watch(ctx context.Context, hostID string, changed func(ListingChange)) error
}

// Options configures one gateway frontend.
type Options struct {
	Profile string
	Store   *Store
	Native  Native
	// Hosted reports whether this frontend has a verified application
	// identity. Without one, hosted routes are never used.
	Hosted bool
	// Upstream builds the H3 forwarder for one selection, resolved against
	// expect when known.
	Upstream func(sel Selection, expect *Expect) Upstream
	Catalog  Catalog
	// Cache persists listings for stale fallback across frontends.
	Cache ListingCache
	// CursorSecret is the frontend's private credential; see newCursorSigner.
	CursorSecret []byte
	ServerInfo   json.RawMessage
	Now          func() time.Time
	Log          func(string)
	// ListenSetup bounds the discovery and acknowledgments of a listen.
	ListenSetup time.Duration
	// RefreshEvery is the base interval for polling listings of hosts that
	// cannot deliver listing revisions.
	RefreshEvery time.Duration
	// ProfileEvery is how often an active listen checks the profile file.
	ProfileEvery time.Duration
}

// Gateway implements the aggregate stateless MCP entry for one profile. It
// keeps only request state and caches; every request resolves its route
// under the current profile file and the host's current authorization.
type Gateway struct {
	opts   Options
	signer cursorSigner

	mu        sync.Mutex
	listings  map[listingKey]*Listing
	retained  int
	statuses  map[routeKey]Status
	refreshMu map[listingKey]*sync.Mutex
	starts    map[string]chan struct{}
	// hostScope is the last scope each host reported for this caller, and
	// hostGen fences saves started before a host's listings were retired.
	hostScope map[string]string
	hostGen   map[string]uint64
	// resolved is the service state this frontend last read from each
	// host's catalog. Calls are bound to it, never to a cached listing
	// that may predate a publish.
	resolved map[[2]string]resolvedService
	watches  *hostWatches
}

type resolvedService struct {
	scope  string
	expect Expect
}

// routeKey names one route: a service, and the instance a profile pins.
type routeKey struct{ host, service, instance string }

func keyOf(sel Selection) routeKey { return routeKey{sel.HostID, sel.ServiceID, sel.Instance} }

type listingKey struct {
	route  routeKey
	method string
	// params digests the unowned list request parameters, which can change
	// what an upstream lists.
	params string
}

// New builds a gateway. It reads the profile on first use.
func New(opts Options) (*Gateway, error) {
	if opts.Store == nil || opts.Native == nil || opts.Upstream == nil || opts.Catalog == nil {
		return nil, errors.New("mcp gateway: store, native tools, upstream and catalog are required")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Log == nil {
		opts.Log = func(string) {}
	}
	if opts.ServerInfo == nil {
		opts.ServerInfo = json.RawMessage(`{"name":"mesh","version":"dev"}`)
	}
	if opts.ListenSetup <= 0 {
		opts.ListenSetup = 5 * time.Second
	}
	if opts.RefreshEvery <= 0 {
		opts.RefreshEvery = time.Minute
	}
	if opts.ProfileEvery <= 0 {
		opts.ProfileEvery = time.Second
	}
	if opts.Cache == nil {
		opts.Cache = nopCache{}
	}
	g := &Gateway{
		opts:      opts,
		signer:    newCursorSigner(opts.CursorSecret),
		listings:  map[listingKey]*Listing{},
		statuses:  map[routeKey]Status{},
		refreshMu: map[listingKey]*sync.Mutex{},
		starts:    map[string]chan struct{}{},
		hostScope: map[string]string{},
		hostGen:   map[string]uint64{},
		resolved:  map[[2]string]resolvedService{},
	}
	g.watches = newHostWatches(g)
	return g, nil
}

type budgetKey struct{}

// budgetOf returns the downstream request's byte budget, which every
// upstream exchange and buffer made on its behalf is charged against.
func budgetOf(ctx context.Context) transport.InboundByteBudget {
	budget, _ := ctx.Value(budgetKey{}).(transport.InboundByteBudget)
	return budget
}

type retireKey struct{}

// WithRetire attaches the frontend's way to discard a stream's queued
// notifications, used when the stream's authorization is retired.
func WithRetire(ctx context.Context, retire func()) context.Context {
	return context.WithValue(ctx, retireKey{}, retire)
}

func retireOf(ctx context.Context) func() {
	retire, _ := ctx.Value(retireKey{}).(func())
	if retire == nil {
		return func() {}
	}
	return retire
}

// Methods are the requests the gateway serves.
var Methods = mcpconnect.ForwardMethods

// Forward serves one validated downstream request. It emits a terminal
// response or returns a *mcpconnect.RouteFailure for the frontend to report.
func (g *Gateway) Forward(ctx context.Context, req mcpwire.Message, budget transport.InboundByteBudget, emit Emit) error {
	ctx = context.WithValue(ctx, budgetKey{}, budget)
	file, profile, err := g.current()
	if err != nil {
		return g.failure(agentapi.StateUnavailable, true, "", "", err)
	}
	switch req.Method() {
	case mcpwire.MethodDiscover:
		return g.discover(ctx, req, profile, emit)
	case mcpwire.MethodListTools, mcpwire.MethodListPrompts, mcpwire.MethodListResources, mcpwire.MethodListResourceTemplates:
		return g.list(ctx, req, file, profile, emit)
	case mcpwire.MethodCallTool:
		return g.callNamed(ctx, req, file, profile, KindTool, emit)
	case mcpwire.MethodGetPrompt:
		return g.callNamed(ctx, req, file, profile, KindPrompt, emit)
	case mcpwire.MethodReadResource:
		return g.readResource(ctx, req, profile, emit)
	case mcpwire.MethodComplete:
		return g.complete(ctx, req, file, profile, emit)
	case mcpwire.MethodListen:
		return g.listen(ctx, req, profile, emit)
	default:
		return emitFault(emit, req, mcpwire.MethodNotFound, "MCP method not found")
	}
}

// current returns the profile file as it is now.
func (g *Gateway) current() (File, Profile, error) {
	file, err := g.opts.Store.Load()
	if err != nil {
		return File{}, Profile{}, fmt.Errorf("load %s: %w", ProfilesFileName, err)
	}
	profile, ok := file.Profiles[g.opts.Profile]
	if !ok {
		// A removed profile exposes only mesh_skill until it returns.
		profile = Profile{Native: []string{}}
	}
	return file, profile, nil
}

// routes is the profile's hosted selections this frontend may use. Without
// a verified application identity there are none, whatever the profile
// says after an edit.
func (g *Gateway) routes(profile Profile) []Selection {
	if g.opts.Hosted {
		return profile.Services
	}
	for _, sel := range profile.Services {
		g.setStatusCode(sel, agentapi.GatewayUnauthorized, agentapi.AccessDenied, identityRequired)
	}
	return nil
}

const identityRequired = "hosted services need a verified application identity; start mesh mcp with --identity NAME"

func (g *Gateway) hostedRefused(sel Selection) error {
	return &mcpconnect.RouteFailure{Code: agentapi.AccessDenied, Host: sel.HostID, Service: sel.Alias, Message: identityRequired}
}

// callNamed routes tools/call and prompts/get by exposed name.
func (g *Gateway) callNamed(ctx context.Context, req mcpwire.Message, file File, profile Profile, kind string, emit Emit) error {
	name, _ := stringMember(req.Params(), keyName)
	if kind == KindTool && g.opts.Native.Has(profile.Native, name) {
		if err := g.opts.Native.Call(ctx, req, emit); err != nil {
			return fmt.Errorf("native %s: %w", name, err)
		}
		return nil
	}
	sel, id, ok := g.resolveName(file, profile, name, kind)
	if !ok {
		what := map[string]string{KindTool: "tool", KindPrompt: "prompt"}[kind]
		return emitFault(emit, req, mcpwire.InvalidParams, fmt.Sprintf("Unknown %s %q in profile %s", what, name, g.opts.Profile))
	}
	forwarded, err := rewriteRequest(req, []string{keyParams, keyName}, quoted(id.Name))
	if err != nil {
		return emitFault(emit, req, mcpwire.InvalidParams, err.Error())
	}
	return g.forward(ctx, sel, forwarded, emit)
}

func (g *Gateway) resolveName(file File, profile Profile, name, kind string) (Selection, Identity, bool) {
	if !IsHostedName(name) {
		return Selection{}, Identity{}, false
	}
	id, err := ResolveName(file, name)
	if err != nil || id.Kind != kind {
		return Selection{}, Identity{}, false
	}
	sel, ok := profile.Lookup(id.HostID, id.ServiceID)
	if !ok || !sel.Allows(id.Name) {
		return Selection{}, Identity{}, false
	}
	return sel, id, true
}

func (g *Gateway) readResource(ctx context.Context, req mcpwire.Message, profile Profile, emit Emit) error {
	uri, _ := stringMember(req.Params(), keyURI)
	sel, original, ok := resolveURI(profile, uri, false)
	if !ok {
		return emitFault(emit, req, mcpwire.InvalidParams, fmt.Sprintf("Unknown resource %q in profile %s", uri, g.opts.Profile))
	}
	forwarded, err := rewriteRequest(req, []string{keyParams, keyURI}, quoted(original))
	if err != nil {
		return emitFault(emit, req, mcpwire.InvalidParams, err.Error())
	}
	return g.forward(ctx, sel, forwarded, emit)
}

func resolveURI(profile Profile, uri string, template bool) (Selection, string, bool) {
	hostID, serviceID, original, err := DecodeURI(uri, template)
	if err != nil {
		return Selection{}, "", false
	}
	sel, ok := profile.Lookup(hostID, serviceID)
	return sel, original, ok
}

// complete routes completion/complete by its prompt name or resource
// template reference.
func (g *Gateway) complete(ctx context.Context, req mcpwire.Message, file File, profile Profile, emit Emit) error {
	refType, _ := stringMember(req.Params(), "ref", "type")
	var sel Selection
	var path []string
	var original string
	ok := false
	switch refType {
	case "ref/prompt":
		name, _ := stringMember(req.Params(), "ref", keyName)
		var id Identity
		sel, id, ok = g.resolveName(file, profile, name, KindPrompt)
		path, original = []string{keyParams, "ref", keyName}, id.Name
	case "ref/resource":
		uri, _ := stringMember(req.Params(), "ref", keyURI)
		sel, original, ok = resolveURI(profile, uri, true)
		path = []string{keyParams, "ref", keyURI}
	}
	if !ok {
		return emitFault(emit, req, mcpwire.InvalidParams, "Unknown completion reference in profile "+g.opts.Profile)
	}
	forwarded, err := rewriteRequest(req, path, quoted(original))
	if err != nil {
		return emitFault(emit, req, mcpwire.InvalidParams, err.Error())
	}
	return g.forward(ctx, sel, forwarded, emit)
}

func rewriteRequest(req mcpwire.Message, path []string, value json.RawMessage) (mcpwire.Message, error) {
	raw, err := setMember(req.Raw(), path, value)
	if err != nil {
		return mcpwire.Message{}, err
	}
	if len(raw) > mcpwire.MaxMessage {
		return mcpwire.Message{}, mcpwire.ErrTooLarge
	}
	msg, fault := mcpwire.Parse(raw)
	if fault != nil {
		return mcpwire.Message{}, fault
	}
	return msg, nil
}

// resolveExpect binds a call to the service state this frontend last read
// from the host's catalog, reading it now when it has none. A cached
// listing never supplies it: one loaded from disk can predate a publish.
func (g *Gateway) resolveExpect(ctx context.Context, sel Selection) (*Expect, error) {
	g.mu.Lock()
	known, ok := g.resolved[[2]string{sel.HostID, sel.ServiceID}]
	current := ok && known.scope == g.hostScope[sel.HostID]
	g.mu.Unlock()
	if current {
		expect := known.expect
		return &expect, nil
	}
	_, entry, err := newCatalogMemo(g).lookup(ctx, sel)
	if err != nil {
		return nil, err
	}
	return &Expect{Revision: entry.Revision, Account: entry.Account}, nil
}

// forward sends one request to its owning host and rewrites resource
// identities and server identity in what comes back.
func (g *Gateway) forward(ctx context.Context, sel Selection, req mcpwire.Message, emit Emit) error {
	if !g.opts.Hosted {
		return g.hostedRefused(sel)
	}
	expect, err := g.resolveExpect(ctx, sel)
	if err != nil {
		return g.routeError(sel, err)
	}
	rewriter := uriRewriter{hostID: sel.HostID, serviceID: sel.ServiceID}
	err = g.opts.Upstream(sel, expect).Forward(ctx, req, budgetOf(ctx), func(raw []byte, terminal bool) error {
		var err error
		if terminal {
			raw, err = g.terminal(req, sel, raw, rewriter)
		} else {
			raw, err = rewriter.notification(raw)
		}
		if err == nil && len(raw) > mcpwire.MaxMessage {
			err = mcpwire.ErrTooLarge
		}
		if err != nil {
			return g.failure(agentapi.ServiceFailed, false, sel.HostID, sel.Alias, err)
		}
		return emit(raw, terminal)
	})
	return g.routeError(sel, err)
}

func (g *Gateway) terminal(req mcpwire.Message, sel Selection, raw []byte, rewriter uriRewriter) ([]byte, error) {
	g.observeReplyError(sel, raw)
	// A host's Mesh failure inside tools/call is a tool execution error the
	// model can act on (section 2.4).
	if converted, matched, err := mcpconnect.TranslateHostToolError(req, raw, sel.HostID, sel.Alias, g.opts.ServerInfo); matched || err != nil {
		if err != nil {
			return nil, fmt.Errorf("convert host tool error: %w", err)
		}
		return converted, nil
	}
	raw, err := rewriter.response(req.Method(), raw)
	if err != nil {
		return nil, err
	}
	// No fallback identity: one route's server must never be reported for
	// another's result.
	raw, err = mcpconnect.RewriteForwardedResult(req.Method(), raw, g.opts.ServerInfo)
	if err != nil {
		return nil, fmt.Errorf("rewrite server identity: %w", err)
	}
	return raw, nil
}

// observeReplyError applies what a host's in-band Mesh error says about the
// route, so a denial retires protected listings like any other.
func (g *Gateway) observeReplyError(sel Selection, raw []byte) {
	code, ok := stringMember(raw, "error", "data", "meshCode")
	if !ok {
		return
	}
	message, _ := stringMember(raw, "error", "message")
	g.observeFailure(sel, &mcpconnect.RouteFailure{Code: agentapi.Code(code), Host: sel.HostID, Service: sel.Alias, Message: message})
}

// routeError gives a hosted failure its gateway code and records what it
// says about the route.
func (g *Gateway) routeError(sel Selection, err error) error {
	if err == nil {
		return nil
	}
	var rf *mcpconnect.RouteFailure
	if !errors.As(err, &rf) {
		if errors.Is(err, mcpwire.ErrTooLarge) {
			return g.failure(agentapi.MCPMessageTooLarge, false, sel.HostID, sel.Alias, err)
		}
		return err
	}
	if rf.Code == agentapi.HostIdentityMismatch {
		rf.Code = agentapi.RouteIdentityChanged
	}
	if rf.Service == "" || len(rf.Service) == 32 {
		rf.Service = sel.Alias
	}
	g.observeFailure(sel, rf)
	return rf
}

func (g *Gateway) failure(code agentapi.Code, retryable bool, host, service string, err error) *mcpconnect.RouteFailure {
	return &mcpconnect.RouteFailure{Code: code, Retryable: retryable, Host: host, Service: service, Message: agentapi.CleanMessage(err.Error()), Err: err}
}

func emitFault(emit Emit, req mcpwire.Message, code int, message string) error {
	raw, err := (&mcpwire.Fault{ID: req.ID(), Code: code, Message: message}).Reply()
	if err != nil {
		return fmt.Errorf("encode gateway fault: %w", err)
	}
	return emit(raw, true)
}

func (g *Gateway) completeReply(req mcpwire.Message, result any, ttl time.Duration) ([]byte, error) {
	raw, err := encode(result)
	if err != nil {
		return nil, err
	}
	reply, err := mcpwire.Complete(req, raw, g.opts.ServerInfo, ttl.Milliseconds())
	if err != nil {
		return nil, fmt.Errorf("complete %s reply: %w", req.Method(), err)
	}
	return reply, nil
}

func (g *Gateway) instructions(routes []Selection) string {
	var b strings.Builder
	b.WriteString("Mesh gateway profile " + g.opts.Profile + ". Call mesh_skill to read Mesh usage instructions before acting on a host.")
	if len(routes) > 0 {
		aliases := make([]string, 0, len(routes))
		for _, s := range routes {
			aliases = append(aliases, s.Alias)
		}
		b.WriteString(" Hosted services: " + strings.Join(aliases, ", ") + ". Their tools and prompts use h_ names; their resources use mesh-mcp:// URIs.")
	}
	return b.String()
}
