package mcphost

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/mcpwire"
)

// scriptLines is a LineConn whose server output the test writes directly.
type scriptLines struct {
	out     chan []byte
	mu      sync.Mutex
	written [][]byte
	wrote   chan struct{}
}

func newScriptLines() *scriptLines {
	return &scriptLines{out: make(chan []byte, 1024), wrote: make(chan struct{}, 1024)}
}

func (s *scriptLines) WriteLine(_ context.Context, line []byte) error {
	s.mu.Lock()
	s.written = append(s.written, append([]byte(nil), line...))
	s.mu.Unlock()
	s.wrote <- struct{}{}
	return nil
}

func (s *scriptLines) ReadLine() ([]byte, error) {
	line, ok := <-s.out
	if !ok {
		return nil, ErrStdoutClosed
	}
	return line, nil
}

func (s *scriptLines) Close() error { return nil }

// lastID returns the host-assigned ID of the most recent request written.
func (s *scriptLines) lastID(t *testing.T) string {
	t.Helper()
	select {
	case <-s.wrote:
	case <-time.After(testHang):
		t.Fatal("no request reached the server")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(member(s.written[len(s.written)-1], "id"))
}

func listenRequest(t *testing.T, id string) mcpwire.Message {
	t.Helper()
	return parseRequest(t, `{"jsonrpc":"2.0","id":`+id+`,"method":"subscriptions/listen","params":{"notifications":{"toolsListChanged":true},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`)
}

func listChanged(id string, pad int) []byte {
	return []byte(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":` + id + `,"com.example/pad":"` + strings.Repeat("p", pad) + `"}}}`)
}

func acknowledged(id, filter string) []byte {
	return []byte(`{"jsonrpc":"2.0","method":"notifications/subscriptions/acknowledged","params":{"notifications":` + filter + `,"_meta":{"io.modelcontextprotocol/subscriptionId":` + id + `}}}`)
}

func TestSlowConsumerReleasesItsQueuedBytes(t *testing.T) {
	router, err := NewRouter()
	if err != nil {
		t.Fatal(err)
	}
	lines := newScriptLines()
	u := newStdioUpstream(t.Context(), lines, router)
	defer func() { _ = u.Close() }()
	release := make(chan struct{})
	done := make(chan error, 1)
	route := Route{Caller: Caller{Account: "local", App: "slow"}, Upstream: u}
	go func() {
		done <- router.Forward(t.Context(), route, listenRequest(t, `"L"`), func([]byte, bool) error {
			<-release
			return nil
		})
	}()
	id := lines.lastID(t)
	lines.out <- acknowledged(id, `{"toolsListChanged":true}`)
	// The sink holds the acknowledgment; the queue fills past 8 MiB.
	for range 10 {
		lines.out <- listChanged(id, 1<<20)
	}
	eventually(t, func() bool {
		u.mu.Lock()
		defer u.mu.Unlock()
		return u.calls[strings.Trim(id, `"`)] == nil
	})
	// Only the acknowledgment in the caller's hands is still charged.
	if used := budgetUsed(u.budget); used > 4<<10 {
		t.Fatalf("failed call still holds %d bytes of the instance budget", used)
	}
	if used := budgetUsed(router.budget); used > 4<<10 {
		t.Fatalf("failed call still holds %d bytes of the host budget", used)
	}
	close(release)
	if err := <-done; !errors.Is(err, ErrSlowConsumer) {
		t.Fatalf("slow consumer ended with %v", err)
	}
}

// HTTP calls draw on the same instance and host budgets as stdio calls.
func TestHTTPQueueChargesSharedBudget(t *testing.T) {
	host := &byteBudget{limit: 3 << 20}
	instance := &byteBudget{limit: 2 << 20, parent: host}
	event := func(pad int) mcpwire.Event {
		raw := listChanged(`"L"`, pad)
		m, fault := mcpwire.Parse(raw)
		if fault != nil {
			t.Fatal(fault)
		}
		return mcpwire.Event{Message: m}
	}
	first, second := newEventQueue(instance), newEventQueue(instance)
	if !first.push(event(1500 << 10)) {
		t.Fatal("the first call's event did not fit")
	}
	if second.push(event(1500 << 10)) {
		t.Fatal("a second call exceeded the shared instance budget")
	}
	if used := budgetUsed(instance); used > 1600<<10 {
		t.Fatalf("instance budget after the refused call: %d", used)
	}
	first.close()
	if used, hostUsed := budgetUsed(instance), budgetUsed(host); used != 0 || hostUsed != 0 {
		t.Fatalf("finished calls still hold %d instance and %d host bytes", used, hostUsed)
	}
}

func TestNothingIsDeliveredAfterCancellation(t *testing.T) {
	router, err := NewRouter()
	if err != nil {
		t.Fatal(err)
	}
	lines := newScriptLines()
	u := newStdioUpstream(t.Context(), lines, router)
	defer func() { _ = u.Close() }()
	ctx, cancel := context.WithCancel(t.Context())
	inSink, release := make(chan struct{}), make(chan struct{})
	var delivered []string
	done := make(chan error, 1)
	route := Route{Caller: Caller{Account: "local", App: "a"}, Upstream: u}
	go func() {
		done <- router.Forward(ctx, route, listenRequest(t, `"L"`), func(raw []byte, _ bool) error {
			delivered = append(delivered, string(raw))
			if len(delivered) == 1 {
				close(inSink)
				<-release
			}
			return nil
		})
	}()
	id := lines.lastID(t)
	lines.out <- acknowledged(id, `{"toolsListChanged":true}`)
	<-inSink
	lines.out <- listChanged(id, 16)
	eventually(t, func() bool {
		u.mu.Lock()
		defer u.mu.Unlock()
		call := u.calls[strings.Trim(id, `"`)]
		return call != nil && len(call.queue) == 2
	})
	cancel()
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled listen: %v", err)
	}
	if len(delivered) != 1 {
		t.Fatalf("delivered after cancellation: %v", delivered)
	}
}

func budgetUsed(b *byteBudget) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

func TestInstanceBudgetIsSharedAcrossCalls(t *testing.T) {
	router, err := NewRouter()
	if err != nil {
		t.Fatal(err)
	}
	lines := newScriptLines()
	u := newStdioUpstream(t.Context(), lines, router)
	defer func() { _ = u.Close() }()
	// Five stalled listens of 7 MiB each fit their own 8 MiB call caps but
	// not the 32 MiB instance budget, so later ones fail without stalling
	// the reader.
	release := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan error, 5)
	for i := range 5 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			route := Route{Caller: Caller{Account: "local", App: fmt.Sprint(i)}, Upstream: u}
			results <- router.Forward(t.Context(), route, listenRequest(t, fmt.Sprintf(`"L%d"`, i)), func([]byte, bool) error {
				<-release
				return nil
			})
		}(i)
		id := lines.lastID(t)
		lines.out <- acknowledged(id, `{"toolsListChanged":true}`)
		for range 7 {
			lines.out <- listChanged(id, 1<<20)
		}
	}
	// Some call must lose its place once 35 MiB is held against 32 MiB.
	eventually(t, func() bool {
		u.mu.Lock()
		defer u.mu.Unlock()
		return len(u.calls) < 5 && len(lines.out) == 0
	})
	if used := budgetUsed(u.budget); used > instanceQueueBytes {
		t.Fatalf("instance budget exceeded: %d", used)
	}
	close(release)
	_ = u.Close()
	wg.Wait()
	close(results)
	slow := 0
	for err := range results {
		if errors.Is(err, ErrSlowConsumer) {
			slow++
		}
	}
	if slow == 0 {
		t.Fatal("no call hit the 32 MiB instance budget")
	}
}

