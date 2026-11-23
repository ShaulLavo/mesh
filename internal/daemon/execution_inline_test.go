package daemon

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session/execution"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/worker"
)

func TestInlineCreateReturnsResultAndOutputOnce(t *testing.T) {
	f := newNativePipeDaemon(t)
	req := protocol.Control{Type: protocol.TypeExecutionCreate, RequestID: "inline", RequestKey: "inline", ExecutionKind: execution.KindPipe, Cwd: f.root, Command: []string{"/bin/sh", "-c", "printf out; printf err >&2; exit 3"}, WaitMillis: 10000, OutputBytes: 4096}
	first := f.request(t, req)
	assertInlineCompletion(t, first, 3)
	// A replay of the same key observes the same job; it never launches again.
	replay := f.request(t, req)
	assertInlineCompletion(t, replay, 3)
	if replay.ExecutionReceipt.Instance != first.ExecutionReceipt.Instance {
		t.Fatalf("replay changed instance: %s then %s", first.ExecutionReceipt.Instance, replay.ExecutionReceipt.Instance)
	}
	// The smallest budget still observes a job that has finished.
	brief := req
	brief.WaitMillis = 1
	assertInlineCompletion(t, f.request(t, brief), 3)
	conflict := req
	conflict.Command = []string{"/bin/true"}
	reply, err := f.exchange(conflict)
	if err != nil || reply.Type != protocol.TypeError || reply.ErrorCode != "request.key_conflict" || len(reply.ExecutionOutput) != 0 {
		t.Fatalf("conflicting replay = %+v, %v", reply, err)
	}
}

// A context job, which is what mesh_exec creates, gets its result and both
// pages from the one admission its inline wait made for its caller.
func TestInlineContextJobReturnsResultAndOutput(t *testing.T) {
	f := newNativePipeDaemon(t)
	caller, _ := testCaller(t)
	opened := f.request(t, protocol.Control{Type: protocol.TypeContextOpen, RequestID: "open", RequestKey: "ctx", Caller: caller, ExecutionContext: &protocol.ExecutionContextRequest{Open: &protocol.ContextSpec{Cwd: f.root, Shell: protocol.ContextShellSh}}})
	if opened.ContextInfo == nil {
		t.Fatalf("context open = %+v", opened)
	}
	req := protocol.Control{Type: protocol.TypeExecutionCreate, RequestID: "job", RequestKey: "job", Caller: caller, ExecutionKind: execution.KindPipe, ExecutionContext: &protocol.ExecutionContextRequest{ContextID: opened.ContextInfo.ContextID, ShellCommand: "printf out; printf err >&2; exit 3"}, WaitMillis: 10000, OutputBytes: 4096}
	assertInlineCompletion(t, f.request(t, req), 3)
	// Another caller cannot observe the job through a replay of its key.
	other, _ := testCaller(t)
	stranger := req
	stranger.Caller = other
	reply, err := f.exchange(stranger)
	if err != nil || reply.Type != protocol.TypeError || reply.ExecutionWait != "" || len(reply.ExecutionOutput) != 0 {
		t.Fatalf("another caller's replay = %+v, %v", reply, err)
	}
}

// A job whose retirement marks its receipt after the stdout page is read and
// before the stderr page, with every stream byte still on disk: the stderr
// page sees the mark and is left out, as a separate execution.logs reports
// the output expired.
func TestInlinePagesSeeARetirementBetweenStreams(t *testing.T) {
	gate := t.TempDir()
	f := newNativePipeDaemon(t, inlinePageGateFixture+"="+gate)
	req := protocol.Control{Type: protocol.TypeExecutionCreate, RequestID: "retire", RequestKey: "retire", ExecutionKind: execution.KindPipe, Cwd: f.root, Command: []string{"/bin/sh", "-c", "printf out; printf err >&2"}, WaitMillis: 10000, OutputBytes: 64}
	type result struct {
		reply protocol.Control
		err   error
	}
	done := make(chan result, 1)
	go func() {
		reply, err := f.exchange(req)
		done <- result{reply, err}
	}()
	pipeEventually(t, func() bool {
		_, err := os.Stat(filepath.Join(gate, "paused"))
		return err == nil
	})
	replay := req
	replay.WaitMillis, replay.OutputBytes = 0, 0
	job := f.request(t, replay).ExecutionReceipt
	f.markRetiring(t, job)
	if err := os.WriteFile(filepath.Join(gate, "resume"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err != nil || got.reply.ExecutionWait != "completed" {
		t.Fatalf("inline create = %+v, %v", got.reply, got.err)
	}
	if len(got.reply.ExecutionOutput) != 1 || got.reply.ExecutionOutput[0].Stream != execution.StreamStdout || string(got.reply.ExecutionOutput[0].Bytes) != "out" {
		t.Fatalf("inline pages after retirement = %+v", got.reply.ExecutionOutput)
	}
	logs := boundPipeRequest(protocol.TypeExecutionLogs, job)
	logs.Stream, logs.MaxBytes = execution.StreamStderr, 64
	if reply, err := f.exchange(logs); err != nil || reply.Type != protocol.TypeError || reply.ErrorCode != "result.expired" {
		t.Fatalf("stderr after retirement = %+v, %v", reply, err)
	}
}

// markRetiring starts an exact retirement of job from the test process and
// stops it right after the receipt is marked, before any bytes are deleted.
func (f *nativePipeDaemon) markRetiring(t *testing.T, job *protocol.ExecutionReceipt) {
	t.Helper()
	out, err := execution.OpenOutput(f.state)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = out.Close() }()
	b, err := out.Binding(job.SessionID, job.Instance, job.HostID)
	if err != nil {
		t.Fatal(err)
	}
	j, err := execution.Open(f.state, worker.BootID(), "test-retirement")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = j.Close() }()
	stop := errors.New("stop after the mark")
	if err := j.RetireExact(t.Context(), f.sessions, b, func() error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("retirement before its deletion = %v", err)
	}
}

