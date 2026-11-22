package mcpgateway

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/mcpconnect"
	"github.com/shaul/mesh/internal/mcpwire"
)

func (s *fakeService) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (h *harness) edit(change func(p *Profile)) {
	h.t.Helper()
	if _, err := h.store.Update(context.Background(), nil, func(f *File) error {
		p := f.Profiles["dev"]
		change(&p)
		f.Profiles["dev"] = p
		return nil
	}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) forward(id, method, params string) error {
	h.t.Helper()
	return h.gw.Forward(context.Background(), request(h.t, id, method, params), nil, newRecorded().emit)
}

func hostedNames(t *testing.T, reply json.RawMessage) []string {
	t.Helper()
	var out []string
	for _, n := range names(t, reply, "tools", "name") {
		if IsHostedName(n) {
			out = append(out, n)
		}
	}
	return out
}

func routeState(t *testing.T, reply json.RawMessage) Status {
	t.Helper()
	var statuses []Status
	if err := json.Unmarshal(member(reply, "result", "_meta", MetaRoutes), &statuses); err != nil {
		t.Fatalf("route states: %v in %s", err, reply)
	}
	for _, st := range statuses {
		if st.Alias == "repo" {
			return st
		}
	}
	t.Fatalf("no state for repo in %s", reply)
	return Status{}
}

// Review 1: without a verified identity, no hosted route is used, even
// after a profile edit adds one.
func TestHostedRoutesNeedAnIdentity(t *testing.T) {
	svc := &fakeService{name: "repo", tools: []string{"echo"}}
	h := newHarness(t, svc)
	h.hosted = false
	h.open()
	reply := h.do("1", mcpwire.MethodListTools, "")
	if got := hostedNames(t, reply); len(got) != 0 {
		t.Fatalf("an identity-free frontend listed hosted tools: %v", got)
	}
	if st := routeState(t, reply); st.State != agentapi.GatewayUnauthorized {
		t.Fatalf("state %+v", st)
	}
	sel := h.profile().Services[0]
	name, _, _ := EncodedName(Identity{sel.HostID, sel.ServiceID, KindTool, "echo"})
	err := h.forward("2", mcpwire.MethodCallTool, `"name":"`+name+`","arguments":{}`)
	var rf *mcpconnect.RouteFailure
	if !errors.As(err, &rf) || rf.Code != agentapi.AccessDenied {
		t.Fatalf("an identity-free call: %v", err)
	}
	other := testServiceID(9)
	h.catalog.entries[sel.HostID] = append(h.catalog.entries[sel.HostID], CatalogEntry{ServiceID: other, Name: "late", Revision: 1, State: "ready"})
	h.edit(func(p *Profile) {
		p.Services = append(p.Services, Selection{Alias: "late", HostID: sel.HostID, ServiceID: other, Include: []string{}, Exclude: []string{}})
	})
	if got := hostedNames(t, h.do("3", mcpwire.MethodListTools, "")); len(got) != 0 || svc.callCount() != 0 {
		t.Fatalf("a profile edit enabled hosted routes without an identity: %v, %d upstream calls", got, svc.callCount())
	}
}

// Review 2: every hosted call is bound to the revision and account the
// gateway resolved, and a stale binding retires the cached listing.
func TestCallsCarryTheResolvedServiceState(t *testing.T) {
	svc := &fakeService{name: "repo", tools: []string{"echo"}}
	h := newHarness(t, svc)
	sel := h.profile().Services[0]
	name, _, _ := EncodedName(Identity{sel.HostID, sel.ServiceID, KindTool, "echo"})
	h.do("1", mcpwire.MethodCallTool, `"name":"`+name+`","arguments":{}`)
	if e := svc.lastExpect(); e == nil || e.Revision != 1 {
		t.Fatalf("a call without a listing was not bound: %+v", e)
	}
	h.do("2", mcpwire.MethodListTools, "")
	h.catalog.setRevision(sel.HostID, sel.ServiceID, 2)
	svc.fail = &mcpconnect.RouteFailure{Code: agentapi.CatalogStale, Retryable: true, Message: "moved on"}
	if err := h.forward("3", mcpwire.MethodCallTool, `"name":"`+name+`","arguments":{}`); err == nil {
		t.Fatal("a refused stale call succeeded")
	}
	svc.fail = nil
	h.do("4", mcpwire.MethodListTools, "")
	if e := svc.lastExpect(); e.Revision != 2 {
		t.Fatalf("the listing after a stale refusal used revision %d", e.Revision)
	}
}

// Review 3: a changed instance pin takes effect at once, for calls,
// continuations and listings.
func TestInstancePinChangesTakeEffect(t *testing.T) {
	a := &fakeService{name: "a", tools: []string{"ask"}}
	h := newHarness(t, a)
	sel := h.profile().Services[0]
	b := &fakeService{name: "b", tools: []string{"ask"}, caps: a.caps, ttlMS: 60000, t: t}
	h.services[routeKey{sel.HostID, sel.ServiceID, "inst-a"}] = a
	h.services[routeKey{sel.HostID, sel.ServiceID, "inst-b"}] = b
	h.edit(func(p *Profile) { p.Services[0].Instance = "inst-a" })
	name, _, _ := EncodedName(Identity{sel.HostID, sel.ServiceID, KindTool, "ask"})
	first := h.do("1", mcpwire.MethodCallTool, `"name":"`+name+`","arguments":{}`)
	state, _ := stringMember(first, "result", "requestState")
	h.do("2", mcpwire.MethodListTools, "")
	before := a.callCount()
	h.edit(func(p *Profile) { p.Services[0].Instance = "inst-b" })
	h.do("3", mcpwire.MethodCallTool, `"name":"`+name+`","arguments":{},"requestState":`+string(quoted(state))+`,"inputResponses":{"n":{"action":"accept","content":{"name":"x"}}}`)
	h.do("4", mcpwire.MethodListTools, "")
	if a.callCount() != before || b.callCount() < 2 {
		t.Fatalf("the old pin still served after the edit: a %d -> %d, b %d", before, a.callCount(), b.callCount())
	}
}

// Review 5: a warm listing is served only under the caller's current
// authorization and scope.
func TestWarmListingsRecheckAuthorization(t *testing.T) {
	svc := &fakeService{name: "repo", tools: []string{"echo"}}
	h := newHarness(t, svc)
	sel := h.profile().Services[0]
	h.do("1", mcpwire.MethodListTools, "")
	h.catalog.setFail(sel.HostID, &mcpconnect.RouteFailure{Code: agentapi.AccessDenied, Message: "revoked"})
	if got := hostedNames(t, h.do("2", mcpwire.MethodListTools, "")); len(got) != 0 {
		t.Fatalf("a warm listing outlived revocation: %v", got)
	}
	h.catalog.setFail(sel.HostID, nil)
	h.do("3", mcpwire.MethodListTools, "")
	fetches := svc.callCount()
	h.catalog.setScope(sel.HostID, "scope-2")
	h.do("4", mcpwire.MethodListTools, "")
	if svc.callCount() == fetches {
		t.Fatal("a listing from the replaced grant was reused after re-approval")
	}
}

// Review 5: one caller's denial drops only that caller's persisted records.
func TestFileCacheDropsOnlyOneCallersRecords(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	host := testHostID(t, 3)
	mine := NewFileCache(dir, func(string) string { return "pin\x00app:mine" })
	theirs := NewFileCache(dir, func(string) string { return "pin\x00app:theirs" })
	l := &Listing{HostID: host, ServiceID: testServiceID(1), Method: mcpwire.MethodListTools, FetchedAt: time.Now(), TTL: time.Minute}
	mine.Save(l)
	theirs.Save(l)
	mine.DropHost(host)
	if _, ok := mine.Load(host, l.ServiceID, "", l.Method, ""); ok {
		t.Fatal("the dropped caller still loads its record")
	}
	if _, ok := theirs.Load(host, l.ServiceID, "", l.Method, ""); !ok {
		t.Fatal("dropping one caller's records removed another caller's")
	}
	repinned := NewFileCache(dir, func(string) string { return "new-pin\x00app:theirs" })
	if _, ok := repinned.Load(host, l.ServiceID, "", l.Method, ""); ok {
		t.Fatal("a re-pinned host read a record of its old pin")
	}
	theirs.Expire(host, l.ServiceID, "")
	if got, ok := theirs.Load(host, l.ServiceID, "", l.Method, ""); !ok || got.TTL != 0 {
		t.Fatalf("expire left the persisted record fresh: %+v", got)
	}
}

// Review 6: a cursor fails after any profile edit, even one reverted, and
// after a new service revision with an identical schema.
func TestCursorsBindRevisions(t *testing.T) {
	many := make([]string, 450)
	for i := range many {
		many[i] = "t" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + strings.Repeat("y", i/26)
	}
	h := newHarness(t, &fakeService{name: "big", tools: many})
	sel := h.profile().Services[0]
	cursorOf := func(id string) string {
		c, _ := stringMember(h.do(id, mcpwire.MethodListTools, ""), "result", "nextCursor")
		return c
	}
	invalid := func(id, cursor string) bool {
		return string(member(h.do(id, mcpwire.MethodListTools, `"cursor":`+string(quoted(cursor))), "error", "code")) == "-32602"
	}
	cursor := cursorOf("1")
	h.edit(func(p *Profile) { p.Services[0].Exclude = []string{"zzz"} })
	h.edit(func(p *Profile) { p.Services[0].Exclude = []string{} })
	if !invalid("2", cursor) {
		t.Fatal("a cursor survived an edit and its revert")
	}
	cursor = cursorOf("3")
	h.catalog.setRevision(sel.HostID, sel.ServiceID, 2)
	if !invalid("4", cursor) {
		t.Fatal("a cursor survived a new service revision with the same schema")
	}
	cursor = cursorOf("5")
	h.advance(25 * time.Hour)
	h.catalog.setFail(sel.HostID, &mcpconnect.RouteFailure{Code: agentapi.HostUnreachable, Message: "offline"})
	if !invalid("6", cursor) {
		t.Fatal("a cursor kept reading an expired listing")
	}
}

// Review 12: replies say whether a route is offline, stale or expired.
func TestRepliesDistinguishOfflineStaleAndExpired(t *testing.T) {
	h := newHarness(t, &fakeService{name: "repo", tools: []string{"echo"}})
	sel := h.profile().Services[0]
	h.cache = NewFileCache(filepath.Join(t.TempDir(), "cache"), func(string) string { return "scope" })
	h.open()
	h.do("1", mcpwire.MethodListTools, "")
	h.catalog.setFail(sel.HostID, &mcpconnect.RouteFailure{Code: agentapi.HostUnreachable, Message: "offline"})
	h.advance(time.Hour)
	if st := routeState(t, h.do("2", mcpwire.MethodListTools, "")); st.State != agentapi.GatewayStale || st.Freshness != freshStale {
		t.Fatalf("stale: %+v", st)
	}
	h.advance(25 * time.Hour)
	h.open()
	h.catalog.setFail(sel.HostID, nil)
	h.do("3", mcpwire.MethodDiscover, "")
	h.catalog.setFail(sel.HostID, &mcpconnect.RouteFailure{Code: agentapi.HostUnreachable, Message: "offline"})
	if st := routeState(t, h.do("4", mcpwire.MethodListTools, "")); st.State != agentapi.GatewayExpired || st.Freshness != freshExpired {
		t.Fatalf("a fresh frontend lost the expired state: %+v", st)
	}
	h.cache = nil
	h.open()
	if st := routeState(t, h.do("5", mcpwire.MethodListTools, "")); st.State != agentapi.GatewayOffline {
		t.Fatalf("offline without a cache: %+v", st)
	}
}

// Review 7: the setup bound covers discovery; a declined route never ends
// a listen another route backs; an event right after an acknowledgment is
// delivered after the downstream acknowledgment.
func TestListenSetupAndDeclinedRoutes(t *testing.T) {
	silent := &fakeService{name: "silent", tools: []string{"x"}, discoverDelay: time.Hour}
	declined := &fakeService{name: "declined", tools: []string{"x"}, ackFilter: func(json.RawMessage) string { return `{}` }, endAfterAck: true}
	quick := &fakeService{name: "quick", tools: []string{"x"}, caps: `{"resources":{"subscribe":true}}`,
		afterAck: `{"jsonrpc":"2.0","method":"notifications/resources/updated","params":{"uri":"file:///a",SUB}}`}
	slow := &fakeService{name: "slow", tools: []string{"x"}, ackDelay: 150 * time.Millisecond}
	h := newHarness(t, silent, declined, quick, slow)
	sels := h.profile().Services
	uri, _ := EncodeURI(sels[2].HostID, sels[2].ServiceID, "file:///a")
	quick.ackFilter = func(json.RawMessage) string { return `{"resourceSubscriptions":["file:///a"]}` }
	start := time.Now()
	rec, cancel, done := listenFor(t, h, `"notifications":{"toolsListChanged":true,"resourceSubscriptions":[`+string(quoted(uri))+`]}`)
	defer cancel()
	ack := rec.wait(t, 1)[0]
	if time.Since(start) > 5*time.Second {
		t.Fatal("a silent discovery held the acknowledgment past the setup bound")
	}
	if got := string(member(ack, "params", "notifications", "resourceSubscriptions")); got != `[`+string(quoted(uri))+`]` {
		t.Fatalf("acknowledged %s", ack)
	}
	msgs := rec.wait(t, 2)
	if got, _ := stringMember(msgs[1], "params", "uri"); got != uri {
		t.Fatalf("the immediate event was lost: %s", msgs)
	}
	select {
	case err := <-done:
		t.Fatalf("a declined route ended a healthy listen: %v %s", err, rec.terminal)
	case <-time.After(300 * time.Millisecond):
	}
}

// Review 8: a listen covered only by refresh ends when authorization goes;
// a template-only change is reported; revocation retires queued output.
func TestRefreshOnlyListens(t *testing.T) {
	svc := &fakeService{name: "repo", tools: []string{"echo"}, caps: `{"resources":{}}`, resources: []string{"file:///r"}}
	h := newHarness(t, svc)
	h.catalog.unsupported = true
	h.refresh = 30 * time.Millisecond
	h.open()
	sel := h.profile().Services[0]
	h.do("0", mcpwire.MethodListResourceTemplates, "")
	var retired sync.WaitGroup
	retired.Add(1)
	ctx, cancel := context.WithCancel(WithRetire(context.Background(), retired.Done))
	defer cancel()
	rec := newRecorded()
	done := make(chan error, 1)
	go func() {
		done <- h.gw.Forward(ctx, request(t, `"sub"`, mcpwire.MethodListen, `"notifications":{"resourcesListChanged":true}`), nil, rec.emit)
	}()
	rec.wait(t, 1)
	svc.mu.Lock()
	svc.template = "repo://{owner}/{other}"
	svc.mu.Unlock()
	stop := h.tick(time.Hour)
	msgs := rec.wait(t, 2)
	stop()
	if method, _ := stringMember(msgs[1], "method"); method != "notifications/resources/list_changed" {
		t.Fatalf("a template change was not reported: %s", msgs[1])
	}
	h.catalog.setFail(sel.HostID, &mcpconnect.RouteFailure{Code: agentapi.AccessDenied, Message: "revoked"})
	stopRevoked := h.tick(time.Hour)
	defer stopRevoked()
	select {
	case err := <-done:
		var rf *mcpconnect.RouteFailure
		if !errors.As(err, &rf) || rf.Code != agentapi.AccessDenied {
			t.Fatalf("a refresh-only listen ended with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a refresh-only listen outlived revocation")
	}
	retired.Wait()
}

// Review 14: a host's published listing revision reaches listens and
// retires the cached listing.
func TestHostListingRevisionsReachListens(t *testing.T) {
	svc := &fakeService{name: "repo", tools: []string{"echo"}, caps: `{"tools":{}}`}
	h := newHarness(t, svc)
	sel := h.profile().Services[0]
	h.do("1", mcpwire.MethodListTools, "")
	fetches := svc.callCount()
	rec, cancel, _ := listenFor(t, h, `"notifications":{"toolsListChanged":true}`)
	defer cancel()
	rec.wait(t, 1)
	deadline := time.Now().Add(5 * time.Second)
	for !h.catalog.watching(sel.HostID) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	h.catalog.publish(t, sel.HostID, sel.ServiceID)
	if method, _ := stringMember(rec.wait(t, 2)[1], "method"); method != "notifications/tools/list_changed" {
		t.Fatal("a published listing revision was not reported")
	}
	h.do("2", mcpwire.MethodListTools, "")
	if svc.callCount() == fetches {
		t.Fatal("the listing was not fetched again after its revision changed")
	}
}

// Review 9: an update that fits the message cap upstream but not after the
// URI rewrite ends only its own stream.
func TestOversizedUpdateEndsOnlyItsStream(t *testing.T) {
	svc := &fakeService{name: "repo", tools: []string{"echo"}, caps: `{"resources":{"subscribe":true}}`,
		ackFilter: func(json.RawMessage) string { return `{"resourceSubscriptions":["file:///a"]}` }}
	h := newHarness(t, svc)
	sel := h.profile().Services[0]
	uri, _ := EncodeURI(sel.HostID, sel.ServiceID, "file:///a")
	rec, cancel, done := listenFor(t, h, `"notifications":{"resourceSubscriptions":[`+string(quoted(uri))+`]}`)
	defer cancel()
	rec.wait(t, 1)
	event := `{"jsonrpc":"2.0","method":"notifications/resources/updated","params":{"uri":"file:///a","x-pad":"PAD","_meta":{"io.modelcontextprotocol/subscriptionId":"sub-1"}}}`
	event = strings.Replace(event, "PAD", strings.Repeat("p", mcpwire.MaxMessage-len(event)), 1)
	waitListens(t, svc, 1)[0].events <- event
	var rf *mcpconnect.RouteFailure
	if err := <-done; !errors.As(err, &rf) || rf.Code != agentapi.MCPMessageTooLarge {
		t.Fatalf("oversized update: %v", err)
	}
	h.do("2", mcpwire.MethodListTools, "")
}

// Review 10: unowned request parameters, page fields and list-change
// fields pass through; malformed URIs never do.
func TestAggregationKeepsUnownedFields(t *testing.T) {
	svc := &fakeService{name: "repo", tools: []string{"echo"}, caps: `{"tools":{"listChanged":true},"resources":{}}`,
		resources: []string{"file:///ok", "file:///bad%ZZ"}, pageExtra: `,"x-page":{"n":1},"_meta":{"vendor.example/page":"p"}`}
	h := newHarness(t, svc)
	sel := h.profile().Services[0]
	reply := h.do("1", mcpwire.MethodListTools, `"x-filter":"a"`)
	if string(member(svc.lastCall(), "params", "x-filter")) != `"a"` {
		t.Fatalf("an unowned list parameter was dropped: %s", svc.lastCall())
	}
	var extras []upstreamExtras
	_ = json.Unmarshal(member(reply, "result", "_meta", MetaUpstreams), &extras)
	if len(extras) != 1 || string(member(extras[0].Fields, "x-page")) != `{"n":1}` || member(extras[0].Fields, "_meta", "vendor.example/page") == nil {
		t.Fatalf("page-level fields were dropped: %s", reply)
	}
	uris := names(t, h.do("2", mcpwire.MethodListResources, ""), "resources", "uri")
	if len(uris) != 1 || !strings.HasSuffix(uris[0], "file:///ok") {
		t.Fatalf("a malformed resource URI was exposed: %v", uris)
	}
	bad, _ := EncodeURI(sel.HostID, sel.ServiceID, "file:///bad%ZZ")
	if string(member(h.do("3", mcpwire.MethodReadResource, `"uri":`+string(quoted(bad))), "error", "code")) != "-32602" {
		t.Fatal("a malformed wrapped URI was routed")
	}
	for _, template := range []string{"   ", "repo://{a", "repo://a}", "x y://z"} {
		if validResourceURI(template, true) {
			t.Fatalf("template %q accepted", template)
		}
	}
	rec, cancel, _ := listenFor(t, h, `"notifications":{"toolsListChanged":true}`)
	defer cancel()
	rec.wait(t, 1)
	waitListens(t, svc, 1)[0].events <- `{"jsonrpc":"2.0","method":"notifications/tools/list_changed","params":{"x-why":"rebuilt","_meta":{"io.modelcontextprotocol/subscriptionId":"sub-1"}}}`
	if got := string(member(rec.wait(t, 2)[1], "params", "x-why")); got != `"rebuilt"` {
		t.Fatalf("a list change lost its fields: %s", got)
	}
}

// tick advances the fake clock by step every few milliseconds until the
// returned stop is called, so pollers waiting on it make progress.
func (h *harness) tick(step time.Duration) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
				h.advance(step)
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}
