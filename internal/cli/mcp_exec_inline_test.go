package cli

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/mcpnative"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session/execution"
	"github.com/shaul/mesh/internal/transport"
)

const inlineTestHost = "khI9qfAZ1eqQXe4C2JhMIfS8lwSL_GC5Aef-MsKEYZE"
const inlineTestInstance = "11111111111111111111111111111111"

func inlineTestRef() mcpnative.JobRef {
	return mcpnative.JobRef{HostID: inlineTestHost, SessionID: "7K3D", Instance: inlineTestInstance}
}

func inlineTestPage(stream, data string) protocol.ExecutionPage {
	return protocol.ExecutionPage{HostID: inlineTestHost, SessionID: "7K3D", Instance: inlineTestInstance, Stream: stream, Bytes: []byte(data), EOF: true}
}

func inlineTestReply(wait, phase string, pages ...protocol.ExecutionPage) protocol.Control {
	return protocol.Control{ExecutionWait: wait, ExecutionReceipt: &protocol.ExecutionReceipt{Phase: phase}, ExecutionOutput: pages}
}

func TestInlineExecStreamsTakesOnlyConsistentObservations(t *testing.T) {
	ref := inlineTestRef()
	both := []protocol.ExecutionPage{inlineTestPage(execution.StreamStdout, "out"), inlineTestPage(execution.StreamStderr, "err")}
	for _, ok := range []protocol.Control{inlineTestReply("completed", "terminal", both...), inlineTestReply(execWaitTimedOut, "live", both...)} {
		observed := inlineObservation(ok)
		if observed != ok.ExecutionWait {
			t.Fatalf("consistent %s/%s observed as %q", ok.ExecutionWait, ok.ExecutionReceipt.Phase, observed)
		}
		if got := inlineExecStreams(ok, observed, inlineTestHost, ref, 16); len(got) != 2 {
			t.Fatalf("%s/%s pages = %+v", ok.ExecutionWait, ok.ExecutionReceipt.Phase, got)
		}
	}
	// An inconsistent status is no observation for every use of it.
	for _, bad := range []protocol.Control{inlineTestReply("completed", "live", both...), inlineTestReply(execWaitTimedOut, "terminal", both...), inlineTestReply("", "live", both...), {ExecutionWait: "completed"}} {
		if observed := inlineObservation(bad); observed != "" {
			t.Fatalf("inconsistent observation %q was accepted as %q", bad.ExecutionWait, observed)
		}
	}
	// A repeated stream discards every page, even after an invalid copy.
	invalid := inlineTestPage(execution.StreamStdout, "x")
	invalid.Instance = "other"
	for _, pages := range [][]protocol.ExecutionPage{
		{inlineTestPage(execution.StreamStdout, "a"), inlineTestPage(execution.StreamStdout, "b")},
		{invalid, inlineTestPage(execution.StreamStdout, "b"), inlineTestPage(execution.StreamStderr, "e")},
	} {
		if got := inlineExecStreams(inlineTestReply("completed", "terminal", pages...), "completed", inlineTestHost, ref, 16); len(got) != 0 {
			t.Fatalf("repeated stream pages were used: %+v", got)
		}
	}
	// A page for another instance, or larger than asked, is read again instead.
	foreign := inlineTestPage(execution.StreamStderr, "err")
	foreign.Instance = "other"
	mixed := inlineTestReply("completed", "terminal", inlineTestPage(execution.StreamStdout, "too long"), foreign)
	if got := inlineExecStreams(mixed, "completed", inlineTestHost, ref, 4); len(got) != 0 {
		t.Fatalf("invalid pages were used: %+v", got)
	}
}