// A result published after the inline wait timed out: whatever pages the
// create carries match what separate reads of the finished job report.
// TestInlineCreateTimesOutWithTheLiveReceipt covers a receipt that is still
// live, which reads no pages.
func TestInlinePagesOfAJobFinishedAfterItsWaitMatchSeparateReads(t *testing.T) {
	f := newNativePipeDaemon(t, inlineAfterWaitFixture+"=1s")
	req := protocol.Control{Type: protocol.TypeExecutionCreate, RequestID: "late-result", RequestKey: "late-result", ExecutionKind: execution.KindPipe, Cwd: f.root, Command: []string{"/bin/sh", "-c", "sleep 0.2; printf out"}, WaitMillis: 50, OutputBytes: 64}
	reply := f.request(t, req)
	if reply.ExecutionWait != "timed_out" {
		t.Fatalf("inline create of a job that outlived its wait = %+v", reply)
	}
	wait := boundPipeRequest(protocol.TypeExecutionWait, reply.ExecutionReceipt)
	wait.TimeoutMillis = 10000
	if done := f.request(t, wait); done.ExecutionWait != "completed" {
		t.Fatalf("job did not finish: %+v", done)
	}
	for _, inline := range reply.ExecutionOutput {
		logs := boundPipeRequest(protocol.TypeExecutionLogs, reply.ExecutionReceipt)
		logs.Stream, logs.MaxBytes = inline.Stream, 64
		page := f.request(t, logs).ExecutionPage
		if page == nil || !bytes.Equal(page.Bytes, inline.Bytes) || page.EOF != inline.EOF || page.StreamClosed != inline.StreamClosed || page.CloseReason != inline.CloseReason {
			t.Fatalf("inline %s page %+v differs from a separate read %+v", inline.Stream, inline, page)
		}
	}
}

// A live receipt may lag a result published after the wait, so a job still
// running carries no pages, even with output already written; the caller's
// separate reads refresh it. TestInlinePagesOfAJobFinishedAfterItsWaitMatchSeparateReads
// covers a job that finishes in between.
func TestInlineCreateTimesOutWithTheLiveReceipt(t *testing.T) {
	f := newNativePipeDaemon(t)
	req := protocol.Control{Type: protocol.TypeExecutionCreate, RequestID: "slow", RequestKey: "slow", ExecutionKind: execution.KindPipe, Cwd: f.root, Command: []string{"/bin/sh", "-c", "printf started; exec sleep 30"}, WaitMillis: 200, OutputBytes: 64}
	reply := f.request(t, req)
	if reply.ExecutionWait != "timed_out" || reply.ExecutionReceipt == nil || reply.ExecutionReceipt.Phase == "terminal" || reply.ExecutionReceipt.Result != nil {
		t.Fatalf("slow inline create = %+v", reply)
	}
	if len(reply.ExecutionOutput) != 0 {
		t.Fatalf("live job carried inline pages: %+v", reply.ExecutionOutput)
	}
	logs := boundPipeRequest(protocol.TypeExecutionLogs, reply.ExecutionReceipt)
	logs.Stream, logs.MaxBytes = execution.StreamStdout, 64
	pipeEventually(t, func() bool {
		page := f.request(t, logs).ExecutionPage
		return page != nil && string(page.Bytes) == "started" && !page.EOF
	})
	// Without waitMillis a create answers as before, with no observation.
	plain := req
	plain.RequestKey, plain.WaitMillis, plain.OutputBytes = "plain", 0, 0
	if reply := f.request(t, plain); reply.ExecutionWait != "" || reply.ExecutionOutput != nil {
		t.Fatalf("plain create carried an observation: %+v", reply)
	}
}

