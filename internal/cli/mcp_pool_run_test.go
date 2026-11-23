package cli

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

// runPoolFixture dials fixture connections that answer host.info and
// execution.run, counting dials and probes.
type runPoolFixture struct {
	mu               sync.Mutex
	dials            int
	probes           int
	runs             int
	badReply         bool
	emptyTermination bool
	wrongHost        bool
	deadConns        map[*poolFixtureConn]bool
	conns            []*poolFixtureConn
	clock            time.Time
}

func newRunPoolFixture(t *testing.T) (*runPoolFixture, *mcpHostPool, HostRecord) {
	t.Helper()
	f := &runPoolFixture{deadConns: map[*poolFixtureConn]bool{}, clock: time.Now()}
	host := HostRecord{ID: inlineTestHost, MeshIdentity: inlineTestHost, MachineName: "pc", Endpoint: "ws://127.0.0.1:1/control/ws"}
	pool := newMCPHostPool(func(context.Context, HostRecord) (transport.Conn, error) { return f.dial(), nil }, f.now)
	t.Cleanup(pool.Close)
	return f, pool, host
}

func (f *runPoolFixture) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clock
}

func (f *runPoolFixture) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clock = f.clock.Add(d)
}

func (f *runPoolFixture) dial() *poolFixtureConn {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dials++
	var conn *poolFixtureConn
	conn = newPoolFixtureConn(func(req protocol.Control) (protocol.Control, error) { return f.answer(conn, req) })
	f.conns = append(f.conns, conn)
	return conn
}

func (f *runPoolFixture) answer(conn *poolFixtureConn, req protocol.Control) (protocol.Control, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deadConns[conn] {
		return protocol.Control{}, errors.New("connection closed by the host")
	}
	if req.Type == protocol.TypeHostInfo {
		f.probes++
		id := inlineTestHost
		if f.wrongHost && f.probes > 1 {
			id = "someone-else"
		}
		return protocol.Control{Type: protocol.TypeHostInfoResult, Host: &protocol.HostInfo{ID: id, MeshIdentity: id, MachineName: "pc", NameRevision: 1}}, nil
	}
	f.runs++
	if f.badReply {
		return protocol.Control{Type: protocol.TypeExecutionReceipt}, nil
	}
	if f.emptyTermination {
		return protocol.Control{Type: protocol.TypeExecutionRunResult, ExecutionRun: &protocol.ExecutionRunResult{}}, nil
	}
	code := 0
	return protocol.Control{Type: protocol.TypeExecutionRunResult, ExecutionRun: &protocol.ExecutionRunResult{Termination: protocol.ExecutionTermination{Kind: "exit", Code: &code}}}, nil
}

func (f *runPoolFixture) counts() (dials, probes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dials, f.probes
}

func runPoolRequest() protocol.Control {
	return protocol.Control{Type: protocol.TypeExecutionRun, Caller: "caller", TimeoutMillis: 1000, OutputBytes: 16, Command: []string{"/bin/true"}, ExecutionContext: &protocol.ExecutionContextRequest{ContextID: "0123456789abcdef0123456789abcdef"}}
}

func TestExchangeRunReusesAConnectionAfterAValidReply(t *testing.T) {
	f, pool, host := newRunPoolFixture(t)
	for range 2 {
		if _, _, err := pool.ExchangeRun(t.Context(), host, runPoolRequest()); err != nil {
			t.Fatal(err)
		}
	}
	// The first dial answers host.info as part of the handshake.
	if dials, probes := f.counts(); dials != 1 || probes != 1 {
		t.Fatalf("dials=%d probes=%d, want one connection and no probe on reuse", dials, probes)
	}
}

func TestExchangeRunDiscardsAConnectionAfterAnInvalidReply(t *testing.T) {
	f, pool, host := newRunPoolFixture(t)
	f.badReply = true
	_, _, err := pool.ExchangeRun(t.Context(), host, runPoolRequest())
	if !errors.Is(err, errInvalidRunReply) || isPreDispatchError(err) {
		t.Fatalf("invalid reply = %v", err)
	}
	f.badReply = false
	if _, _, err := pool.ExchangeRun(t.Context(), host, runPoolRequest()); err != nil {
		t.Fatal(err)
	}
	if dials, _ := f.counts(); dials != 2 {
		t.Fatalf("dials = %d, want a fresh connection after the invalid reply", dials)
	}
}