func TestRemainingExecWaitChargesTheInlineWait(t *testing.T) {
	req := mcpnative.ExecRequest{WaitMS: 5000}
	for _, observed := range []string{"", "completed"} {
		if got := remainingExecWait(req, 1000, observed); got.WaitMS != 5000 {
			t.Fatalf("observation %q changed the budget: %d", observed, got.WaitMS)
		}
	}
	if got := remainingExecWait(req, 0, execWaitTimedOut); got.WaitMS != 5000 {
		t.Fatalf("a plain create changed the budget: %d", got.WaitMS)
	}
	if got := remainingExecWait(req, 1000, execWaitTimedOut); got.WaitMS != 4000 {
		t.Fatalf("remaining wait = %d, want 4000", got.WaitMS)
	}
	if got := remainingExecWait(mcpnative.ExecRequest{WaitMS: 600}, 1000, execWaitTimedOut); got.WaitMS != 0 {
		t.Fatalf("an exhausted budget = %d, want 0", got.WaitMS)
	}
}

// inlineFixture answers creates: a waiting create blocks until release, a
// plain one is recorded and answered at once.
type inlineFixture struct {
	release chan struct{}
	waiting chan struct{}
	mu      sync.Mutex
	plain   int
	logsErr map[string]bool
}

func newInlineFixture(t *testing.T) (*inlineFixture, *mcpHostPool, HostRecord) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("MESH_CONFIG_DIR", filepath.Join(dir, "cfg"))
	t.Setenv("MESH_STATE_DIR", filepath.Join(dir, "state"))
	host := HostRecord{ID: inlineTestHost, MeshIdentity: inlineTestHost, MachineName: "pc", Endpoint: "ws://127.0.0.1:1/control/ws"}
	if err := SaveHost(host); err != nil {
		t.Fatal(err)
	}
	f := &inlineFixture{release: make(chan struct{}), waiting: make(chan struct{}, 64), logsErr: map[string]bool{}}
	pool := newMCPHostPool(func(_ context.Context, h HostRecord) (transport.Conn, error) {
		return newPoolFixtureConn(f.answer), nil
	}, time.Now)
	t.Cleanup(pool.Close)
	return f, pool, host
}

func (f *inlineFixture) answer(req protocol.Control) (protocol.Control, error) {
	switch req.Type {
	case protocol.TypeHostInfo:
		return protocol.Control{Type: protocol.TypeHostInfoResult, Host: &protocol.HostInfo{ID: inlineTestHost, MeshIdentity: inlineTestHost, MachineName: "pc", NameRevision: 1}}, nil
	case protocol.TypeExecutionCreate:
		if req.WaitMillis > 0 {
			f.waiting <- struct{}{}
			<-f.release
		} else {
			f.mu.Lock()
			f.plain++
			f.mu.Unlock()
		}
		return protocol.Control{Type: protocol.TypeExecutionReceipt}, nil
	case protocol.TypeExecutionLogs:
		if f.logsErr[req.Stream] {
			return protocol.Control{Type: protocol.TypeError, ErrorCode: "state.unavailable", Message: "busy"}, nil
		}
		page := inlineTestPage(req.Stream, "")
		return protocol.Control{Type: protocol.TypeExecutionReceipt, ExecutionPage: &page}, nil
	default:
		return protocol.Control{Type: protocol.TypeExecutionReceipt}, nil
	}
}

