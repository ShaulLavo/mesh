package mcphost

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/mcphost/mcpfixture"
	"github.com/shaul/mesh/internal/mcpwire"
)

const testHang = 20 * time.Second

// memLines connects a StdioUpstream to an in-process fixture.
type memLines struct {
	stdin  *io.PipeWriter
	stdout *bufio.Reader
	stderr *syncBuffer
	mu     sync.Mutex
	closer func()
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newMemLines(t *testing.T) *memLines {
	t.Helper()
	l := startMemLines(func(string) string { return "" })
	t.Cleanup(l.closer)
	return l
}

// startMemLines runs one in-process fixture. closer ends it like a process
// exit: stdin closes and stdout reaches EOF.
func startMemLines(getenv func(string) string) *memLines {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	l := &memLines{stdin: inW, stdout: bufio.NewReaderSize(outR, 64<<10), stderr: &syncBuffer{}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = mcpfixture.RunEnv(inR, outW, l.stderr, getenv)
		_ = outW.Close()
	}()
	var once sync.Once
	l.closer = func() { once.Do(func() { _ = inW.Close(); _ = outR.Close(); <-done }) }
	return l
}

func (l *memLines) WriteLine(_ context.Context, line []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, err := l.stdin.Write(line)
	return err
}

func (l *memLines) ReadLine() ([]byte, error) {
	line, err := l.stdout.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(line, []byte("\n")), nil
}

func (l *memLines) Close() error { return nil }

type testHost struct {
	router   *Router
	upstream *StdioUpstream
	lines    *memLines
}

func newTestHost(t *testing.T) *testHost {
	t.Helper()
	router, err := NewRouter()
	if err != nil {
		t.Fatal(err)
	}
	lines := newMemLines(t)
	upstream := newStdioUpstream(lines, router.ids)
	t.Cleanup(func() { _ = upstream.Close() })
	return &testHost{router: router, upstream: upstream, lines: lines}
}

func (h *testHost) route(caller string) Route {
	return Route{Caller: Caller{Account: "local", App: caller, Grant: "g1"}, Service: "svc", Revision: 1, Instance: "inst", Generation: "gen", Upstream: h.upstream}
}

func callRequest(t *testing.T, id string, params string) mcpwire.Message {
	t.Helper()
	return parseRequest(t, `{"jsonrpc":"2.0","id":`+id+`,"method":"tools/call","params":`+params+`}`)
}

func toolParams(name, args, extraMeta string) string {
	meta := `"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{"elicitation":{"form":{}}}`
	if extraMeta != "" {
		meta += "," + extraMeta
	}
	return `{"name":"` + name + `","arguments":` + args + `,"_meta":{` + meta + `}}`
}

func parseRequest(t *testing.T, raw string) mcpwire.Message {
	t.Helper()
	m, fault := mcpwire.Parse([]byte(raw))
	if fault != nil {
		t.Fatalf("parse %s: %v", raw, fault)
	}
	if fault := mcpwire.ValidateRequest(m, HostedMethods); fault != nil {
		t.Fatalf("validate %s: %v", raw, fault)
	}
	return m
}

type collected struct {
	mu       sync.Mutex
	messages []string
}

func (c *collected) emit(raw []byte, _ bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.messages = append(c.messages, string(raw))
	return nil
}

func (c *collected) last() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.messages) == 0 {
		return ""
	}
	return c.messages[len(c.messages)-1]
}

func forward(t *testing.T, h *testHost, route Route, req mcpwire.Message) (*collected, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), testHang)
	defer cancel()
	out := &collected{}
	err := h.router.Forward(ctx, route, req, out.emit)
	return out, err
}

func TestRouterRestoresExactIDs(t *testing.T) {
	h := newTestHost(t)
	for _, id := range []string{`7`, `"7"`, `-0`, `"mésh"`} {
		out, err := forward(t, h, h.route("a"), callRequest(t, id, toolParams("echo", `{"text":"hi"}`, "")))
		if err != nil {
			t.Fatal(err)
		}
		reply := out.last()
		if got := string(member([]byte(reply), "id")); got != id {
			t.Fatalf("id %s came back as %s", id, got)
		}
		if !strings.Contains(reply, `"text":"hi"`) {
			t.Fatalf("reply %s", reply)
		}
	}
}