func TestExchangeRunRefusesAFullRunBudget(t *testing.T) {
	_, pool, host := newRunPoolFixture(t)
	bucket, err := pool.bucket(host)
	if err != nil {
		t.Fatal(err)
	}
	for range mcpPoolRunLimit {
		bucket.runSlots <- struct{}{}
	}
	if _, _, err := pool.ExchangeRun(t.Context(), host, runPoolRequest()); !errors.Is(err, errRunPoolFull) {
		t.Fatalf("full budget = %v", err)
	}
	failure := runTransportFailure(errRunPoolFull, host.ID, nil)
	if failure.Envelope.Errors[0].Code != "resource.limit" {
		t.Fatalf("full budget outcome = %+v", failure.Envelope.Errors)
	}
}

func TestExchangeRunProbesAConnectionThatSatIdle(t *testing.T) {
	f, pool, host := newRunPoolFixture(t)
	if _, _, err := pool.ExchangeRun(t.Context(), host, runPoolRequest()); err != nil {
		t.Fatal(err)
	}
	f.advance(runProbeIdle + time.Second)
	if _, _, err := pool.ExchangeRun(t.Context(), host, runPoolRequest()); err != nil {
		t.Fatal(err)
	}
	if dials, probes := f.counts(); dials != 1 || probes != 2 {
		t.Fatalf("dials=%d probes=%d, want the idle connection probed and reused", dials, probes)
	}
	// A connection the host closed while idle is replaced before the run.
	f.mu.Lock()
	f.deadConns[f.conns[0]] = true
	f.mu.Unlock()
	f.advance(runProbeIdle + time.Second)
	if _, _, err := pool.ExchangeRun(t.Context(), host, runPoolRequest()); err != nil {
		t.Fatalf("run after a dead idle connection = %v", err)
	}
	if dials, _ := f.counts(); dials != 2 {
		t.Fatalf("dials = %d, want a fresh connection", dials)
	}
}

func TestExchangeRunRejectsAMalformedResult(t *testing.T) {
	f, pool, host := newRunPoolFixture(t)
	f.emptyTermination = true
	if _, _, err := pool.ExchangeRun(t.Context(), host, runPoolRequest()); !errors.Is(err, errInvalidRunReply) || isPreDispatchError(err) {
		t.Fatalf("empty termination = %v", err)
	}
}

func TestExchangeRunSendsNothingOnceCancelled(t *testing.T) {
	f, pool, host := newRunPoolFixture(t)
	// A pooled connection is reused without touching the context, so the
	// cancelled call reaches the exchange itself.
	if _, _, err := pool.ExchangeRun(t.Context(), host, runPoolRequest()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err := pool.ExchangeRun(ctx, host, runPoolRequest())
	if !isPreDispatchError(err) {
		t.Fatalf("cancelled run = %v, want a pre-dispatch failure", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.runs != 1 {
		t.Fatalf("the host saw %d runs, want only the first", f.runs)
	}
}

func TestExchangeRunReplacesAConnectionWhoseProbeNamesAnotherHost(t *testing.T) {
	f, pool, host := newRunPoolFixture(t)
	if _, _, err := pool.ExchangeRun(t.Context(), host, runPoolRequest()); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.wrongHost = true
	f.mu.Unlock()
	f.advance(runProbeIdle + time.Second)
	// The fresh connection's own handshake also answers for another host,
	// so the run fails before anything is sent.
	if _, _, err := pool.ExchangeRun(t.Context(), host, runPoolRequest()); !isPreDispatchError(err) {
		t.Fatalf("run after a mismatched probe = %v", err)
	}
	if dials, _ := f.counts(); dials != 2 {
		t.Fatalf("dials = %d, want the mismatched connection replaced", dials)
	}
}

func TestRunTransportFailureIsUnknownOnceSent(t *testing.T) {
	sent := runTransportFailure(errors.New("read execution.run reply: EOF"), inlineTestHost, nil)
	if sent.Envelope.Errors[0].Code != "request.outcome_unknown" {
		t.Fatalf("failure after sending = %+v", sent.Envelope.Errors)
	}
	before := runTransportFailure(&preDispatchError{err: errors.New("dial refused")}, inlineTestHost, nil)
	if before.Envelope.Errors[0].Code != "host.unreachable" {
		t.Fatalf("failure before sending = %+v", before.Envelope.Errors)
	}
}