func TestProgressFloodIsCoalesced(t *testing.T) {
	router, err := NewRouter()
	if err != nil {
		t.Fatal(err)
	}
	lines := newScriptLines()
	u := newStdioUpstream(t.Context(), lines, router)
	defer func() { _ = u.Close() }()
	release := make(chan struct{})
	var mu sync.Mutex
	var seen []string
	done := make(chan error, 1)
	route := Route{Caller: Caller{Account: "local", App: "a"}, Upstream: u}
	go func() {
		done <- router.Forward(t.Context(), route, callRequest(t, `1`, toolParams("echo", `{}`, `"progressToken":"t"`)), func(raw []byte, _ bool) error {
			<-release
			mu.Lock()
			seen = append(seen, string(raw))
			mu.Unlock()
			return nil
		})
	}()
	id := lines.lastID(t)
	lines.mu.Lock()
	token := string(member(lines.written[len(lines.written)-1], "params", "_meta", "progressToken"))
	lines.mu.Unlock()
	// Far more progress than the 8 MiB call queue could hold uncoalesced.
	for i := range 20000 {
		lines.out <- []byte(fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":%s,"progress":%d,"message":"%s"}}`, token, i, strings.Repeat("m", 1024)))
	}
	lines.out <- []byte(`{"jsonrpc":"2.0","id":` + id + `,"result":{"resultType":"complete","content":[{"type":"text","text":"done"}]}}`)
	// Release the caller only after every line was routed: the terminal
	// result removes the call from the routing table.
	eventually(t, func() bool {
		u.mu.Lock()
		defer u.mu.Unlock()
		return len(u.calls) == 0
	})
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("progress flood: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	// The progress in hand, one coalesced update, and the result.
	if len(seen) > 3 || !strings.Contains(seen[len(seen)-1], "done") {
		t.Fatalf("progress was not coalesced: %d messages", len(seen))
	}
}
