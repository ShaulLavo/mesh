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

// MetaRoute is the Mesh-owned route metadata on every hosted entry.
const MetaRoute = mcpconnect.MetaPrefix + "route"

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

// CatalogEntry is one hosted service in a host's passive catalog.
type CatalogEntry struct {
	ServiceID string
	Name      string
	Revision  int64
	// State is the hosted service state: ready, stopped, starting,
	// uncertain, incompatible or failed.
	State string
}

// Catalog reads one host's passive catalog. It never starts a service.
// Errors are *mcpconnect.RouteFailure values carrying the host's code.
type Catalog interface {
	Services(ctx context.Context, hostID string) ([]CatalogEntry, error)
}

// Options configures one gateway frontend.
type Options struct {
	Profile string
	Store   *Store
	Native  Native
	// Upstream builds the H3 forwarder for one selection.
	Upstream func(Selection) Upstream
	Catalog  Catalog
	// Cache persists listings for stale fallback across frontends.
	Cache ListingCache
	// CursorSecret is the frontend's private credential; see newCursorSigner.
	CursorSecret []byte
	ServerInfo   json.RawMessage
	Now          func() time.Time
	Log          func(string)
	// ListenSetup bounds how long a listen waits for upstream acknowledgments.
	ListenSetup time.Duration
	// RefreshEvery is the base interval for polling listings that have no
	// upstream listener.
	RefreshEvery time.Duration
	// ProfileEvery is how often an active listen checks the profile file.
	ProfileEvery time.Duration
}

// Gateway implements the aggregate stateless MCP entry for one profile. It
// keeps only request state and caches; every request resolves its route
// under the current profile file.
type Gateway struct {
	opts   Options
	signer cursorSigner

	mu        sync.Mutex
	loaded    bool
	file      File
	profile   Profile
	upstreams map[routeKey]Upstream
	listings  map[listingKey]*Listing
	statuses  map[routeKey]Status
	refreshMu map[listingKey]*sync.Mutex
	starts    map[string]chan struct{}
}

type routeKey struct{ host, service string }

type listingKey struct {
	route  routeKey
	method string
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
	return &Gateway{
		opts:      opts,
		signer:    newCursorSigner(opts.CursorSecret),
		upstreams: map[routeKey]Upstream{},
		listings:  map[listingKey]*Listing{},
		statuses:  map[routeKey]Status{},
		refreshMu: map[listingKey]*sync.Mutex{},
		starts:    map[string]chan struct{}{},
	}, nil
}

type budgetKey struct{}

// budgetOf returns the downstream request's byte budget, which every
// upstream exchange made on its behalf is charged against.
func budgetOf(ctx context.Context) transport.InboundByteBudget {
	budget, _ := ctx.Value(budgetKey{}).(transport.InboundByteBudget)
	return budget
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

// current returns the profile file, reloading it when it changed.
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
	g.mu.Lock()
	g.loaded, g.file, g.profile = true, file, profile
	g.mu.Unlock()
	return file, profile, nil
}

func (g *Gateway) upstream(sel Selection) Upstream {
	key := routeKey{sel.HostID, sel.ServiceID}
	g.mu.Lock()
	defer g.mu.Unlock()
	if u, ok := g.upstreams[key]; ok {
		return u
	}
	u := g.opts.Upstream(sel)
	g.upstreams[key] = u
	return u
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

// forward sends one request to its owning host and rewrites resource
// identities and server identity in what comes back.
func (g *Gateway) forward(ctx context.Context, sel Selection, req mcpwire.Message, emit Emit) error {
	rewriter := uriRewriter{hostID: sel.HostID, serviceID: sel.ServiceID}
	err := g.upstream(sel).Forward(ctx, req, budgetOf(ctx), func(raw []byte, terminal bool) error {
		var err error
		if terminal {
			raw, err = g.terminal(req, sel, raw, rewriter)
		} else {
			raw, err = rewriter.notification(raw)
		}
		if err != nil {
			return g.failure(agentapi.ServiceFailed, false, sel.HostID, sel.ServiceID, err)
		}
		return emit(raw, terminal)
	})
	return g.routeError(sel, err)
}

func (g *Gateway) terminal(req mcpwire.Message, sel Selection, raw []byte, rewriter uriRewriter) ([]byte, error) {
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
	if len(raw) > mcpwire.MaxMessage {
		return nil, mcpwire.ErrTooLarge
	}
	return raw, nil
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
			return g.failure(agentapi.MCPMessageTooLarge, false, sel.HostID, sel.ServiceID, err)
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

func (g *Gateway) instructions(profile Profile) string {
	var b strings.Builder
	b.WriteString("Mesh gateway profile " + g.opts.Profile + ". Call mesh_skill to read Mesh usage instructions before acting on a host.")
	if len(profile.Services) > 0 {
		aliases := make([]string, 0, len(profile.Services))
		for _, s := range profile.Services {
			aliases = append(aliases, s.Alias)
		}
		b.WriteString(" Hosted services: " + strings.Join(aliases, ", ") + ". Their tools and prompts use h_ names; their resources use mesh-mcp:// URIs.")
	}
	return b.String()
}
