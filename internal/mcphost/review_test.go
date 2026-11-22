package mcphost

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shaul/mesh/internal/mcpwire"
)

// A listen whose acknowledgment backs no filter watches nothing, so it must
// not keep an on-demand instance alive.
func TestEmptyListenDoesNotHoldInstance(t *testing.T) {
	f := newSupervisorFixture(t)
	f.publish(t, Definition{Name: "repo", Idle: 50 * time.Millisecond})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	events := make(chan string, 4)
	// The fixture backs only toolsListChanged, so this acknowledgment is empty.
	listen := parseRequest(t, `{"jsonrpc":"2.0","id":"L","method":"subscriptions/listen","params":{"notifications":{"promptsListChanged":true},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`)
	go func() {
		_ = f.sup.Forward(ctx, caller("a"), Target{Service: "repo"}, listen, func(raw []byte, _ bool) error { events <- string(raw); return nil })
	}()
	if ack := waitEvent(t, events); string(member([]byte(ack), "params", "notifications")) != `{}` {
		t.Fatalf("fixture acknowledgment: %s", ack)
	}
	eventually(t, func() bool { return f.sup.Services(caller("a"))[0].Instances[0].State == StateStopped })
}

func (f *fakeLauncher) live() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	live := 0
	for _, w := range f.workers {
		select {
		case <-w.ended:
		default:
			live++
		}
	}
	return live
}

func (f *fakeLauncher) holdDials() chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dialing, f.dialGate = make(chan struct{}, 8), make(chan struct{})
	return f.dialGate
}

// A stop that arrives while the instance is still starting must win: the
// start's worker is stopped and the instance stays stopped.
func TestStopDuringStartStaysStopped(t *testing.T) {
	f := newSupervisorFixture(t)
	f.publish(t, Definition{Name: "repo", InstancePolicy: PolicyShared})
	gate := f.launcher.holdDials()
	calls := make(chan error, 1)
	go func() { _, err := whoami(t, f.sup, caller("a"), Target{Service: "repo"}); calls <- err }()
	<-f.launcher.dialing
	stopped := make(chan error, 1)
	go func() {
		_, err := f.sup.StopInstance(t.Context(), Caller{Account: "local", Admin: true}, Target{Service: "repo"})
		stopped <- err
	}()
	time.Sleep(50 * time.Millisecond)
	close(gate)
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	<-calls
	view := f.sup.Services(Caller{Account: "local", Admin: true})[0].Instances[0]
	if view.State != StateStopped || f.launcher.live() != 0 {
		t.Fatalf("stop lost to the start: state %s, %d live workers", view.State, f.launcher.live())
	}
}

// Removing a service while one of its instances is starting leaves no
// worker behind.
func TestRemoveDuringStartLeavesNoWorker(t *testing.T) {
	f := newSupervisorFixture(t)
	f.publish(t, Definition{Name: "repo"})
	gate := f.launcher.holdDials()
	go func() { _, _ = whoami(t, f.sup, caller("a"), Target{Service: "repo"}) }()
	<-f.launcher.dialing
	removed := make(chan error, 1)
	go func() { removed <- f.sup.Remove(t.Context(), "repo") }()
	time.Sleep(50 * time.Millisecond)
	close(gate)
	if err := <-removed; err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return f.launcher.live() == 0 })
	if len(f.sup.Services(Caller{Account: "local", Admin: true})) != 0 {
		t.Fatal("removed service is still listed")
	}
}

// Background recovery after a restart and a stop must not race either.
func TestStopDuringRecoveryStaysStopped(t *testing.T) {
	f := newSupervisorFixture(t)
	f.publish(t, Definition{Name: "repo", InstancePolicy: PolicyShared})
	if _, err := whoami(t, f.sup, caller("a"), Target{Service: "repo"}); err != nil {
		t.Fatal(err)
	}
	f.sup.Close()
	gate := f.launcher.holdDials()
	next := f.open(t)
	<-f.launcher.dialing
	stopped := make(chan error, 1)
	go func() {
		_, err := next.StopInstance(t.Context(), Caller{Account: "local", Admin: true}, Target{Service: "repo"})
		stopped <- err
	}()
	time.Sleep(50 * time.Millisecond)
	close(gate)
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	view := next.Services(Caller{Account: "local", Admin: true})[0].Instances[0]
	if view.State != StateStopped || f.launcher.live() != 0 {
		t.Fatalf("stop lost to recovery: state %s, %d live workers", view.State, f.launcher.live())
	}
}