// A client that disconnects while its accepted job runs loses only the
// observation: the job finishes, and a retry of its key returns that same
// instance and its result without a second launch.
func TestInlineCreateDisconnectAfterAcceptanceKeepsTheJob(t *testing.T) {
	f := newNativePipeDaemon(t)
	marker := filepath.Join(f.root, "launches")
	req := protocol.Control{Type: protocol.TypeExecutionCreate, RequestID: "gone", RequestKey: "gone", ExecutionKind: execution.KindPipe, Cwd: f.root, Command: []string{"/bin/sh", "-c", "echo x >> " + marker + "; sleep 1; printf done"}, WaitMillis: 10000, OutputBytes: 64}
	conn, err := net.DialTimeout("unix", SocketPath(f.state), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.NewWriter(conn).WriteControlMsg(req); err != nil {
		t.Fatal(err)
	}
	var accepted *protocol.ExecutionReceipt
	pipeEventually(t, func() bool {
		reply, err := f.exchange(protocol.Control{Type: protocol.TypeExecutionRequest, RequestID: "probe", RequestKey: req.RequestKey})
		accepted = reply.ExecutionReceipt
		return err == nil && reply.Type != protocol.TypeError && accepted != nil
	})
	_ = conn.Close()
	// A retry while the first create is still launching may come back without
	// an observation; either way it names the accepted instance.
	retry := f.request(t, req)
	if retry.ExecutionReceipt == nil || retry.ExecutionReceipt.Instance != accepted.Instance {
		t.Fatalf("retry returned %+v, accepted instance %s", retry.ExecutionReceipt, accepted.Instance)
	}
	wait := boundPipeRequest(protocol.TypeExecutionWait, accepted)
	wait.TimeoutMillis = 10000
	if done := f.request(t, wait); done.ExecutionWait != "completed" {
		t.Fatalf("accepted job did not finish after its client left: %+v", done)
	}
	assertPipeBytes(t, f, accepted, execution.StreamStdout, []byte("done"))
	launches, err := os.ReadFile(marker) //nolint:gosec // the fixture's marker under its own temporary root
	if err != nil || string(launches) != "x\n" {
		t.Fatalf("launches = %q, %v; want exactly one", launches, err)
	}
}

// The pages have their own budget: a wait budget that ran out after the wait
// succeeded still returns both of them.
func TestInlinePagesOutliveTheWaitBudget(t *testing.T) {
	f := newNativePipeDaemon(t, inlineAfterWaitFixture+"="+(inlineObservationGrace+200*time.Millisecond).String())
	req := protocol.Control{Type: protocol.TypeExecutionCreate, RequestID: "late", RequestKey: "late", ExecutionKind: execution.KindPipe, Cwd: f.root, Command: []string{"/bin/sh", "-c", "printf out; printf err >&2"}}
	job := f.request(t, req).ExecutionReceipt
	wait := boundPipeRequest(protocol.TypeExecutionWait, job)
	wait.TimeoutMillis = 10000
	if done := f.request(t, wait); done.ExecutionWait != "completed" {
		t.Fatalf("job did not finish: %+v", done)
	}
	req.WaitMillis, req.OutputBytes = 10, 64
	assertInlineCompletion(t, f.request(t, req), 0)
}

// A client that leaves before its waiting create is accepted cancels only
// the observation: the create itself still goes through.
func TestInlineCreateDisconnectBeforeAcceptanceStillCreates(t *testing.T) {
	state := compactSocketTempDir(t)
	host, _, err := identity.LoadOrCreate(state)
	if err != nil {
		t.Fatal(err)
	}
	launches := 0
	l := mustLifecycle(t, lifecycleConfig{Catalog: &lifecycleTestCatalog{}, Connector: failingLifecycleConnector(),
		Host: storage.Host{ID: storage.HostID(host.ID), MeshIdentity: host.ID}, SessionsDir: filepath.Join(state, "s"),
		Launch: func(cfg worker.LaunchConfig) (worker.Launched, error) {
			launches++
			return worker.Launched{Meta: worker.Meta{Reservation: cfg.Reservation}}, nil
		}})
	t.Cleanup(l.closeExecution)
	conn := newRelayTestConn()
	request := protocol.Control{Type: protocol.TypeExecutionCreate, RequestID: "early", RequestKey: "early", ExecutionKind: execution.KindPipe, Cwd: state, Command: []string{"/bin/true"}, WaitMillis: 50}
	ctx, cancel := executionRequestContext(context.WithValue(t.Context(), localClientKey{}, true), &frameLookahead{conn: conn}, request)
	defer cancel()
	executionBeforeReserve = func(ctx context.Context) {
		gone, _ := ctx.Value(inlineDisconnectKey{}).(<-chan struct{})
		_ = conn.Close()
		<-gone
	}
	t.Cleanup(func() { executionBeforeReserve = func(context.Context) {} })
	reply, err := l.executionControl(ctx, request)
	if err != nil || reply.ExecutionReceipt == nil || launches != 1 {
		t.Fatalf("create after its client left = %+v, %v, %d launches", reply, err, launches)
	}
}

// A device whose grant is revoked receives no observation of its job.
func TestInlineObservationNeedsACurrentGrant(t *testing.T) {
	state := compactSocketTempDir(t)
	host, _, err := identity.LoadOrCreate(state)
	if err != nil {
		t.Fatal(err)
	}
	device, grantCtx := bindExecutionFixtureGrant(t, state, host.ID)
	l := mustLifecycle(t, lifecycleConfig{Catalog: &lifecycleTestCatalog{}, Connector: failingLifecycleConnector(),
		Host: storage.Host{ID: storage.HostID(host.ID), MeshIdentity: host.ID}, SessionsDir: filepath.Join(state, "s")})
	t.Cleanup(l.closeExecution)
	key := prepareQueuedExecution(t, l, host.ID, device, state, false)
	request := protocol.Control{Type: protocol.TypeExecutionCreate, RequestID: "observe", RequestKey: key.Opaque, WaitMillis: 1000, OutputBytes: 64}
	j, err := l.executionJournal()
	if err != nil {
		t.Fatal(err)
	}
	r, err := j.Lookup(grantCtx, key)
	if err != nil {
		t.Fatal(err)
	}
	created, _ := executionResponse(request, r, nil)
	t.Cleanup(func() { inlineAfterWait = func() {} })
	if observed, err := l.withInlineResult(grantCtx, request, created, nil); err != nil || observed.ExecutionWait != "completed" {
		t.Fatalf("current grant observation = %+v, %v", observed, err)
	}
	// A wait budget that runs out after the wait succeeded keeps the result.
	// TestInlinePagesOutliveTheWaitBudget covers the pages.
	late := request
	late.WaitMillis = 10
	inlineAfterWait = func() { time.Sleep(10*time.Millisecond + inlineObservationGrace + 100*time.Millisecond) }
	if observed, err := l.withInlineResult(grantCtx, late, created, nil); err != nil || observed.ExecutionWait != "completed" {
		t.Fatalf("observation past its budget = %+v, %v", observed, err)
	}
	// Revoked while the wait is in flight: the check after the wait stops it.
	revoked := false
	inlineAfterWait = func() {
		revoked = identity.RevokeDevice(t.Context(), state, device) == nil
	}
	observed, err := l.withInlineResult(grantCtx, request, created, nil)
	if !revoked || err == nil || observed.ExecutionWait != "" || observed.ExecutionOutput != nil {
		t.Fatalf("grant revoked during the wait: observation %+v, %v", observed, err)
	}
	// Revoked before: nothing is observed at all.
	observed, _ = l.withInlineResult(grantCtx, request, created, nil)
	if observed.ExecutionWait != "" || observed.ExecutionOutput != nil {
		t.Fatalf("revoked grant received an observation: %+v", observed)
	}
}

// assertInlineCompletion expects the fixture command that prints out and err.
func assertInlineCompletion(t *testing.T, reply protocol.Control, code int) {
	t.Helper()
	r := reply.ExecutionReceipt
	if reply.ExecutionWait != "completed" || r == nil || r.Phase != "terminal" || r.Result == nil || r.Result.Termination.Code == nil || *r.Result.Termination.Code != code {
		t.Fatalf("inline completion = %+v", reply)
	}
	want := map[string]string{execution.StreamStdout: "out", execution.StreamStderr: "err"}
	if len(reply.ExecutionOutput) != len(want) {
		t.Fatalf("inline pages = %+v", reply.ExecutionOutput)
	}
	// Exactly one page per stream: each expected stream is consumed once.
	for _, page := range reply.ExecutionOutput {
		expected, ok := want[page.Stream]
		if !ok || page.SessionID != r.SessionID || page.Instance != r.Instance || !page.EOF || !bytes.Equal(page.Bytes, []byte(expected)) {
			t.Fatalf("inline page %s = %+v", page.Stream, page)
		}
		delete(want, page.Stream)
	}
}