// Creates that wait on the host must not take the short-control slots a
// signal or kill for those same jobs needs, and once every inline slot is
// busy a further create is sent without waiting instead of queueing.
func TestInlineCreatesLeaveShortControlSlotsFree(t *testing.T) {
	f, pool, host := newInlineFixture(t)
	controls := newMCPControls(&application{dependencies: Dependencies{Now: time.Now}}, pool, "mac1_test_cred")
	plain := protocol.Control{Type: protocol.TypeExecutionCreate, RequestKey: "k", ExecutionKind: execution.KindPipe, Command: []string{"/bin/true"}, Cwd: "/"}
	var creates sync.WaitGroup
	defer func() { close(f.release); creates.Wait() }()
	for range mcpPoolInlineLimit {
		creates.Go(func() {
			inline := plain
			inline.WaitMillis = 1000
			_, _, _, _ = pool.ExchangeInline(t.Context(), host, inline)
		})
	}
	for range mcpPoolInlineLimit {
		select {
		case <-f.waiting:
		case <-time.After(outputSlotsHangCap):
			t.Fatal("inline creates did not reach the host")
		}
	}
	short, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	signal := protocol.Control{Type: protocol.TypeExecutionSignal, SessionID: "7K3D", Instance: inlineTestInstance, Signal: "TERM"}
	if _, _, err := pool.ExchangeShort(short, host, signal, false); err != nil {
		t.Fatalf("a short control could not get through while inline creates waited: %v", err)
	}
	plain.RequestID = "r"
	if _, _, inlineWait, err := controls.sendExecCreate(short, host, plain, mcpnative.ExecRequest{WaitMS: 5000, OutputBytes: 64}); err != nil || inlineWait != 0 {
		t.Fatalf("create with every inline slot busy: inline wait %d, %v", inlineWait, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.plain != 1 {
		t.Fatalf("plain creates = %d, want 1", f.plain)
	}
}

// A command that fits the request limit only without the inline fields is
// still sent, as a plain create.
func TestInlineFieldsNeverRejectACommandThatFits(t *testing.T) {
	f, pool, host := newInlineFixture(t)
	controls := newMCPControls(&application{dependencies: Dependencies{Now: time.Now}}, pool, "mac1_test_cred")
	plain := protocol.Control{Type: protocol.TypeExecutionCreate, RequestID: "r", RequestKey: "k", ExecutionKind: execution.KindPipe, Command: []string{"/bin/echo", ""}, Cwd: "/"}
	encoded, err := plain.Encode()
	if err != nil {
		t.Fatal(err)
	}
	for pad := (40 << 10) - len(encoded); pad > 0; pad-- {
		plain.Command[1] = strings.Repeat("x", pad)
		if validateControlBeforeSend(&plain) == nil {
			break
		}
	}
	inline := plain
	inline.WaitMillis, inline.OutputBytes = 1000, 64
	if validateControlBeforeSend(&plain) != nil || validateControlBeforeSend(&inline) == nil {
		t.Fatal("fixture command is not at the request limit")
	}
	if _, _, inlineWait, err := controls.sendExecCreate(t.Context(), host, plain, mcpnative.ExecRequest{WaitMS: 5000, OutputBytes: 64}); err != nil || inlineWait != 0 {
		t.Fatalf("create at the limit: inline wait %d, %v", inlineWait, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.plain != 1 {
		t.Fatalf("plain creates = %d, want 1", f.plain)
	}
}

// A page the create already carried survives the failure of the other
// stream's read, in either direction.
func TestExecStreamFailureKeepsTheOtherInlinePage(t *testing.T) {
	f, pool, host := newInlineFixture(t)
	controls := newMCPControls(&application{dependencies: Dependencies{Now: time.Now}}, pool, "mac1_test_cred")
	ref := inlineTestRef()
	req := mcpnative.ExecRequest{OutputBytes: 64}
	target := &mcpnative.TargetMeta{HostID: host.ID}
	for failing, kept := range map[string]string{execution.StreamStdout: execution.StreamStderr, execution.StreamStderr: execution.StreamStdout} {
		f.logsErr = map[string]bool{failing: true}
		inline := map[string]*mcpnative.ExecStreamData{kept: {Data: "kept"}}
		_, _, failed := controls.fetchExecInitialStreams(t.Context(), host, req, ref, &agentapi.ExecutionReceipt{}, "completed", target, inline)
		if failed == nil {
			t.Fatalf("%s read failure was not reported", failing)
		}
		data, ok := failed.Envelope.Data.(mcpnative.ExecResultData)
		if !ok {
			t.Fatalf("%s failure carried %T", failing, failed.Envelope.Data)
		}
		got := data.Stdout
		if kept == execution.StreamStderr {
			got = data.Stderr
		}
		if got == nil || got.Data != "kept" {
			t.Fatalf("%s failure dropped the %s page: %+v", failing, kept, data)
		}
	}
}
