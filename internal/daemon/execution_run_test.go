package daemon

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/oneshot"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/updategate"
	"github.com/shaul/mesh/internal/worker"
)

// runHangCap only catches a run or reply that never comes.
const runHangCap = 30 * time.Second

type runFixture struct {
	t         *testing.T
	l         *lifecycle
	ctx       context.Context
	root      string
	caller    protocol.CallerCredential
	contextID string
}

func newRunFixture(t *testing.T) *runFixture {
	t.Helper()
	env := []string{"PATH=/bin:/usr/bin", "MESH_SESSION_ID=leak", "MESH_DEPTH=3", "MESH_TERMINAL_ID=tab", "MESH_HOST_ID=stale"}
	l, err := newLifecycle(lifecycleConfig{Catalog: &lifecycleTestCatalog{}, Connector: failingLifecycleConnector(), Host: storage.Host{ID: "host", MeshIdentity: "host"}, SessionsDir: filepath.Join(t.TempDir(), "s"), Env: env, Launch: func(worker.LaunchConfig) (worker.Launched, error) {
		return worker.Launched{}, errors.New("a run never launches a worker")
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.closeExecution)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &runFixture{t: t, l: l, ctx: localServerContext(t.Context()), root: root}
	f.caller, _ = testCaller(t)
	f.contextID = f.open(f.caller)
	return f
}

func (f *runFixture) open(caller protocol.CallerCredential) string {
	f.t.Helper()
	open := protocol.Control{Type: protocol.TypeContextOpen, RequestID: "open", RequestKey: "workspace", Caller: caller, ExecutionContext: &protocol.ExecutionContextRequest{Open: &protocol.ContextSpec{Cwd: f.root, Shell: protocol.ContextShellSh}}}
	reply, err := f.l.contextControl(f.ctx, open)
	if err != nil || reply.ContextInfo == nil {
		f.t.Fatalf("open context: %v", err)
	}
	return reply.ContextInfo.ContextID
}

func (f *runFixture) request(script string) protocol.Control {
	return protocol.Control{Type: protocol.TypeExecutionRun, RequestID: "run", Caller: f.caller, TimeoutMillis: 10000, OutputBytes: 4096, ExecutionContext: &protocol.ExecutionContextRequest{ContextID: f.contextID, ShellCommand: script}}
}

// serve runs the client server on one test connection.
func (f *runFixture) serve() (*serverTestConn, <-chan error) {
	f.t.Helper()
	server, err := newClientServer(f.l, &contextWorkerCounter{}, &contextControlCounter{}, disabledCertificateController{})
	if err != nil {
		f.t.Fatal(err)
	}
	client := newServerTestConn()
	done := make(chan error, 1)
	go func() { done <- server.Handle(f.ctx, client) }()
	return client, done
}

func (f *runFixture) call(request protocol.Control) protocol.Control {
	f.t.Helper()
	client, done := f.serve()
	client.pushRead(serverControlFrame(f.t, request))
	reply := awaitRunWrite(f.t, client)
	client.pushReadError(io.EOF)
	if err := waitServerResult(f.t, done, "run server"); err != nil {
		f.t.Fatal(err)
	}
	return reply
}

func awaitRunWrite(t *testing.T, client *serverTestConn) protocol.Control {
	t.Helper()
	select {
	case frame := <-client.writes:
		return decodeServerControl(t, frame)
	case <-time.After(runHangCap):
		t.Fatal("no execution.run reply")
		return protocol.Control{}
	}
}

func runResult(t *testing.T, reply protocol.Control) *protocol.ExecutionRunResult {
	t.Helper()
	if reply.Type != protocol.TypeExecutionRunResult || reply.ExecutionRun == nil {
		t.Fatalf("reply = %+v", reply)
	}
	return reply.ExecutionRun
}

func runCode(t *testing.T, reply protocol.Control) string {
	t.Helper()
	if reply.Type != protocol.TypeError {
		t.Fatalf("reply = %+v, want an error", reply)
	}
	return reply.ErrorCode
}

func TestRunReturnsItsResultInOneReply(t *testing.T) {
	f := newRunFixture(t)
	if err := os.Mkdir(filepath.Join(f.root, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	request := f.request(`printf '%s|%s|%s|%s' "$PWD" "$MESH_HOST_ID" "${MESH_SESSION_ID-}${MESH_DEPTH-}${MESH_TERMINAL_ID-}" "$RUN_VAR"; printf e >&2; exit 3`)
	request.Cwd = "sub"
	request.ExecutionContext.Env = map[string]string{"RUN_VAR": "v"}
	result := runResult(t, f.call(request))
	want := filepath.Join(f.root, "sub") + "|host||v"
	if string(result.Stdout.Bytes) != want || string(result.Stderr.Bytes) != "e" {
		t.Fatalf("output = %q / %q, want %q", result.Stdout.Bytes, result.Stderr.Bytes, want)
	}
	if result.Termination.Kind != "exit" || result.Termination.Code == nil || *result.Termination.Code != 3 || result.TimedOut {
		t.Fatalf("termination = %+v timedOut=%t", result.Termination, result.TimedOut)
	}
	if result.Cwd != filepath.Join(f.root, "sub") || result.ContextRevision != 1 || result.Stdout.CloseReason != oneshot.CloseEOF {
		t.Fatalf("result = %+v", result)
	}
}

func TestRunReportsTruncationAndTheDeadline(t *testing.T) {
	f := newRunFixture(t)
	request := f.request("head -c 10000 /dev/zero; exec sleep 30")
	request.TimeoutMillis, request.OutputBytes = 200, 100
	result := runResult(t, f.call(request))
	if !result.TimedOut || result.Termination.Kind != "signal" || result.Termination.Signal == nil || *result.Termination.Signal != int(syscall.SIGTERM) {
		t.Fatalf("deadline result = %+v", result)
	}
	if len(result.Stdout.Bytes) != 100 || result.Stdout.Total != 10000 || !result.Stdout.Truncated {
		t.Fatalf("stdout kept %d of %d", len(result.Stdout.Bytes), result.Stdout.Total)
	}
}

func TestRunResolvesArgvInTheRunPath(t *testing.T) {
	f := newRunFixture(t)
	tools := t.TempDir()
	if err := os.WriteFile(filepath.Join(tools, "only-here"), []byte("#!/bin/sh\necho found\n"), 0o700); err != nil { //nolint:gosec // the fixture tool must be executable
		t.Fatal(err)
	}
	request := f.request("")
	request.ExecutionContext.ShellCommand, request.Command = "", []string{"only-here"}
	request.ExecutionContext.Env = map[string]string{"PATH": tools + ":/bin:/usr/bin"}
	if result := runResult(t, f.call(request)); string(result.Stdout.Bytes) != "found\n" {
		t.Fatalf("argv run = %+v", result)
	}
	request.ExecutionContext.Env = nil
	result := runResult(t, f.call(request))
	if result.Termination.Kind != "not_started" || result.Termination.Reason == "" {
		t.Fatalf("a tool outside the run's PATH = %+v", result)
	}
}

func TestRunAuthorizesLikeAContextJob(t *testing.T) {
	f := newRunFixture(t)
	other, _ := testCaller(t)
	stranger := f.request("true")
	stranger.Caller = other
	if code := runCode(t, f.call(stranger)); code != "context.not_found" {
		t.Fatalf("another caller's context = %q", code)
	}
	admin := f.request("true")
	admin.Administration = true
	// The wire refuses the field before the daemon would deny it.
	if code := runCode(t, f.call(admin)); code != executionUsageInvalidCode {
		t.Fatalf("administration = %q", code)
	}
	if _, err := f.l.startRun(t.Context(), f.request("true")); !errors.Is(err, errCallerDenied) && runErrorCode(err) != accessDeniedCode {
		t.Fatalf("a connection that is neither local nor a current device = %v", err)
	}
	removeMembership(t, f)
	if code := runCode(t, f.call(f.request("true"))); code != stateUnavailableCode {
		t.Fatalf("damaged context = %q", code)
	}
}

func TestRunRefusesAClosedContext(t *testing.T) {
	f := newRunFixture(t)
	closeRequest := protocol.Control{Type: protocol.TypeContextClose, RequestID: "close", Caller: f.caller, ExecutionContext: &protocol.ExecutionContextRequest{ContextID: f.contextID}}
	if _, err := f.l.contextControl(f.ctx, closeRequest); err != nil {
		t.Fatal(err)
	}
	if code := runCode(t, f.call(f.request("true"))); code != "context.closed" {
		t.Fatalf("closed context = %q", code)
	}
}

// removeMembership deletes the context's membership directory, the damage a
// keyed job also refuses.
func removeMembership(t *testing.T, f *runFixture) {
	t.Helper()
	state := filepath.Join(filepath.Dir(f.l.sessionsDir), "execution")
	var members string
	_ = filepath.WalkDir(state, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && entry.IsDir() && entry.Name() == f.contextID {
			members = path
		}
		return nil
	})
	if members == "" || os.RemoveAll(members) != nil {
		t.Fatalf("no membership directory for %s under %s", f.contextID, state)
	}
}

func TestRunDisconnectKillsTheCommand(t *testing.T) {
	f := newRunFixture(t)
	pidFile := filepath.Join(f.root, "pid")
	client, done := f.serve()
	client.pushRead(serverControlFrame(t, f.request("echo $$ > pid; exec sleep 30")))
	pid := readRunPID(t, pidFile)
	client.pushReadError(io.EOF)
	if err := waitServerResult(t, done, "run server"); err != nil {
		t.Fatal(err)
	}
	assertRunGone(t, pid)
	assertNoRunReply(t, client)
	assertRunsReleased(t, f.l.runs)
}

func TestRunRefusesAPipelinedFrame(t *testing.T) {
	f := newRunFixture(t)
	pidFile := filepath.Join(f.root, "pid")
	client, done := f.serve()
	client.pushRead(serverControlFrame(t, f.request("echo $$ > pid; exec sleep 30")))
	pid := readRunPID(t, pidFile)
	client.pushRead(serverControlFrame(t, f.request("true")))
	if err := waitServerResult(t, done, "run server"); err != nil {
		t.Fatal(err)
	}
	assertRunGone(t, pid)
	assertNoRunReply(t, client)
}

func TestRunReplyToASlowReaderIsBounded(t *testing.T) {
	f := newRunFixture(t)
	saved := runReplyWriteBudget
	runReplyWriteBudget = 100 * time.Millisecond
	t.Cleanup(func() { runReplyWriteBudget = saved })
	client, done := f.serve()
	entered, release := client.blockNextWrite()
	defer release()
	client.pushRead(serverControlFrame(t, f.request("echo hi")))
	select {
	case <-entered:
	case <-time.After(runHangCap):
		t.Fatal("the reply was never written")
	}
	client.waitClosed(t)
	if err := waitServerResult(t, done, "run server"); err != nil {
		t.Logf("server after a slow reader: %v", err)
	}
	assertRunsReleased(t, f.l.runs)
}

func TestRunSupervisorLimits(t *testing.T) {
	s := newRunSupervisor()
	releases := make([]func(), 0, maxRunsPerPrincipal)
	for range maxRunsPerPrincipal {
		release, err := s.admit("a")
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	if _, err := s.admit("a"); !errors.Is(err, errRunLimit) {
		t.Fatalf("over the principal limit = %v", err)
	}
	other, err := s.admit("b")
	if err != nil {
		t.Fatalf("another principal = %v", err)
	}
	other()
	other()
	releases[0]()
	if _, err := s.admit("a"); err != nil {
		t.Fatalf("after a release = %v", err)
	}
	burst := newRunSupervisor()
	for range runRateBurst {
		release, err := burst.admit("c")
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
	if _, err := burst.admit("c"); !errors.Is(err, errRunRate) || runErrorCode(err) != resourceLimitCode {
		t.Fatalf("over the burst = %v", err)
	}
}

func TestRunShutdownCancelsRunsAndStopsAdmission(t *testing.T) {
	s := newRunSupervisor()
	run, err := oneshot.Start(oneshot.Spec{Path: "/bin/sh", Args: []string{"/bin/sh", "-c", "exec sleep 30"}, Dir: t.TempDir(), OutputBytes: 16, Timeout: runHangCap})
	if err != nil {
		t.Fatal(err)
	}
	launch, err := s.beginLaunch()
	if err != nil {
		t.Fatal(err)
	}
	s.finishLaunch(launch, startedRun{run: run, release: func() {}}, nil)
	s.shutdown(runHangCap)
	select {
	case <-run.Done():
	default:
		t.Fatal("shutdown returned before the run's cleanup ended")
	}
	if _, err := s.admit("a"); !errors.Is(err, errRunsClosed) {
		t.Fatalf("admission after shutdown = %v", err)
	}
	if _, err := s.beginLaunch(); !errors.Is(err, errRunsClosed) {
		t.Fatalf("launch after shutdown = %v", err)
	}
}

// A launch still resolving when shutdown begins is waited for, and the run
// it then starts goes straight to cleanup.
func TestRunShutdownWaitsForAPendingLaunch(t *testing.T) {
	s := newRunSupervisor()
	launch, err := s.beginLaunch()
	if err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	go func() {
		s.shutdown(runHangCap)
		close(stopped)
	}()
	eventuallyClosed(t, s)
	run, err := oneshot.Start(oneshot.Spec{Path: "/bin/sh", Args: []string{"/bin/sh", "-c", "exec sleep 30"}, Dir: t.TempDir(), OutputBytes: 16, Timeout: runHangCap})
	if err != nil {
		t.Fatal(err)
	}
	s.finishLaunch(launch, startedRun{run: run, release: func() {}}, nil)
	select {
	case <-stopped:
	case <-time.After(runHangCap):
		t.Fatal("shutdown never returned")
	}
	select {
	case <-run.Done():
	default:
		t.Fatal("shutdown returned before the late run was cleaned up")
	}
}

func eventuallyClosed(t *testing.T, s *runSupervisor) {
	t.Helper()
	deadline := time.Now().Add(runHangCap)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		closed := s.closed
		s.mu.Unlock()
		if closed {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("shutdown never closed admission")
}

// A daemon stop during a run cancels the connection and the run: the
// command's group is cleaned up and its charge returned.
func TestRunDaemonStopCleansUpTheGroup(t *testing.T) {
	f := newRunFixture(t)
	ctx, cancel := context.WithCancel(f.ctx)
	f.ctx = ctx
	client, done := f.serve()
	client.pushRead(serverControlFrame(t, f.request("echo $$ > pid; exec sleep 30")))
	pid := readRunPID(t, filepath.Join(f.root, "pid"))
	cancel()
	f.l.runs.shutdown(runHangCap)
	if err := waitServerResult(t, done, "run server"); err != nil {
		t.Logf("server after daemon stop: %v", err)
	}
	assertRunGone(t, pid)
	assertRunsReleased(t, f.l.runs)
}

// A frame queued while the reply is about to be committed wins: the run's
// reply is not sent and the connection ends, never both.
func TestRunFrameBeforeCommitEndsTheConnection(t *testing.T) {
	f := newRunFixture(t)
	claimed := make(chan int, 1)
	publish := make(chan struct{})
	committing := make(chan struct{})
	// The watcher pauses after claiming the second frame and before
	// publishing it, the window in which the reply must not commit. Nothing
	// may be pending yet: a watcher that published first is the old race.
	// Watchers of other, closing connections see read errors; only the
	// frame this test queues is held.
	frameClaimed := func(pending chan lookaheadRead, err error) {
		if err != nil {
			return
		}
		select {
		case claimed <- len(pending):
		default:
		}
		<-publish
	}
	pendingAtClaim := make(chan int, 1)
	beforeCommit := func() {
		close(committing)
		select {
		case queued := <-claimed:
			pendingAtClaim <- queued
		case <-time.After(runHangCap):
			pendingAtClaim <- -1
		}
	}
	lookaheadFrameClaimedHook.Store(&frameClaimed)
	runBeforeCommitHook.Store(&beforeCommit)
	t.Cleanup(func() {
		lookaheadFrameClaimedHook.Store(nil)
		runBeforeCommitHook.Store(nil)
	})
	defer close(publish)
	client, done := f.serve()
	client.pushRead(serverControlFrame(t, f.request("true")))
	waitSignal(t, committing, "the reply commit")
	client.pushRead(serverControlFrame(t, f.request("true")))
	if err := waitServerResult(t, done, "run server"); err != nil {
		t.Fatal(err)
	}
	if queued := <-pendingAtClaim; queued != 0 {
		t.Fatalf("the frame was published before its interruption was claimed (%d pending)", queued)
	}
	assertNoRunReply(t, client)
	assertRunsReleased(t, f.l.runs)
}

// A broken stream during a run is a fault the client loop reports, not a
// caller that left; the run is still cleaned up.
func TestRunInvalidFrameEndsTheConnectionWithAnError(t *testing.T) {
	f := newRunFixture(t)
	client, done := f.serve()
	client.pushRead(serverControlFrame(t, f.request("echo $$ > pid; exec sleep 30")))
	pid := readRunPID(t, filepath.Join(f.root, "pid"))
	client.pushReadError(transport.ErrInvalidFrame)
	select {
	case err := <-done:
		if !errors.Is(err, transport.ErrInvalidFrame) {
			t.Fatalf("client loop after an invalid frame = %v", err)
		}
	case <-time.After(runHangCap):
		t.Fatal("the client loop never ended")
	}
	assertRunGone(t, pid)
	assertRunsReleased(t, f.l.runs)
}

// A refusal the daemon has no specific code for still carries one, so the
// client can tell it from an unknown outcome.
func TestRunRefusalWithoutASpecificCodeIsStillCoded(t *testing.T) {
	f := newRunFixture(t)
	gate := updategate.Path(filepath.Dir(f.l.sessionsDir))
	if err := os.MkdirAll(gate, 0o700); err != nil {
		t.Fatal(err)
	}
	reply := f.call(f.request("true"))
	if code := runCode(t, reply); code != stateUnavailableCode || reply.Message == "" {
		t.Fatalf("unreadable update gate = %+v", reply)
	}
}

func TestRunValidationRejectsOtherFields(t *testing.T) {
	f := newRunFixture(t)
	for name, mutate := range map[string]func(*protocol.Control){
		"stdin":         func(c *protocol.Control) { c.Stdin = "open" },
		"request key":   func(c *protocol.Control) { c.RequestKey = "k" },
		"short timeout": func(c *protocol.Control) { c.TimeoutMillis = 99 },
		"large output":  func(c *protocol.Control) { c.OutputBytes = protocol.MaxExecutionRunOutput + 1 },
		"context revision": func(c *protocol.Control) {
			revision := uint64(1)
			c.ExecutionContext.ExpectedRevision = &revision
		},
	} {
		request := f.request("true")
		mutate(&request)
		encoded, err := request.Encode()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := protocol.DecodeControl(encoded); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func readRunPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(runHangCap)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path) //nolint:gosec // the pid file belongs to this test's temporary directory
		if pid, convErr := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && convErr == nil {
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no pid in %s", path)
	return 0
}

func assertRunGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(runHangCap)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("run %d outlived its connection", pid)
}

func assertNoRunReply(t *testing.T, client *serverTestConn) {
	t.Helper()
	select {
	case frame := <-client.writes:
		t.Fatalf("a cancelled run replied: %+v", decodeServerControl(t, frame))
	default:
	}
}

// assertRunsReleased waits for an abandoned run to return its charge.
func assertRunsReleased(t *testing.T, s *runSupervisor) {
	t.Helper()
	deadline := time.Now().Add(runHangCap)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		active, tracked := s.active, len(s.runs)
		s.mu.Unlock()
		if active == 0 && tracked == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("an abandoned run kept its charge")
}