// Revoking and re-approving a device gives it a new grant incarnation. The
// old grant's leases, continuations and isolated instances must go even
// though the device is approved again; shared instances keep running.
func TestReapprovedDeviceLosesOldGrantState(t *testing.T) {
	f := newSupervisorFixture(t)
	f.publish(t, Definition{Name: "repo"})
	f.publish(t, Definition{Name: "shared", InstancePolicy: PolicyShared})
	old := caller("a")
	for _, svc := range []string{"repo", "shared"} {
		if _, err := whoami(t, f.sup, old, Target{Service: svc}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := f.sup.Lease(t.Context(), old, Target{Service: "repo"}, "", time.Hour, ""); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), testHang)
	defer cancel()
	if err := f.sup.Forward(ctx, old, Target{Service: "repo"}, callRequest(t, `1`, toolParams("ask", `{}`, "")), (&collected{}).emit); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.retiredGrants[old.Grant] = true
	f.mu.Unlock()
	f.sup.Sweep(t.Context())
	admin := Caller{Account: "local", Admin: true}
	for _, v := range f.sup.Services(admin) {
		want := StateReady
		if v.Name == "repo" {
			want = StateStopped
		}
		if v.Instances[0].State != want || (v.Name == "repo" && v.Instances[0].Leases != 0) {
			t.Fatalf("%s after re-approval: %+v", v.Name, v.Instances[0])
		}
	}
	f.sup.router.continuations.mu.Lock()
	pending := len(f.sup.router.continuations.entries)
	f.sup.router.continuations.mu.Unlock()
	if pending != 0 {
		t.Fatalf("%d continuations of the retired grant remain", pending)
	}
}

// A lease added while a sweep is checking jobs must survive the sweep.
func TestSweepKeepsLeaseAddedConcurrently(t *testing.T) {
	f := newSupervisorFixture(t)
	f.publish(t, Definition{Name: "repo", InstancePolicy: PolicyShared})
	f.jobs["job"] = true
	if _, _, err := f.sup.Lease(t.Context(), caller("a"), Target{Service: "repo"}, "", time.Hour, "job"); err != nil {
		t.Fatal(err)
	}
	checking, resume := make(chan struct{}), make(chan struct{})
	f.mu.Lock()
	f.jobs["job"] = false
	f.jobCheck = func() { close(checking); <-resume }
	f.mu.Unlock()
	swept := make(chan struct{})
	go func() { defer close(swept); f.sup.Sweep(t.Context()) }()
	<-checking
	if _, _, err := f.sup.Lease(t.Context(), caller("b"), Target{Service: "repo"}, "", time.Hour, ""); err != nil {
		t.Fatal(err)
	}
	close(resume)
	<-swept
	if leases := f.sup.Services(caller("b"))[0].Instances[0].Leases; leases != 1 {
		t.Fatalf("leases after the sweep: %d, want only the new one", leases)
	}
}

func TestDiscoverMasksTasks(t *testing.T) {
	router, err := NewRouter()
	if err != nil {
		t.Fatal(err)
	}
	lines := newScriptLines()
	u := newStdioUpstream(t.Context(), lines, router)
	defer func() { _ = u.Close() }()
	route := Route{Caller: Caller{Account: "local", App: "a"}, Upstream: u}
	discover := parseRequest(t, `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`)
	out := &collected{}
	done := make(chan error, 1)
	go func() { done <- router.Forward(t.Context(), route, discover, out.emit) }()
	id := lines.lastID(t)
	lines.out <- []byte(`{"jsonrpc":"2.0","id":` + id + `,"result":{"resultType":"complete","supportedVersions":["2026-07-28"],"capabilities":{"tools":{},"extensions":{"io.modelcontextprotocol/tasks":{},"com.example/other":{}}},"ttlMs":0,"cacheScope":"private"}}`)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	extensions := member([]byte(out.last()), "result", "capabilities", "extensions")
	if member(extensions, tasksExtension) != nil || member(extensions, "com.example/other") == nil {
		t.Fatalf("hosted discover extensions: %s", extensions)
	}
}

// A compliant HTTP server without tools is ready without a tools/list.
func TestExternalServerWithoutTools(t *testing.T) {
	t.Setenv("MCPGODEBUG", "")
	server := mcp.NewServer(&mcp.Implementation{Name: "prompts-only", Version: "1"}, &mcp.ServerOptions{SupportedProtocolVersions: []string{mcpwire.Version}})
	server.AddPrompt(&mcp.Prompt{Name: "hello"}, func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: "hi"}}}}, nil
	})
	sdk := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	// A server without the tools capability may refuse tools/list outright.
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var call struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(body, &call)
		if call.Method == mcpwire.MethodListTools {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(call.ID) + `,"error":{"code":-32601,"message":"no tools"}}`))
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		sdk.ServeHTTP(w, r)
	}))
	defer fixture.Close()
	f := newSupervisorFixture(t)
	f.publish(t, Definition{Name: "prompts", Argv: []string{}, URL: fixture.URL + "/mcp", InstancePolicy: PolicyShared})
	ctx, cancel := context.WithTimeout(t.Context(), testHang)
	defer cancel()
	out := &collected{}
	get := parseRequest(t, `{"jsonrpc":"2.0","id":"g","method":"prompts/get","params":{"name":"hello","_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`)
	if err := f.sup.Forward(ctx, caller("a"), Target{Service: "prompts"}, get, out.emit); err != nil {
		t.Fatalf("prompts-only server: %v", err)
	}
	var result struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(member([]byte(out.last()), "result"), &result); err != nil || len(result.Messages) != 1 || !strings.Contains(out.last(), "hi") {
		t.Fatalf("prompts/get: %s", out.last())
	}
}
