package mcpgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/mcpconnect"
	"github.com/shaul/mesh/internal/mcpwire"
	"github.com/shaul/mesh/internal/transport"
)

const testMeta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{"elicitation":{}}}`

// fakeService is an in-process hosted service behind one route. It answers
// like a hosted MCP server after the owning host restored caller IDs.
type fakeService struct {
	t         *testing.T
	name      string
	tools     []string
	prompts   []string
	resources []string
	caps      string
	ttlMS     int
	fail      error
	mu        sync.Mutex
	calls     []json.RawMessage
	listens   []*fakeListen
	ackDelay  time.Duration
	ackFilter func(json.RawMessage) string
}

type fakeListen struct {
	params json.RawMessage
	events chan string
	ctx    context.Context
}

func (s *fakeService) Forward(ctx context.Context, req mcpwire.Message, _ transport.InboundByteBudget, emit func([]byte, bool) error) error {
	s.mu.Lock()
	fail := s.fail
	s.calls = append(s.calls, append(json.RawMessage(nil), req.Raw()...))
	s.mu.Unlock()
	if fail != nil {
		return fail
	}
	id, _ := req.ID().MarshalJSON()
	reply := func(result string) error {
		return emit([]byte(`{"jsonrpc":"2.0","id":`+string(id)+`,"result":`+result+`}`), true)
	}
	switch req.Method() {
	case mcpwire.MethodDiscover:
		return reply(`{"resultType":"complete","supportedVersions":["2026-07-28"],"capabilities":` + s.caps + `,"ttlMs":60000,"cacheScope":"private","_meta":{"io.modelcontextprotocol/serverInfo":{"name":"` + s.name + `","version":"1"}}}`)
	case mcpwire.MethodListTools:
		return reply(s.page(req, "tools", s.tools, func(n string) string {
			return `{"name":"` + n + `","inputSchema":{"type":"object"},"x-vendor":{"keep":[1,2.50,"z"]},"annotations":{"readOnlyHint":true}}`
		}))
	case mcpwire.MethodListPrompts:
		return reply(s.page(req, "prompts", s.prompts, func(n string) string { return `{"name":"` + n + `","description":"d"}` }))
	case mcpwire.MethodListResources:
		return reply(s.page(req, "resources", s.resources, func(u string) string { return `{"name":"r","uri":"` + u + `"}` }))
	case mcpwire.MethodListResourceTemplates:
		return reply(`{"resultType":"complete","resourceTemplates":[{"name":"t","uriTemplate":"repo://{owner}/{name}"}],"ttlMs":60000,"cacheScope":"private"}`)
	case mcpwire.MethodCallTool:
		return s.call(req, reply)
	case mcpwire.MethodGetPrompt:
		return reply(`{"resultType":"complete","messages":[{"role":"user","content":{"type":"resource","resource":{"uri":"file:///p.txt","text":"x"}}}]}`)
	case mcpwire.MethodReadResource:
		uri, _ := stringMember(req.Params(), "uri")
		return reply(`{"resultType":"complete","contents":[{"uri":` + string(quoted(uri)) + `,"text":"body"}],"ttlMs":0,"cacheScope":"private"}`)
	case mcpwire.MethodComplete:
		return reply(`{"resultType":"complete","completion":{"values":["main"]}}`)
	case mcpwire.MethodListen:
		return s.listen(ctx, req, id, emit)
	}
	return fmt.Errorf("fake %s: unexpected %s", s.name, req.Method())
}

func (s *fakeService) page(req mcpwire.Message, field string, names []string, item func(string) string) string {
	start := 0
	if cursor, ok := stringMember(req.Params(), "cursor"); ok {
		_, _ = fmt.Sscanf(cursor, "c%d", &start)
	}
	end := min(start+150, len(names))
	items := make([]string, 0, end-start)
	for _, n := range names[start:end] {
		items = append(items, item(n))
	}
	out := `{"resultType":"complete","` + field + `":[` + strings.Join(items, ",") + `],"ttlMs":` + fmt.Sprint(s.ttlMS) + `,"cacheScope":"private"`
	if end < len(names) {
		out += `,"nextCursor":"c` + fmt.Sprint(end) + `"`
	}
	return out + `}`
}

func (s *fakeService) call(req mcpwire.Message, reply func(string) error) error {
	name, _ := stringMember(req.Params(), "name")
	switch name {
	case "ask":
		if member(req.Params(), "requestState") == nil {
			return reply(`{"resultType":"input_required","requestState":"mesh-c1.ref","inputRequests":{"n":{"method":"elicitation/create","params":{"message":"Name?","requestedSchema":{"type":"object"}}}}}`)
		}
		return reply(`{"resultType":"complete","content":[{"type":"text","text":"done"}]}`)
	default:
		return reply(`{"resultType":"complete","content":[{"type":"text","text":"called ` + name + ` see file:///x"},{"type":"resource_link","uri":"file:///x","name":"x"}],"structuredContent":{"uri":"file:///untouched"},"x-extra":{"n":12345678901234567890},"_meta":{"io.modelcontextprotocol/serverInfo":{"name":"` + s.name + `","version":"1"}}}`)
	}
}

func (s *fakeService) listen(ctx context.Context, req mcpwire.Message, id json.RawMessage, emit func([]byte, bool) error) error {
	l := &fakeListen{params: member(req.Params(), "notifications"), events: make(chan string, 8), ctx: ctx}
	s.mu.Lock()
	s.listens = append(s.listens, l)
	delay, filter := s.ackDelay, s.ackFilter
	s.mu.Unlock()
	select {
	case <-time.After(delay):
	case <-ctx.Done():
		return fmt.Errorf("listen before ack: %w", ctx.Err())
	}
	ack := string(l.params)
	if filter != nil {
		ack = filter(l.params)
	}
	sub := `"_meta":{"io.modelcontextprotocol/subscriptionId":` + string(id) + `}`
	if err := emit([]byte(`{"jsonrpc":"2.0","method":"notifications/subscriptions/acknowledged","params":{"notifications":`+ack+`,`+sub+`}}`), false); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("listen: %w", ctx.Err())
		case ev := <-l.events:
			if ev == "end" {
				return emit([]byte(`{"jsonrpc":"2.0","id":`+string(id)+`,"result":{"resultType":"complete","_meta":{`+`"io.modelcontextprotocol/subscriptionId":`+string(id)+`}}}`), true)
			}
			if ev == "revoked" {
				return &mcpconnect.RouteFailure{Code: agentapi.AccessDenied, Message: "grant retired"}
			}
			if err := emit([]byte(ev), false); err != nil {
				return err
			}
		}
	}
}

func (s *fakeService) lastCall() json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[len(s.calls)-1]
}

type fakeCatalog struct {
	mu      sync.Mutex
	entries map[string][]CatalogEntry
	fail    map[string]error
}

func (c *fakeCatalog) Services(_ context.Context, hostID string) ([]CatalogEntry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.fail[hostID]; err != nil {
		return nil, err
	}
	return c.entries[hostID], nil
}

func (c *fakeCatalog) setFail(hostID string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fail[hostID] = err
}

type fakeNative struct{ names []string }

func (n fakeNative) Tools(groups []string) []json.RawMessage {
	out := []json.RawMessage{}
	for _, name := range n.names {
		out = append(out, json.RawMessage(`{"name":"`+name+`","inputSchema":{"type":"object"}}`))
	}
	return out
}

func (n fakeNative) Has(_ []string, name string) bool { return strings.HasPrefix(name, "mesh_") }

func (n fakeNative) Call(_ context.Context, req mcpwire.Message, emit Emit) error {
	reply, err := mcpwire.Complete(req, json.RawMessage(`{"content":[{"type":"text","text":"native"}]}`), json.RawMessage(`{"name":"mesh","version":"t"}`), 0)
	if err != nil {
		return fmt.Errorf("native reply: %w", err)
	}
	return emit(reply, true)
}

type harness struct {
	t        *testing.T
	gw       *Gateway
	store    *Store
	catalog  *fakeCatalog
	services map[routeKey]*fakeService
	now      time.Time
	mu       sync.Mutex
}

func (h *harness) clock() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now
}

func (h *harness) advance(d time.Duration) {
	h.mu.Lock()
	h.now = h.now.Add(d)
	h.mu.Unlock()
}

func newHarness(t *testing.T, services ...*fakeService) *harness {
	t.Helper()
	h := &harness{t: t, store: NewStore(t.TempDir()), catalog: &fakeCatalog{entries: map[string][]CatalogEntry{}, fail: map[string]error{}},
		services: map[routeKey]*fakeService{}, now: time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)}
	var sels []Selection
	for i, s := range services {
		s.t = t
		if s.caps == "" {
			s.caps = `{"tools":{"listChanged":true}}`
		}
		if s.ttlMS == 0 {
			s.ttlMS = 60000
		}
		sel := Selection{Alias: s.name, HostID: testHostID(t, byte(10*(i+1))), ServiceID: testServiceID(byte(i + 1)), ServiceName: s.name, Include: []string{}, Exclude: []string{}}
		sels = append(sels, sel)
		h.services[routeKey{sel.HostID, sel.ServiceID}] = s
		h.catalog.entries[sel.HostID] = append(h.catalog.entries[sel.HostID], CatalogEntry{ServiceID: sel.ServiceID, Name: s.name, Revision: 1, State: "ready"})
	}
	if _, err := h.store.Update(context.Background(), nil, func(f *File) error {
		f.Profiles["dev"] = Profile{Native: []string{"hosts"}, Services: sels}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	gw, err := New(Options{
		Profile: "dev", Store: h.store, Native: fakeNative{names: []string{"mesh_hosts_list", "mesh_skill"}}, Catalog: h.catalog,
		Upstream: func(sel Selection) Upstream {
			return h.services[routeKey{sel.HostID, sel.ServiceID}]
		},
		CursorSecret: []byte("caller"), Now: h.clock, ListenSetup: 300 * time.Millisecond, ProfileEvery: 20 * time.Millisecond, RefreshEvery: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.gw = gw
	return h
}

func (h *harness) profile() Profile {
	f, err := h.store.Load()
	if err != nil {
		h.t.Fatal(err)
	}
	return f.Profiles["dev"]
}

func request(t *testing.T, id, method, params string) mcpwire.Message {
	t.Helper()
	if params != "" {
		params = "," + params
	}
	msg, fault := mcpwire.Parse([]byte(`{"jsonrpc":"2.0","id":` + id + `,"method":"` + method + `","params":{` + testMeta + params + `}}`))
	if fault != nil {
		t.Fatal(fault)
	}
	if f := mcpwire.ValidateRequest(msg, Methods); f != nil {
		t.Fatalf("%s: %v", method, f)
	}
	return msg
}

type recorded struct {
	mu       sync.Mutex
	messages []json.RawMessage
	terminal json.RawMessage
	signal   chan struct{}
}

func newRecorded() *recorded { return &recorded{signal: make(chan struct{}, 64)} }

func (r *recorded) emit(raw []byte, terminal bool) error {
	r.mu.Lock()
	if terminal {
		r.terminal = append(json.RawMessage(nil), raw...)
	} else {
		r.messages = append(r.messages, append(json.RawMessage(nil), raw...))
	}
	r.mu.Unlock()
	r.signal <- struct{}{}
	return nil
}

func (r *recorded) wait(t *testing.T, n int) []json.RawMessage {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		r.mu.Lock()
		if len(r.messages) >= n {
			out := append([]json.RawMessage(nil), r.messages...)
			r.mu.Unlock()
			return out
		}
		r.mu.Unlock()
		select {
		case <-r.signal:
		case <-deadline:
			t.Fatalf("waited for %d notifications", n)
		}
	}
}

func (h *harness) do(id, method, params string) json.RawMessage {
	h.t.Helper()
	rec := newRecorded()
	if err := h.gw.Forward(context.Background(), request(h.t, id, method, params), nil, rec.emit); err != nil {
		h.t.Fatalf("%s: %v", method, err)
	}
	if rec.terminal == nil {
		h.t.Fatalf("%s: no terminal reply", method)
	}
	return rec.terminal
}

func names(t *testing.T, reply json.RawMessage, field, key string) []string {
	t.Helper()
	items, err := decodeArray(member(reply, "result", field))
	if err != nil {
		t.Fatalf("%s: %v in %s", field, err, reply)
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		v, _ := stringMember(item, key)
		out = append(out, v)
	}
	return out
}

func TestGatewayListsNativeFirstThenServicesInProfileOrder(t *testing.T) {
	alpha := &fakeService{name: "alpha", tools: []string{"echo", "git_log", "git_push"}}
	beta := &fakeService{name: "beta", tools: []string{"echo"}}
	h := newHarness(t, alpha, beta)
	_, err := h.store.Update(context.Background(), nil, func(f *File) error {
		p := f.Profiles["dev"]
		p.Services[0].Exclude = []string{"git_push"}
		f.Profiles["dev"] = p
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	reply := h.do("1", mcpwire.MethodListTools, "")
	got := names(t, reply, "tools", "name")
	if len(got) != 5 || got[0] != "mesh_hosts_list" || got[1] != "mesh_skill" {
		t.Fatalf("order %v", got)
	}
	profile := h.profile()
	for i, want := range []Identity{
		{profile.Services[0].HostID, profile.Services[0].ServiceID, KindTool, "echo"},
		{profile.Services[0].HostID, profile.Services[0].ServiceID, KindTool, "git_log"},
		{profile.Services[1].HostID, profile.Services[1].ServiceID, KindTool, "echo"},
	} {
		id, err := DecodeName(got[i+2])
		if err != nil || id != want {
			t.Fatalf("entry %d %q = %v %v, want %v", i+2, got[i+2], id, err, want)
		}
	}
	tools, _ := decodeArray(member(reply, "result", "tools"))
	if string(member(tools[2], "x-vendor")) != `{"keep":[1,2.50,"z"]}` || member(tools[2], "annotations", "readOnlyHint") == nil {
		t.Fatalf("unknown fields not kept: %s", tools[2])
	}
	if alias, _ := stringMember(tools[4], "_meta", MetaRoute, "alias"); alias != "beta" {
		t.Fatalf("route meta %s", tools[4])
	}
	if ttl := string(member(reply, "result", "ttlMs")); ttl != "60000" {
		t.Fatalf("ttl %s", ttl)
	}
	again := names(t, h.do("2", mcpwire.MethodListTools, ""), "tools", "name")
	if !reflect.DeepEqual(again, got) {
		t.Fatalf("order changed between lists: %v vs %v", again, got)
	}
	if n := len(alpha.calls); n != 2 {
		t.Fatalf("a fresh cached listing was fetched again (%d upstream requests)", n)
	}
}

func TestGatewayPagesAreBoundToTheListing(t *testing.T) {
	many := make([]string, 450)
	for i := range many {
		many[i] = fmt.Sprintf("tool%03d", i)
	}
	h := newHarness(t, &fakeService{name: "big", tools: many})
	var all []string
	cursor := ""
	for page := 0; ; page++ {
		params := ""
		if cursor != "" {
			params = `"cursor":` + string(quoted(cursor))
		}
		reply := h.do(fmt.Sprint(page+1), mcpwire.MethodListTools, params)
		got := names(t, reply, "tools", "name")
		if len(got) > PageSize {
			t.Fatalf("page of %d", len(got))
		}
		all = append(all, got...)
		next, ok := stringMember(reply, "result", "nextCursor")
		if !ok {
			break
		}
		cursor = next
	}
	if len(all) != 452 {
		t.Fatalf("listed %d entries", len(all))
	}
	first := h.do("9", mcpwire.MethodListTools, "")
	cursor, _ = stringMember(first, "result", "nextCursor")
	if fault := h.do("10", mcpwire.MethodListPrompts, `"cursor":`+string(quoted(cursor))); member(fault, "error", "code") == nil {
		t.Fatalf("a tools cursor paged prompts: %s", fault)
	}
	if _, err := h.store.Update(context.Background(), nil, func(f *File) error {
		p := f.Profiles["dev"]
		p.Services[0].Exclude = []string{"tool000"}
		f.Profiles["dev"] = p
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	stale := h.do("11", mcpwire.MethodListTools, `"cursor":`+string(quoted(cursor)))
	if string(member(stale, "error", "code")) != "-32602" {
		t.Fatalf("a cursor survived a profile change: %s", stale)
	}
}

func TestGatewayRejectsDuplicateUpstreamNames(t *testing.T) {
	h := newHarness(t, &fakeService{name: "dup", tools: []string{"a", "b", "a"}}, &fakeService{name: "ok", tools: []string{"a"}})
	got := names(t, h.do("1", mcpwire.MethodListTools, ""), "tools", "name")
	if len(got) != 3 {
		t.Fatalf("malformed listing was exposed: %v", got)
	}
	st := h.gw.Statuses()
	if st[0].State != agentapi.GatewayFailed || !strings.Contains(st[0].Message, "twice") || st[1].State != agentapi.GatewayReady {
		t.Fatalf("statuses %+v", st)
	}
}

func TestGatewayCallsWithoutAPriorList(t *testing.T) {
	svc := &fakeService{name: "repo", tools: []string{"echo"}}
	h := newHarness(t, svc)
	sel := h.profile().Services[0]
	name, _, _ := EncodedName(Identity{sel.HostID, sel.ServiceID, KindTool, "echo"})
	reply := h.do(`"call-1"`, mcpwire.MethodCallTool, `"name":"`+name+`","arguments":{"text":"hi"}`)
	if got, _ := stringMember(svc.lastCall(), "params", "name"); got != "echo" {
		t.Fatalf("upstream saw name %q", got)
	}
	if string(member(svc.lastCall(), "params", "arguments")) != `{"text":"hi"}` {
		t.Fatalf("arguments changed: %s", svc.lastCall())
	}
	content, _ := decodeArray(member(reply, "result", "content"))
	link, _ := stringMember(content[1], "uri")
	if h2, s2, original, err := DecodeURI(link, false); err != nil || h2 != sel.HostID || s2 != sel.ServiceID || original != "file:///x" {
		t.Fatalf("resource link %q", link)
	}
	if text, _ := stringMember(content[0], "text"); !strings.Contains(text, "file:///x") {
		t.Fatalf("text was rewritten: %q", text)
	}
	if string(member(reply, "result", "structuredContent")) != `{"uri":"file:///untouched"}` || string(member(reply, "result", "x-extra")) != `{"n":12345678901234567890}` {
		t.Fatalf("unowned fields changed: %s", reply)
	}
	if up, _ := stringMember(reply, "result", "_meta", mcpconnect.MetaUpstreamServerInfo, "name"); up != "repo" {
		t.Fatalf("upstream identity lost: %s", reply)
	}
	if got, _ := stringMember(reply, "id"); got != "call-1" {
		t.Fatalf("id %s", member(reply, "id"))
	}
	unknown := h.do("2", mcpwire.MethodCallTool, `"name":"h_nope","arguments":{}`)
	if string(member(unknown, "error", "code")) != "-32602" {
		t.Fatalf("unknown name: %s", unknown)
	}
	other, _, _ := EncodedName(Identity{testHostID(t, 99), sel.ServiceID, KindTool, "echo"})
	if string(member(h.do("3", mcpwire.MethodCallTool, `"name":"`+other+`","arguments":{}`), "error", "code")) != "-32602" {
		t.Fatal("a name outside the profile was routed")
	}
	native := h.do("4", mcpwire.MethodCallTool, `"name":"mesh_hosts_list","arguments":{}`)
	if text, _ := stringMember(member(native, "result", "content"), ""); text != "" || !strings.Contains(string(native), "native") {
		t.Fatalf("native call %s", native)
	}
}

func TestGatewayRoutesResourcesPromptsAndCompletions(t *testing.T) {
	svc := &fakeService{name: "docs", prompts: []string{"review"}, resources: []string{"file:///a%20b.md"}, caps: `{"prompts":{},"resources":{"subscribe":true},"completions":{}}`}
	h := newHarness(t, svc)
	sel := h.profile().Services[0]
	discover := h.do("1", mcpwire.MethodDiscover, "")
	if member(discover, "result", "capabilities", "prompts") == nil || string(member(discover, "result", "capabilities", "resources", "subscribe")) != "true" ||
		member(discover, "result", "capabilities", "completions") == nil || member(discover, "result", "capabilities", "extensions") != nil {
		t.Fatalf("discover %s", discover)
	}
	uris := names(t, h.do("2", mcpwire.MethodListResources, ""), "resources", "uri")
	if want, _ := EncodeURI(sel.HostID, sel.ServiceID, "file:///a%20b.md"); len(uris) != 1 || uris[0] != want {
		t.Fatalf("resources %v", uris)
	}
	// A resource that was never listed still resolves exactly.
	unlisted, _ := EncodeURI(sel.HostID, sel.ServiceID, "file:///never%2Flisted?x=1")
	read := h.do("3", mcpwire.MethodReadResource, `"uri":`+string(quoted(unlisted)))
	if got, _ := stringMember(svc.lastCall(), "params", "uri"); got != "file:///never%2Flisted?x=1" {
		t.Fatalf("upstream read %q", got)
	}
	contents, _ := decodeArray(member(read, "result", "contents"))
	if got, _ := stringMember(contents[0], "uri"); got != unlisted {
		t.Fatalf("read reply uri %q", got)
	}
	templates := names(t, h.do("4", mcpwire.MethodListResourceTemplates, ""), "resourceTemplates", "uriTemplate")
	expanded := strings.Replace(strings.Replace(templates[0], "{owner}", "me", 1), "{name}", "repo", 1)
	h.do("5", mcpwire.MethodReadResource, `"uri":`+string(quoted(expanded)))
	if got, _ := stringMember(svc.lastCall(), "params", "uri"); got != "repo://me/repo" {
		t.Fatalf("expanded template read %q", got)
	}
	prompt := names(t, h.do("6", mcpwire.MethodListPrompts, ""), "prompts", "name")[0]
	got := h.do("7", mcpwire.MethodGetPrompt, `"name":"`+prompt+`"`)
	messages, _ := decodeArray(member(got, "result", "messages"))
	if uri, _ := stringMember(messages[0], "content", "resource", "uri"); !strings.HasPrefix(uri, URIScheme) {
		t.Fatalf("embedded resource %s", got)
	}
	h.do("8", mcpwire.MethodComplete, `"ref":{"type":"ref/prompt","name":"`+prompt+`"},"argument":{"name":"branch","value":"m"}`)
	if name, _ := stringMember(svc.lastCall(), "params", "ref", "name"); name != "review" {
		t.Fatalf("completion ref %s", svc.lastCall())
	}
	h.do("9", mcpwire.MethodComplete, `"ref":{"type":"ref/resource","uri":`+string(quoted(templates[0]))+`},"argument":{"name":"owner","value":"m"}`)
	if uri, _ := stringMember(svc.lastCall(), "params", "ref", "uri"); uri != "repo://{owner}/{name}" {
		t.Fatalf("template completion ref %s", svc.lastCall())
	}
}

func TestGatewayContinuationsPassThroughEveryRound(t *testing.T) {
	svc := &fakeService{name: "ask", tools: []string{"ask"}}
	h := newHarness(t, svc)
	sel := h.profile().Services[0]
	name, _, _ := EncodedName(Identity{sel.HostID, sel.ServiceID, KindTool, "ask"})
	first := h.do("1", mcpwire.MethodCallTool, `"name":"`+name+`","arguments":{}`)
	if kind, _ := stringMember(first, "result", "resultType"); kind != "input_required" {
		t.Fatalf("first round %s", first)
	}
	state, _ := stringMember(first, "result", "requestState")
	second := h.do("2", mcpwire.MethodCallTool, `"name":"`+name+`","arguments":{},"requestState":`+string(quoted(state))+`,"inputResponses":{"n":{"action":"accept","content":{"name":"x"}}}`)
	if kind, _ := stringMember(second, "result", "resultType"); kind != "complete" {
		t.Fatalf("second round %s", second)
	}
	if got, _ := stringMember(svc.lastCall(), "params", "requestState"); got != state {
		t.Fatalf("the host reference was not passed through: %s", svc.lastCall())
	}
}

func TestGatewayStaleExpiredAndUnauthorized(t *testing.T) {
	svc := &fakeService{name: "repo", tools: []string{"echo"}}
	h := newHarness(t, svc)
	sel := h.profile().Services[0]
	h.do("1", mcpwire.MethodListTools, "")
	h.advance(2 * time.Minute)
	h.catalog.setFail(sel.HostID, &mcpconnect.RouteFailure{Code: agentapi.HostUnreachable, Retryable: true, Message: "offline"})
	stale := h.do("2", mcpwire.MethodListTools, "")
	tools, _ := decodeArray(member(stale, "result", "tools"))
	if len(tools) != 3 || string(member(tools[2], "_meta", MetaRoute, "stale")) != "true" || string(member(stale, "result", "ttlMs")) != "0" {
		t.Fatalf("stale listing %s", stale)
	}
	if st := h.gw.Statuses()[0]; st.State != agentapi.GatewayStale {
		t.Fatalf("status %+v", st)
	}
	h.advance(25 * time.Hour)
	expired := names(t, h.do("3", mcpwire.MethodListTools, ""), "tools", "name")
	if len(expired) != 2 || h.gw.Statuses()[0].State != agentapi.GatewayExpired {
		t.Fatalf("expired listing exposed: %v %+v", expired, h.gw.Statuses())
	}
	h.catalog.setFail(sel.HostID, nil)
	h.do("4", mcpwire.MethodListTools, "")
	h.advance(10 * time.Minute)
	h.catalog.setFail(sel.HostID, &mcpconnect.RouteFailure{Code: agentapi.AccessDenied, Message: "revoked"})
	denied := names(t, h.do("5", mcpwire.MethodListTools, ""), "tools", "name")
	if len(denied) != 2 || h.gw.Statuses()[0].State != agentapi.GatewayUnauthorized {
		t.Fatalf("unauthorized listing kept: %v %+v", denied, h.gw.Statuses())
	}
	h.catalog.setFail(sel.HostID, &mcpconnect.RouteFailure{Code: agentapi.HostUnreachable, Message: "offline"})
	if after := names(t, h.do("6", mcpwire.MethodListTools, ""), "tools", "name"); len(after) != 2 {
		t.Fatalf("a listing discarded on denial came back as stale: %v", after)
	}
}

func TestGatewayServiceReplacementGetsNewNames(t *testing.T) {
	svc := &fakeService{name: "repo", tools: []string{"echo"}}
	h := newHarness(t, svc)
	old := h.profile().Services[0]
	oldName := names(t, h.do("1", mcpwire.MethodListTools, ""), "tools", "name")[2]
	replacement := testServiceID(77)
	h.catalog.mu.Lock()
	h.catalog.entries[old.HostID] = []CatalogEntry{{ServiceID: replacement, Name: "repo", Revision: 1, State: "ready"}}
	h.catalog.mu.Unlock()
	h.advance(10 * time.Minute)
	if got := names(t, h.do("2", mcpwire.MethodListTools, ""), "tools", "name"); len(got) != 2 || h.gw.Statuses()[0].State != agentapi.GatewayNotFound {
		t.Fatalf("removed service still listed: %v %+v", got, h.gw.Statuses())
	}
	h.services[routeKey{old.HostID, replacement}] = svc
	if _, err := h.store.Update(context.Background(), nil, func(f *File) error {
		p := f.Profiles["dev"]
		p.Services[0].ServiceID = replacement
		f.Profiles["dev"] = p
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	newName := names(t, h.do("3", mcpwire.MethodListTools, ""), "tools", "name")[2]
	if newName == oldName {
		t.Fatal("a replacement service inherited its predecessor's name")
	}
	if string(member(h.do("4", mcpwire.MethodCallTool, `"name":"`+oldName+`","arguments":{}`), "error", "code")) != "-32602" {
		t.Fatal("the old name still routes")
	}
}

func TestGatewayAliasChangesDoNotChangeNames(t *testing.T) {
	h := newHarness(t, &fakeService{name: "repo", tools: []string{"echo"}})
	before := names(t, h.do("1", mcpwire.MethodListTools, ""), "tools", "name")
	if _, err := h.store.Update(context.Background(), nil, func(f *File) error {
		p := f.Profiles["dev"]
		p.Services[0].Alias = "code"
		f.Profiles["dev"] = p
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	after := names(t, h.do("2", mcpwire.MethodListTools, ""), "tools", "name")
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("alias change renamed tools: %v -> %v", before, after)
	}
}

func listenFor(t *testing.T, h *harness, params string) (*recorded, context.CancelFunc, chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	rec := newRecorded()
	done := make(chan error, 1)
	go func() { done <- h.gw.Forward(ctx, request(t, `"sub-1"`, mcpwire.MethodListen, params), nil, rec.emit) }()
	return rec, cancel, done
}

func waitListens(t *testing.T, s *fakeService, n int) []*fakeListen {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		got := append([]*fakeListen(nil), s.listens...)
		s.mu.Unlock()
		if len(got) >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s: waited for %d upstream listens", s.name, n)
	return nil
}

func TestGatewayListenAcknowledgesOnlyBackedFilters(t *testing.T) {
	alpha := &fakeService{name: "alpha", tools: []string{"echo"}, caps: `{"tools":{"listChanged":true},"resources":{"subscribe":true,"listChanged":true}}`}
	slow := &fakeService{name: "slow", tools: []string{"echo"}, caps: `{"resources":{"subscribe":true}}`, ackDelay: time.Hour}
	h := newHarness(t, alpha, slow)
	sels := h.profile().Services
	mine, _ := EncodeURI(sels[0].HostID, sels[0].ServiceID, "file:///a")
	refused, _ := EncodeURI(sels[0].HostID, sels[0].ServiceID, "file:///refused")
	slowURI, _ := EncodeURI(sels[1].HostID, sels[1].ServiceID, "file:///b")
	alpha.ackFilter = func(json.RawMessage) string { return `{"toolsListChanged":true,"resourceSubscriptions":["file:///a"]}` }
	rec, cancel, done := listenFor(t, h, `"notifications":{"toolsListChanged":true,"resourceSubscriptions":[`+string(quoted(mine))+`,`+string(quoted(refused))+`,`+string(quoted(slowURI))+`,"file:///not-gateway"]}`)
	ack := rec.wait(t, 1)[0]
	if method, _ := stringMember(ack, "method"); method != ackMethod {
		t.Fatalf("first message %s", ack)
	}
	if id, _ := stringMember(ack, "params", "_meta", mcpwire.SubscriptionID); id != "sub-1" {
		t.Fatalf("ack not tagged with the client's listen ID: %s", ack)
	}
	if got := string(member(ack, "params", "notifications")); got != `{"resourceSubscriptions":[`+string(quoted(mine))+`],"toolsListChanged":true}` {
		t.Fatalf("acknowledged %s", got)
	}
	upstream := waitListens(t, alpha, 1)[0]
	if got := string(upstream.params); !strings.Contains(got, `"file:///a"`) || strings.Contains(got, URIScheme) {
		t.Fatalf("upstream filter %s", got)
	}
	slowListen := waitListens(t, slow, 1)[0]
	select {
	case <-slowListen.ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("an unacknowledged upstream listen was kept open")
	}
	sub := `"_meta":{"io.modelcontextprotocol/subscriptionId":"sub-1"}`
	upstream.events <- `{"jsonrpc":"2.0","method":"notifications/resources/updated","params":{"uri":"file:///a","title":"kept",` + sub + `}}`
	upstream.events <- `{"jsonrpc":"2.0","method":"notifications/resources/updated","params":{"uri":"file:///refused",` + sub + `}}`
	upstream.events <- `{"jsonrpc":"2.0","method":"notifications/tools/list_changed","params":{` + sub + `}}`
	msgs := rec.wait(t, 3)
	var sawUpdate, sawTools bool
	for _, m := range msgs[1:] {
		method, _ := stringMember(m, "method")
		uri, _ := stringMember(m, "params", "uri")
		switch {
		case method == "notifications/resources/updated" && uri == mine && string(member(m, "params", "title")) == `"kept"`:
			sawUpdate = true
		case method == "notifications/tools/list_changed":
			sawTools = true
		default:
			t.Fatalf("unexpected event %s", m)
		}
	}
	if !sawUpdate || !sawTools {
		t.Fatalf("events %s", msgs)
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("a cancelled listen reported success")
	}
	select {
	case <-upstream.ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("client cancellation did not cancel the upstream listen")
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.terminal != nil {
		t.Fatalf("a cancelled listen got a reply: %s", rec.terminal)
	}
}

func TestGatewayListenSetupBoundCoversEverySilentUpstream(t *testing.T) {
	first := &fakeService{name: "first", tools: []string{"x"}, ackDelay: time.Hour}
	second := &fakeService{name: "second", tools: []string{"x"}, ackDelay: time.Hour}
	h := newHarness(t, first, second)
	rec, cancel, done := listenFor(t, h, `"notifications":{"toolsListChanged":true}`)
	defer func() {
		cancel()
		<-done
	}()
	ack := rec.wait(t, 1)[0]
	if got := string(member(ack, "params", "notifications")); got != `{"toolsListChanged":true}` {
		t.Fatalf("list changes are backed by refresh even without upstream acks: %s", got)
	}
	for _, s := range []*fakeService{first, second} {
		select {
		case <-waitListens(t, s, 1)[0].ctx.Done():
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: a silent upstream listen was kept open", s.name)
		}
	}
}

func TestGatewayListenEndsGracefullyOrOnRevocation(t *testing.T) {
	svc := &fakeService{name: "repo", tools: []string{"echo"}}
	h := newHarness(t, svc)
	rec, cancel, done := listenFor(t, h, `"notifications":{"toolsListChanged":true}`)
	defer cancel()
	rec.wait(t, 1)
	waitListens(t, svc, 1)[0].events <- "end"
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if kind, _ := stringMember(rec.terminal, "result", "resultType"); kind != "complete" {
		t.Fatalf("graceful end %s", rec.terminal)
	}
	if id, _ := stringMember(rec.terminal, "result", "_meta", mcpwire.SubscriptionID); id != "sub-1" {
		t.Fatalf("completion not correlated: %s", rec.terminal)
	}
	rec2, cancel2, done2 := listenFor(t, h, `"notifications":{"toolsListChanged":true}`)
	defer cancel2()
	rec2.wait(t, 1)
	waitListens(t, svc, 2)[1].events <- "revoked"
	err := <-done2
	var rf *mcpconnect.RouteFailure
	if !errors.As(err, &rf) || rf.Code != agentapi.AccessDenied {
		t.Fatalf("revocation ended with %v", err)
	}
	if rec2.terminal != nil {
		t.Fatalf("revoked listen delivered a reply: %s", rec2.terminal)
	}
}

func TestGatewayListenNoticesProfileChanges(t *testing.T) {
	svc := &fakeService{name: "repo", tools: []string{"echo"}}
	other := &fakeService{name: "other", tools: []string{"x"}, caps: `{"tools":{}}`}
	h := newHarness(t, svc, other)
	rec, cancel, done := listenFor(t, h, `"notifications":{"toolsListChanged":true}`)
	defer cancel()
	rec.wait(t, 1)
	if _, err := h.store.Update(context.Background(), nil, func(f *File) error {
		p := f.Profiles["dev"]
		p.Native = []string{}
		f.Profiles["dev"] = p
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	msgs := rec.wait(t, 2)
	if method, _ := stringMember(msgs[1], "method"); method != "notifications/tools/list_changed" {
		t.Fatalf("profile change event %s", msgs[1])
	}
	if _, err := h.store.Update(context.Background(), nil, func(f *File) error {
		p := f.Profiles["dev"]
		p.Services = p.Services[1:]
		f.Profiles["dev"] = p
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if kind, _ := stringMember(rec.terminal, "result", "resultType"); kind != "complete" {
		t.Fatalf("removing a held route ended with %s", rec.terminal)
	}
}

func TestGatewayIdentityChangeMapsToRouteCode(t *testing.T) {
	svc := &fakeService{name: "repo", tools: []string{"echo"}}
	h := newHarness(t, svc)
	sel := h.profile().Services[0]
	name, _, _ := EncodedName(Identity{sel.HostID, sel.ServiceID, KindTool, "echo"})
	svc.fail = &mcpconnect.RouteFailure{Code: agentapi.HostIdentityMismatch, Host: sel.HostID, Message: "pin changed"}
	err := h.gw.Forward(context.Background(), request(t, "1", mcpwire.MethodCallTool, `"name":"`+name+`","arguments":{}`), nil, newRecorded().emit)
	var rf *mcpconnect.RouteFailure
	if !errors.As(err, &rf) || rf.Code != agentapi.RouteIdentityChanged || rf.Service != "repo" {
		t.Fatalf("identity change: %v", err)
	}
	if h.gw.Statuses()[0].State != agentapi.GatewayIdentityChanged {
		t.Fatalf("status %+v", h.gw.Statuses())
	}
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