func TestRouterSeparatesCallersWithEqualIDs(t *testing.T) {
	h := newTestHost(t)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := range 8 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			want := fmt.Sprintf("caller-%d", i)
			out, err := forward(t, h, h.route(want), callRequest(t, `1`, toolParams("echo", `{"text":"`+want+`"}`, "")))
			if err == nil && !strings.Contains(out.last(), want) {
				err = fmt.Errorf("caller %d got %s", i, out.last())
			}
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestRouterRestoresProgressTokens(t *testing.T) {
	h := newTestHost(t)
	out, err := forward(t, h, h.route("a"), callRequest(t, `"p"`, toolParams("progress", `{"count":5}`, `"progressToken":"mine"`)))
	if err != nil {
		t.Fatal(err)
	}
	if len(out.messages) < 2 {
		t.Fatalf("expected progress before the result: %v", out.messages)
	}
	for _, m := range out.messages[:len(out.messages)-1] {
		if got := string(member([]byte(m), "params", "progressToken")); got != `"mine"` {
			t.Fatalf("progress token %s in %s", got, m)
		}
	}
}

func TestRouterCancelsTheExactUpstreamRequest(t *testing.T) {
	h := newTestHost(t)
	ctx, cancel := context.WithCancel(t.Context())
	out := &collected{}
	done := make(chan error, 1)
	go func() {
		done <- h.router.Forward(ctx, h.route("a"), callRequest(t, `9`, toolParams("slow", `{}`, "")), out.emit)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("forward = %v", err)
		}
	case <-time.After(testHang):
		t.Fatal("cancelled call did not return")
	}
	eventually(t, func() bool { return strings.Contains(h.lines.stderr.String(), `cancelled "mesh-`) })
	if len(out.messages) != 0 {
		t.Fatalf("delivered after cancellation: %v", out.messages)
	}
	// The pipe stays usable for other calls.
	if _, err := forward(t, h, h.route("a"), callRequest(t, `10`, toolParams("echo", `{"text":"after"}`, ""))); err != nil {
		t.Fatal(err)
	}
}

func TestRouterContinuationIsBoundAndSingleUse(t *testing.T) {
	h := newTestHost(t)
	route := h.route("a")
	first, err := forward(t, h, route, callRequest(t, `1`, toolParams("ask", `{}`, "")))
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := decodeJSONString(member([]byte(first.last()), "result", "requestState"))
	if !strings.HasPrefix(ref, continuationPrefix) {
		t.Fatalf("upstream state leaked or missing: %s", first.last())
	}
	retry := toolParams("ask", `{}`, "")
	retry = strings.TrimSuffix(retry, "}") + `,"requestState":"` + ref + `","inputResponses":{"name":{"action":"accept","content":{"name":"bob"}}}}`

	other := h.route("b")
	if _, err := forward(t, h, other, callRequest(t, `2`, retry)); Classify(err).Code != codeStateInvalid {
		t.Fatalf("another caller used the continuation: %v", err)
	}
	changed := strings.Replace(retry, `"arguments":{}`, `"arguments":{"x":1}`, 1)
	if _, err := forward(t, h, route, callRequest(t, `2`, changed)); Classify(err).Code != codeStateInvalid {
		t.Fatalf("changed operation used the continuation: %v", err)
	}
	second, err := forward(t, h, route, callRequest(t, `2`, retry))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second.last(), "hello bob") {
		t.Fatalf("second round: %s", second.last())
	}
	if _, err := forward(t, h, route, callRequest(t, `3`, retry)); Classify(err).Code != codeStateInvalid {
		t.Fatalf("replayed continuation: %v", err)
	}
}

func TestRouterContinuationExpires(t *testing.T) {
	h := newTestHost(t)
	now := time.Now()
	h.router.continuations.now = func() time.Time { return now }
	first, err := forward(t, h, h.route("a"), callRequest(t, `1`, toolParams("ask", `{}`, "")))
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := decodeJSONString(member([]byte(first.last()), "result", "requestState"))
	now = now.Add(continuationTTL)
	retry := strings.TrimSuffix(toolParams("ask", `{}`, ""), "}") + `,"requestState":"` + ref + `","inputResponses":{"name":{"action":"accept","content":{"name":"bob"}}}}`
	if _, err := forward(t, h, h.route("a"), callRequest(t, `2`, retry)); Classify(err).Code != codeStateInvalid {
		t.Fatalf("expired continuation: %v", err)
	}
}

