package daemon

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/time/rate"

	"github.com/shaul/mesh/internal/access"
	"github.com/shaul/mesh/internal/appcaller"
	"github.com/shaul/mesh/internal/oneshot"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session/execution"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/updategate"
	"github.com/shaul/mesh/internal/worker"
)

// Limits of request-scoped runs. The rate matches the SSH server's limiter,
// since a run is the same kind of request.
const (
	maxActiveRuns       = 32
	maxRunsPerPrincipal = 8
	runRateEvery        = 100 * time.Millisecond
	runRateBurst        = 20
	maxRunLimiters      = 1024
	// runShutdownBudget bounds how long a stopping daemon waits for cancelled
	// runs to finish their cleanup.
	runShutdownBudget = 3 * time.Second
	localRunPrincipal = "local-account"
)

// runReplyWriteBudget bounds how long a reader may take to accept a reply
// before its connection closes and the run's charge is returned. A variable
// so tests can shorten it.
var runReplyWriteBudget = 10 * time.Second

var (
	errRunLimit   = errors.New("execution.run: too many runs in progress; retry when one finishes")
	errRunRate    = errors.New("execution.run: run rate limit exceeded; retry shortly")
	errRunsClosed = errors.New("execution.run: the daemon is stopping")
	// errRunEnded tells the client loop that a run's caller left or sent a
	// frame during the run, so the connection ends without a reply.
	// It wraps context.Canceled because that is what it is: the request was
	// cancelled, and the client loop ends quietly on it.
	errRunEnded = fmt.Errorf("daemon: execution.run ended its connection: %w", context.Canceled)
)

// runSupervisor admits runs and owns the started ones until their cleanup
// ends, independently of the connection that asked for them.
type runSupervisor struct {
	mu          sync.Mutex
	closed      bool
	active      int
	byPrincipal map[string]int
	limiters    map[string]*list.Element
	order       list.List
	runs        map[*oneshot.Run]struct{}
	launches    map[*runLaunch]struct{}
}

type runLimiter struct {
	principal string
	limiter   *rate.Limiter
}

func newRunSupervisor() *runSupervisor {
	return &runSupervisor{byPrincipal: make(map[string]int), limiters: make(map[string]*list.Element), runs: make(map[*oneshot.Run]struct{}), launches: make(map[*runLaunch]struct{})}
}

// admit charges one run to principal. The charge is returned by calling
// release, at most once, after the run's reply was written or discarded.
func (s *runSupervisor) admit(principal string) (release func(), err error) {
	if s == nil {
		return nil, errRunsClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errRunsClosed
	}
	if s.active >= maxActiveRuns || s.byPrincipal[principal] >= maxRunsPerPrincipal {
		return nil, errRunLimit
	}
	if !s.limiterLocked(principal).Allow() {
		return nil, errRunRate
	}
	s.active++
	s.byPrincipal[principal]++
	var once sync.Once
	return func() { once.Do(func() { s.release(principal) }) }, nil
}

func (s *runSupervisor) release(principal string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active--
	s.byPrincipal[principal]--
	if s.byPrincipal[principal] <= 0 {
		delete(s.byPrincipal, principal)
	}
}

// limiterLocked keeps at most maxRunLimiters principals, dropping the one
// least recently seen.
func (s *runSupervisor) limiterLocked(principal string) *rate.Limiter {
	if existing := s.limiters[principal]; existing != nil {
		s.order.MoveToFront(existing)
		return existing.Value.(runLimiter).limiter
	}
	entry := runLimiter{principal: principal, limiter: rate.NewLimiter(rate.Every(runRateEvery), runRateBurst)}
	s.limiters[principal] = s.order.PushFront(entry)
	if s.order.Len() > maxRunLimiters {
		oldest := s.order.Back()
		delete(s.limiters, oldest.Value.(runLimiter).principal)
		s.order.Remove(oldest)
	}
	return entry.limiter
}

// runLaunch is one run from its admission to its start. It runs off the
// connection's loop, so a caller that leaves is not held by a slow journal
// lock or PATH search, and shutdown can wait for it.
type runLaunch struct {
	ready   chan struct{}
	started startedRun
	err     error
}

