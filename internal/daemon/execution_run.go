package daemon

import (
	"container/list"
	"context"
	"errors"
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
	errRunEnded = errors.New("daemon: execution.run ended its connection")
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
}

type runLimiter struct {
	principal string
	limiter   *rate.Limiter
}

func newRunSupervisor() *runSupervisor {
	return &runSupervisor{byPrincipal: make(map[string]int), limiters: make(map[string]*list.Element), runs: make(map[*oneshot.Run]struct{})}
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

// track records a started run so shutdown can cancel it. A run started while
// shutdown began is cancelled at once.
func (s *runSupervisor) track(run *oneshot.Run) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		run.Cancel()
		return
	}
	s.runs[run] = struct{}{}
}

func (s *runSupervisor) forget(run *oneshot.Run) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.runs, run)
}

// abandon cancels a run nobody will hear about. Its charge is returned once
// its cleanup ended, not when its connection went away.
func (s *runSupervisor) abandon(run *oneshot.Run, release func()) {
	run.Cancel()
	go func() {
		<-run.Done()
		s.forget(run)
		release()
	}()
}

// shutdown stops admission, cancels every run and waits for their cleanup
// up to budget.
func (s *runSupervisor) shutdown(budget time.Duration) {
	s.mu.Lock()
	s.closed = true
	runs := make([]*oneshot.Run, 0, len(s.runs))
	for run := range s.runs {
		runs = append(runs, run)
	}
	s.mu.Unlock()
	for _, run := range runs {
		run.Cancel()
	}
	deadline := time.NewTimer(budget)
	defer deadline.Stop()
	for _, run := range runs {
		select {
		case <-run.Done():
		case <-deadline.C:
			log.Printf("daemon: %d request-scoped runs still cleaning up after %s", len(runs), budget)
			return
		}
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
	watched, stop := lookahead.watchAny(ctx)
	defer stop()
	started, err := s.lifecycle.startRun(watched, request)
	if err != nil && watched.Err() != nil {
		return errRunEnded
	}
	if err != nil {
		return writeRunReply(ctx, relay, runErrorReply(request, err))
	}
	if started.run == nil {
		defer started.release()
		return writeRunReply(ctx, relay, runReply(request, started))
	}
	select {
	case <-started.run.Done():
	case <-watched.Done():
		s.lifecycle.runs.abandon(started.run, started.release)
		return errRunEnded
	}
	s.lifecycle.runs.forget(started.run)
	defer started.release()
	return writeRunReply(ctx, relay, runReply(request, started))
}

// runConnectionError ends the client loop after a run: quietly when the
// run's caller left or pipelined a frame, and with the error otherwise.
func runConnectionError(ctx context.Context, err error) error {
	if errors.Is(err, errRunEnded) {
		return nil
	}
	return clientOperationError(ctx, "reply to execution.run", err)
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
	// The lock may have waited, so the caller, its grant and the update gate
	// are checked again right before anything starts.
	if err := l.runAdmission(ctx); err != nil {
		return startedRun{}, err
	}
	env := worker.RunEnvironment(withEnvOverlay(l.environment(), snapshot.Env), string(l.host.ID))
	started := startedRun{snapshot: snapshot, startedAt: time.Now()}
	started.run, started.notStarted = startRunCommand(snapshot.Intent, env, request)
	if started.run != nil {
		l.runs.track(started.run)
	}
	return started, nil
}

// startRunCommand starts the resolved command, or says why it could not:
// a start failure is the run's result, not a failed request.
func startRunCommand(intent execution.Intent, env []string, request protocol.Control) (*oneshot.Run, string) {
	path, err := oneshot.LookPath(intent.Command[0], env)
	if err != nil {
		return nil, err.Error()
	}
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

func runErrorReply(request protocol.Control, err error) protocol.Control {
	reply := clientRequestError(request, err)
	if code := runErrorCode(err); code != "" {
		reply.ErrorCode = code
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