func TestRouterRewritesSubscriptionIDs(t *testing.T) {
	h := newTestHost(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	events := make(chan string, 8)
	listen := parseRequest(t, `{"jsonrpc":"2.0","id":"L1","method":"subscriptions/listen","params":{"notifications":{"toolsListChanged":true},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`)
	done := make(chan error, 1)
	go func() {
		done <- h.router.Forward(ctx, h.route("a"), listen, func(raw []byte, _ bool) error { events <- string(raw); return nil })
	}()
	ack := waitEvent(t, events)
	if got := string(member([]byte(ack), "params", "_meta", mcpwire.SubscriptionID)); got != `"L1"` {
		t.Fatalf("acknowledgment %s", ack)
	}
	if _, err := forward(t, h, h.route("b"), callRequest(t, `1`, toolParams("touch", `{}`, ""))); err != nil {
		t.Fatal(err)
	}
	changed := waitEvent(t, events)
	if !strings.Contains(changed, "tools/list_changed") || string(member([]byte(changed), "params", "_meta", mcpwire.SubscriptionID)) != `"L1"` {
		t.Fatalf("event %s", changed)
	}
	cancel()
	<-done
	eventually(t, func() bool { return strings.Contains(h.lines.stderr.String(), "listen ended") })
}

func waitEvent(t *testing.T, events chan string) string {
	t.Helper()
	select {
	case e := <-events:
		return e
	case <-time.After(testHang):
		t.Fatal("no event")
		return ""
	}
}

func TestRouterDeniesAfterGrantRetirement(t *testing.T) {
	h := newTestHost(t)
	current := true
	route := h.route("a")
	route.Caller.Current = func() bool { return current }
	if _, err := forward(t, h, route, callRequest(t, `1`, toolParams("echo", `{"text":"x"}`, ""))); err != nil {
		t.Fatal(err)
	}
	current = false
	if _, err := forward(t, h, route, callRequest(t, `2`, toolParams("echo", `{"text":"x"}`, ""))); Classify(err).Code != codeAccessDenied {
		t.Fatalf("retired grant: %v", err)
	}
}

func TestMalformedOutputAfterProbeFaultsTheInstance(t *testing.T) {
	h := newTestHost(t)
	ctx, cancel := context.WithTimeout(t.Context(), testHang)
	defer cancel()
	slow := make(chan error, 1)
	go func() {
		slow <- h.router.Forward(ctx, h.route("a"), callRequest(t, `1`, toolParams("slow", `{}`, "")), (&collected{}).emit)
	}()
	time.Sleep(20 * time.Millisecond)
	_, _ = forward(t, h, h.route("b"), callRequest(t, `2`, toolParams("garbage", `{}`, "")))
	if err := <-slow; Classify(err).Code != codeOutcomeUnknown {
		t.Fatalf("in-flight call after fault: %v", err)
	}
	if !errors.Is(h.upstream.Err(), ErrProtocolFault) {
		t.Fatalf("upstream not faulted: %v", h.upstream.Err())
	}
	if _, err := forward(t, h, h.route("a"), callRequest(t, `3`, toolParams("echo", `{"text":"x"}`, ""))); Classify(err).Code != codeUnavailable {
		t.Fatalf("call after fault: %v", err)
	}
}

func TestSlowConsumerDoesNotStallOthers(t *testing.T) {
	h := newTestHost(t)
	release := make(chan struct{})
	ctx, cancel := context.WithTimeout(t.Context(), testHang)
	defer cancel()
	blocked := make(chan error, 1)
	go func() {
		blocked <- h.router.Forward(ctx, h.route("slow"), callRequest(t, `1`, toolParams("big", `{"bytes":3000000}`, "")), func([]byte, bool) error {
			<-release
			return nil
		})
	}()
	time.Sleep(50 * time.Millisecond)
	for i := range 3 {
		out, err := forward(t, h, h.route(fmt.Sprint(i)), callRequest(t, `1`, toolParams("big", `{"bytes":3000000}`, "")))
		if err != nil || len(out.last()) < 3000000 {
			t.Fatalf("fast caller %d: %v", i, err)
		}
	}
	close(release)
	if err := <-blocked; err != nil {
		t.Fatal(err)
	}
}

func TestOversizedRewriteIsRefused(t *testing.T) {
	h := newTestHost(t)
	// A reply just under the cap grows past it when the caller's longer ID
	// replaces the host's; that must become an error, not a truncated line.
	long := `"` + strings.Repeat("i", 4000) + `"`
	_, err := forward(t, h, h.route("a"), callRequest(t, long, toolParams("big", fmt.Sprintf(`{"bytes":%d}`, mcpwire.MaxMessage-200), "")))
	if Classify(err).Code != codeMessageTooLarge {
		t.Fatalf("oversized reply: %v", err)
	}
}

func eventually(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(testHang)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