// beginLaunch registers a launch before any of its work, so shutdown sees
// it.
func (s *runSupervisor) beginLaunch() (*runLaunch, error) {
	if s == nil {
		return nil, errRunsClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errRunsClosed
	}
	launch := &runLaunch{ready: make(chan struct{})}
	s.launches[launch] = struct{}{}
	return launch, nil
}

// finishLaunch records what the launch produced. A run that started after
// shutdown began goes straight to cleanup.
func (s *runSupervisor) finishLaunch(launch *runLaunch, started startedRun, err error) {
	launch.started, launch.err = started, err
	s.mu.Lock()
	delete(s.launches, launch)
	closed := s.closed
	if started.run != nil {
		s.runs[started.run] = struct{}{}
	}
	s.mu.Unlock()
	if closed && started.run != nil {
		started.run.Cancel()
	}
	close(launch.ready)
}

func (s *runSupervisor) forget(run *oneshot.Run) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.runs, run)
}

// abandonLaunch settles a launch nobody will hear about: whatever it
// started is cancelled, and its charge is returned once its cleanup ended,
// not when its connection went away.
func (s *runSupervisor) abandonLaunch(launch *runLaunch) {
	go func() {
		<-launch.ready
		s.settleAbandoned(launch)
	}()
}

func (s *runSupervisor) settleAbandoned(launch *runLaunch) {
	if launch.err != nil {
		return
	}
	if run := launch.started.run; run != nil {
		run.Cancel()
		<-run.Done()
		s.forget(run)
	}
	launch.started.release()
}

// shutdown stops admission, cancels every run, lets pending launches finish
// (their runs are cancelled as they start) and waits for all cleanup up to
// budget.
func (s *runSupervisor) shutdown(budget time.Duration) {
	s.mu.Lock()
	s.closed = true
	runs := make([]*oneshot.Run, 0, len(s.runs))
	for run := range s.runs {
		runs = append(runs, run)
	}
	launches := make([]*runLaunch, 0, len(s.launches))
	for launch := range s.launches {
		launches = append(launches, launch)
	}
	s.mu.Unlock()
	for _, run := range runs {
		run.Cancel()
	}
	deadline := time.NewTimer(budget)
	defer deadline.Stop()
	for _, launch := range launches {
		if !awaitClosed(launch.ready, deadline.C) {
			log.Printf("daemon: request-scoped runs still launching after %s", budget)
			return
		}
		if launch.started.run != nil {
			runs = append(runs, launch.started.run)
		}
	}
	for _, run := range runs {
		if !awaitClosed(run.Done(), deadline.C) {
			log.Printf("daemon: request-scoped runs still cleaning up after %s", budget)
			return
		}
	}
}

func awaitClosed(done <-chan struct{}, deadline <-chan time.Time) bool {
	select {
	case <-done:
		return true
	case <-deadline:
		return false
	}
}

// startedRun is an admitted run: started, or refused at its start with the
// reason in notStarted.
type startedRun struct {
	run        *oneshot.Run
	release    func()
	snapshot   execution.LaunchSnapshot
	startedAt  time.Time
	notStarted string
}

// replyRun serves one execution.run. The run is the connection's only
// request in flight: a disconnect or any further frame cancels it and ends
// the connection, and nothing is replied.
func (s *clientServer) replyRun(ctx context.Context, relay *clientRelay, lookahead *frameLookahead, request protocol.Control) error {
	watched, stop, commit := lookahead.watchAny(ctx)
	defer stop()
	runs := s.lifecycle.runs
	launch, err := runs.beginLaunch()
	if err != nil {
		return commitRunReply(ctx, relay, commit, runErrorReply(request, err))
	}
	go func() {
		started, err := s.lifecycle.startRun(watched, request)
		runs.finishLaunch(launch, started, err)
	}()
	if !awaitRunEvent(watched, launch.ready) {
		runs.abandonLaunch(launch)
		return errRunEnded
	}
	if launch.err != nil {
		return commitRunReply(ctx, relay, commit, runErrorReply(request, launch.err))
	}
	if run := launch.started.run; run != nil && !awaitRunEvent(watched, run.Done()) {
		runs.abandonLaunch(launch)
		return errRunEnded
	}
	if run := launch.started.run; run != nil {
		runs.forget(run)
	}
	defer launch.started.release()
	return commitRunReply(ctx, relay, commit, runReply(request, launch.started))
}

// awaitRunEvent reports whether done closed before the run's caller left or
// sent another frame.
func awaitRunEvent(watched context.Context, done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	case <-watched.Done():
		return false
	}
}

// runBeforeCommit runs just before a run's reply is committed; tests use it
// to queue a frame at that moment.
var runBeforeCommit = func() {}

// commitRunReply sends the reply only if no other frame arrived first; the
// two are mutually exclusive, and a frame that won ends the connection.
func commitRunReply(ctx context.Context, relay *clientRelay, commit func() bool, reply protocol.Control) error {
	runBeforeCommit()
	if !commit() {
		return errRunEnded
	}
	return writeRunReply(ctx, relay, reply)
}

func writeRunReply(ctx context.Context, relay *clientRelay, reply protocol.Control) error {
	frame, err := encodeClientControl(reply)
	if err != nil {
		return executionBoundaryError("encode execution.run reply", err)
	}
	return relay.writeAdmittedResponse(frame, func() error { return workerExecutionAdmission(ctx) }, runReplyWriteBudget)
}

func (l *lifecycle) startRun(ctx context.Context, request protocol.Control) (startedRun, error) {
	caller, err := l.admitRunCaller(ctx, request)
	if err != nil {
		return startedRun{}, err
	}
	job, err := runJobRequest(request)
	if err != nil {
		return startedRun{}, err
	}
	release, err := l.runs.admit(runPrincipal(ctx))
	if err != nil {
		return startedRun{}, err
	}
	started, err := l.launchRun(ctx, caller, request, job)
	if err != nil {
		release()
		return startedRun{}, err
	}
	started.release = release
	return started, nil
}

// admitRunCaller applies execution.create's checks for a context job: a
// current full device or the local account, a credential bound to that
// connection, no administration, and no update in progress.
func (l *lifecycle) admitRunCaller(ctx context.Context, request protocol.Control) (appcaller.Caller, error) {
	if err := validateRequestID(request); err != nil {
		return appcaller.Caller{}, err
	}
	caller, err := requireApplicationCaller(ctx, request)
	if err != nil {
		return appcaller.Caller{}, err
	}
	if request.Administration {
		return appcaller.Caller{}, errCallerDenied
	}
	if request.ExecutionContext == nil || !protocol.ValidContextID(request.ExecutionContext.ContextID) {
		return appcaller.Caller{}, contextUsage("execution.run needs executionContext.contextId")
	}
	return caller, l.updateGate()
}

func (l *lifecycle) updateGate() error {
	return executionBoundaryError("execution.run update gate", updategate.Check(filepath.Dir(l.sessionsDir)))
}

// runJobRequest validates the caller's part of a run exactly as a context
// pipe job with closed stdin.
func runJobRequest(request protocol.Control) (contextJobIntent, error) {
	shadow := request
	shadow.ExecutionKind, shadow.Stdin = execution.KindPipe, execution.StdinClosed
	return contextJobRequest(shadow)
}

func (l *lifecycle) launchRun(ctx context.Context, caller appcaller.Caller, request protocol.Control, job contextJobIntent) (startedRun, error) {
	journal, err := l.executionJournal()
	if err != nil {
		return startedRun{}, err
	}
	resolve := func(c execution.Context) (execution.Intent, map[string]string, error) {
		return resolveContextJob(c, job)
	}
	snapshot, err := journal.ResolveRun(admittedContext(ctx), string(caller.ID), request.ExecutionContext.ContextID, resolve)
	if err != nil {
		return startedRun{}, executionBoundaryError("resolve run context", err)
	}
	env := worker.RunEnvironment(withEnvOverlay(l.environment(), snapshot.Env), string(l.host.ID))
	started := startedRun{snapshot: snapshot, startedAt: time.Now()}
	path, reason := lookupRunCommand(snapshot.Intent, env)
	if reason != "" {
		started.notStarted = reason
		return started, nil
	}
	// The journal lock and the PATH search may both have waited, so
	// cancellation, the grant and the update gate are checked again right
	// before the spawn.
	if err := l.runAdmission(ctx); err != nil {
		return startedRun{}, err
	}
	started.run, started.notStarted = startRunCommand(path, snapshot.Intent, env, request)
	return started, nil
}

// lookupRunCommand resolves the executable, or says why it cannot be: a
// command that is not there is the run's result, not a failed request.
func lookupRunCommand(intent execution.Intent, env []string) (path, reason string) {
	path, err := oneshot.LookPath(intent.Command[0], env)
	if err != nil {
		return "", err.Error()
	}
	return path, ""
}

// startRunCommand starts the resolved command, or says why it could not.
func startRunCommand(path string, intent execution.Intent, env []string, request protocol.Control) (*oneshot.Run, string) {
	run, err := oneshot.Start(oneshot.Spec{Path: path, Args: intent.Command, Dir: intent.Cwd, Env: env, OutputBytes: request.OutputBytes, Timeout: time.Duration(request.TimeoutMillis) * time.Millisecond})
	if err != nil {
		return nil, err.Error()
	}
	return run, ""
}

func (l *lifecycle) runAdmission(ctx context.Context) error {
	if err := workerExecutionAdmission(ctx); err != nil {
		return err
	}
	return l.updateGate()
}

func runPrincipal(ctx context.Context) string {
	if local, _ := ctx.Value(localClientKey{}).(bool); local {
		return localRunPrincipal
	}
	peer, _ := transport.Peer(ctx)
	return peer.Identity
}

func runReply(request protocol.Control, s startedRun) protocol.Control {
	result := &protocol.ExecutionRunResult{Cwd: s.snapshot.Intent.Cwd, ContextRevision: s.snapshot.Context.Revision, StartedAt: s.startedAt.UTC()}
	if s.run == nil {
		result.Termination = protocol.ExecutionTermination{Kind: "not_started", Reason: s.notStarted}
		return protocol.Control{Type: protocol.TypeExecutionRunResult, RequestID: request.RequestID, ExecutionRun: result}
	}
	done := s.run.Result()
	result.Termination, result.TimedOut = runTermination(done), done.TimedOut
	result.DurationMillis = done.Duration.Milliseconds()
	result.Stdout, result.Stderr = wireRunStream(done.Stdout), wireRunStream(done.Stderr)
	return protocol.Control{Type: protocol.TypeExecutionRunResult, RequestID: request.RequestID, ExecutionRun: result}
}

func runTermination(done oneshot.Result) protocol.ExecutionTermination {
	if done.WaitErr != nil || done.State == nil {
		return protocol.ExecutionTermination{Kind: "interrupted", Reason: "wait_failed"}
	}
	if status, ok := done.State.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		signal := int(status.Signal())
		return protocol.ExecutionTermination{Kind: "signal", Signal: &signal}
	}
	code := done.State.ExitCode()
	return protocol.ExecutionTermination{Kind: "exit", Code: &code}
}

func wireRunStream(s oneshot.Stream) protocol.ExecutionRunStream {
	return protocol.ExecutionRunStream{Bytes: s.Bytes, Total: s.Total, Truncated: s.Truncated, CloseReason: s.CloseReason}
}

// runErrorReply always carries a code: a client treats a run reply it
// cannot classify as an unknown outcome, so a refusal must say it is one.
// Anything not more specific, such as an unreadable update gate, is
// state.unavailable.
func runErrorReply(request protocol.Control, err error) protocol.Control {
	reply := clientRequestError(request, err)
	if code := runErrorCode(err); code != "" {
		reply.ErrorCode = code
	}
	if reply.ErrorCode == "" {
		reply.ErrorCode = stateUnavailableCode
	}
	return reply
}

func runErrorCode(err error) string {
	switch {
	case errors.Is(err, errRunLimit), errors.Is(err, errRunRate):
		return resourceLimitCode
	case errors.Is(err, errRunsClosed), errors.Is(err, execution.ErrContextDamaged):
		return stateUnavailableCode
	case errors.Is(err, access.ErrDenied):
		return accessDeniedCode
	}
	return applicationErrorCode(err)
}
