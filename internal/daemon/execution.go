package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/shaul/mesh/internal/access"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session/execution"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/updategate"
	"github.com/shaul/mesh/internal/worker"
)

const executionLivePhase = "live"
const executionUsageInvalidCode = "usage.invalid"

type executionError struct {
	receipt  *protocol.ExecutionReceipt
	page     *protocol.ExecutionPage
	delivery string
	err      error
}

func executionFailure(response protocol.Control, err error) error {
	return &executionError{receipt: response.ExecutionReceipt, page: response.ExecutionPage, delivery: response.ExecutionDelivery, err: err}
}
func (e *executionError) Error() string { return e.err.Error() }
func (e *executionError) Unwrap() error { return e.err }
func (l *lifecycle) executionJournal() (*execution.Journal, error) {
	l.executionOnce.Do(func() {
		var token [16]byte
		if _, err := rand.Read(token[:]); err != nil {
			l.executionErr = err
			return
		}
		l.execution, l.executionErr = execution.Open(filepath.Dir(l.sessionsDir), worker.BootID(), hex.EncodeToString(token[:]))
		if l.executionErr == nil {
			l.executionOutput, l.executionErr = execution.OpenOutput(filepath.Dir(l.sessionsDir))
		}
	})
	return l.execution, l.executionErr
}
func (l *lifecycle) executionControl(ctx context.Context, request protocol.Control) (protocol.Control, error) {
	if err := validateRequestID(request); err != nil {
		return protocol.Control{}, err
	}
	key, err := l.executionKeyFor(ctx, request)
	if err != nil {
		return protocol.Control{}, err
	}
	journal, err := l.executionJournal()
	if err != nil {
		return protocol.Control{}, err
	}
	if request.Type == protocol.TypeExecutionRequest {
		r, err := l.lookupExecution(ctx, journal, key)
		if r.Binding.Mode() == execution.KindLine || r.Key.Class == execution.InternalServiceClass {
			return executionResponse(request, execution.Receipt{}, execution.ErrNotFound)
		}
		return executionResponse(request, r, err)
	}
	var response protocol.Control
	if key.Class == execution.ApplicationClass {
		response, err = l.contextExecutionCreate(ctx, request, journal, key)
	} else {
		response, err = l.executionCreateControl(ctx, request, journal, key)
	}
	return l.withInlineResult(ctx, request, response, err)
}
func (l *lifecycle) executionCreateControl(ctx context.Context, request protocol.Control, journal *execution.Journal, key execution.Key) (protocol.Control, error) {
	if err := updategate.Check(filepath.Dir(l.sessionsDir)); err != nil {
		return protocol.Control{}, executionBoundaryError("execution.go executionControl", err)
	}
	if request.ExecutionKind != execution.KindPTY && request.ExecutionKind != execution.KindPipe {
		return protocol.Control{}, &protocol.ExecutionValidationError{Err: fmt.Errorf("execution: choose kind pty or pipe")}
	}
	intent, err := execution.Normalize(execution.Intent{Kind: request.ExecutionKind, Command: request.Command, Cwd: request.Cwd, Cols: request.Cols, Rows: request.Rows, Term: request.Term, Stdin: request.Stdin})
	if err != nil {
		return protocol.Control{}, &protocol.ExecutionValidationError{Err: err}
	}
	return l.createExecution(ctx, request, journal, key, func() (execution.Receipt, bool, []string, execution.Intent, error) {
		r, owner, env, err := l.reserveExecution(ctx, journal, key, intent)
		return r, owner, env, intent, err
	})
}

// executionReservation reserves one keyed job. It returns the launch intent,
// which a context job resolves only while holding the journal lock.
type executionReservation func() (execution.Receipt, bool, []string, execution.Intent, error)

func (l *lifecycle) createExecution(ctx context.Context, request protocol.Control, journal *execution.Journal, key execution.Key, reserve executionReservation) (protocol.Control, error) {
	executionBeforeReserve(ctx)
	r, owner, env, intent, err := reserve()
	if !owner && (err == nil || errors.Is(err, execution.ErrProgress) || errors.Is(err, execution.ErrUnknown)) {
		r, err = l.lookupExecution(ctx, journal, key)
	}
	if err != nil || !owner {
		return executionResponse(request, r, err)
	}
	// A reservation that ends before its launch leaves a standby with its ID
	// unusable; after a launch took the standby this does nothing.
	defer l.standbys.drop(r.Binding.Reservation.ID)
	// Caller loss cannot cancel work after the acceptance barrier.
	owned, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := l.cancelExecutionOnShutdown(cancel)
	defer func() { stop(); cancel() }()
	// Both reservation paths dispatch a new job in the lock hold that
	// accepted it; a reservation that was not is dispatched here.
	if r.Phase == "reserved" {
		if err := journal.Dispatch(owned, r.Binding, intent); err != nil {
			return executionResponse(request, r, err)
		}
		r.Phase = "dispatched"
		r.LaunchOutcome = executionUnknownOutcome
	}
	if err := owned.Err(); err != nil {
		return executionResponse(request, r, err)
	}
	if err := workerExecutionAdmission(owned); err != nil {
		next, saveErr := journal.NotStarted(owned, r.Binding, "authorization_revoked")
		return executionResponse(request, next, errors.Join(err, saveErr))
	}
	executionDaemonBoundary(l.sessionsDir, "before_worker_launch", r)
	ref := r.Binding.Reservation
	launched, err := l.launch(worker.LaunchConfig{SessionsDir: l.sessionsDir, HostID: string(l.host.ID), Executable: l.executable, Reservation: &ref, Execution: &r.Binding, Command: intent.Command, Cwd: intent.Cwd, Env: env, Cols: intent.Cols, Rows: intent.Rows, Term: intent.Term, Label: "agent request " + ref.Instance, Standby: l.standbys.take(ref.ID)})
	l.standbys.refillSoon()
	if err != nil {
		return l.executionLaunchFailure(request, journal, r, err)
	}
	if launched.Meta.Reservation == nil || *launched.Meta.Reservation != ref {
		return executionResponse(request, r, execution.ErrUnknown)
	}
	r.Phase = executionLivePhase
	r.LaunchOutcome = "started"
	executionDaemonBoundary(l.sessionsDir, "before_catalog_publication", r)
	if err := l.publishExecutionCatalog(owned, r.Binding); err != nil {
		return executionResponse(request, r, err)
	}
	r, err = journal.Published(owned, r.Binding)
	return executionResponse(request, r, err)
}

// publishExecutionCatalog makes a launched terminal job visible in the session
// catalog, which is how it is attached, so its creation waits for a full
// reconcile. The catalog scan leaves pipe reservations out, and pipe jobs are
// only ever reached through their exact binding, which the worker's own
// publication already proves, so they skip the scan.
func (l *lifecycle) publishExecutionCatalog(ctx context.Context, b execution.Binding) error {
	if b.Mode() != execution.KindPTY {
		return nil
	}
	publishCtx, cancel := context.WithTimeout(ctx, l.publishTimeout)
	defer cancel()
	return executionBoundaryError("reconcile execution catalog", l.catalog.Reconcile(publishCtx))
}

var executionDaemonBoundary = func(string, string, execution.Receipt) {}

// executionBeforeReserve lets a test act on a create before its acceptance.
var executionBeforeReserve = func(context.Context) {}

func (l *lifecycle) reserveExecution(ctx context.Context, journal *execution.Journal, key execution.Key, intent execution.Intent) (execution.Receipt, bool, []string, error) {
	var env []string
	var allocated string
	r, owner, err := journal.ReserveAndDispatch(ctx, key, intent, func() (execution.Binding, error) {
		preferred := l.standbys.preferredID()
		ref, err := worker.ReservePreferring(l.sessionsDir, execution.PurposeForKind(intent.Kind), preferred)
		l.standbys.reserved(preferred, ref.ID)
		allocated = ref.ID
		if err != nil {
			return execution.Binding{}, executionBoundaryError("execution.go executionControl", err)
		}
		snapshot := l.env
		if snapshot == nil {
			snapshot = os.Environ()
		}
		env = worker.ExecutionEnvironment(snapshot, string(l.host.ID), ref, intent.Term)
		return execution.NewBinding(key, intent, ref, env), nil
	})
	if err != nil || !owner {
		// Allocated but not accepted: nothing will launch it.
		l.standbys.drop(allocated)
	}
	return r, owner, env, executionBoundaryError("reserve detached execution", err)
}
func (l *lifecycle) executionKey(ctx context.Context, opaque string) (execution.Key, error) {
	key := execution.Key{Host: string(l.host.ID), Opaque: opaque}
	if err := workerExecutionAdmission(ctx); err != nil {
		return key, err
	}
	if local, _ := ctx.Value(localClientKey{}).(bool); local {
		key.Class = executionLocalAccountClass
		key.Principal = "daemon-account"
		return key, executionBoundaryError("execution.go executionKey", key.Validate())
	}
	peer, ok := transport.Peer(ctx)
	if !ok || peer.Identity == "" {
		return key, fmt.Errorf("execution: trusted connection required")
	}
	key.Class = executionDeviceClass
	key.Principal = peer.Identity
	return key, executionBoundaryError("execution.go executionKey", key.Validate())
}

func workerExecutionAdmission(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return executionBoundaryError("execution admission", err)
	}
	if ctx.Value(localClientKey{}) == true {
		return nil
	}
	peer, ok := transport.Peer(ctx)
	if !ok || peer.Class() != access.FullDevice || !peer.Current() {
		return access.ErrDenied
	}
	return nil
}
func (l *lifecycle) executionLaunchFailure(request protocol.Control, journal *execution.Journal, r execution.Receipt, err error) (protocol.Control, error) {
	var started *worker.StartedError
	if errors.As(err, &started) {
		return executionResponse(request, r, errors.Join(execution.ErrUnknown, err))
	}
	next, saveErr := journal.NotStarted(l.context, r.Binding, "worker_start_failed")
	return executionResponse(request, next, errors.Join(err, saveErr))
}
func executionResponse(request protocol.Control, r execution.Receipt, err error) (protocol.Control, error) {
	response := protocol.Control{Type: protocol.TypeExecutionReceipt, RequestID: request.RequestID}
	if r.Binding.Reservation.Instance != "" {
		response.ExecutionReceipt = executionWireReceipt(r)
	}
	if err != nil {
		return response, executionFailure(response, err)
	}
	return response, nil
}
func executionWireReceipt(r execution.Receipt) *protocol.ExecutionReceipt {
	out := &protocol.ExecutionReceipt{HostID: r.Binding.Host, SessionID: r.Binding.Reservation.ID, Instance: r.Binding.Reservation.Instance, RequestKey: r.Key.Opaque, KeyScope: r.Key.Class, Kind: r.Binding.Mode(), Phase: r.Phase, Acceptance: r.Acceptance, LaunchOutcome: r.LaunchOutcome, Publication: r.Publication, Removal: r.Removal, ReceiptRetainUntil: r.RetainUntil, RetentionReason: r.RetentionReason, OutputRetainUntil: r.OutputRetainUntil, OutputExpired: r.OutputExpired, KillRequested: r.KillRequested, Cwd: r.Cwd}
	if !r.AcceptedAt.IsZero() {
		created := r.AcceptedAt.UTC()
		out.CreatedAt = &created
	}
	if r.Context != nil {
		revision := r.Context.Revision
		out.ContextID, out.ContextRevision = r.Context.ID, &revision
	}
	if r.Result != nil {
		t := r.Result.Termination
		out.Result = &protocol.ExecutionResult{Termination: protocol.ExecutionTermination{Kind: t.Kind, Code: t.Code, Signal: t.Signal, Reason: t.Reason}, EndedAt: r.Result.EndedAt, Capture: r.Result.Capture, Streams: wireExecutionStreams(r.Result.Streams)}
	}
	return out
}
func executionCode(err error) string {
	if code := executionOutputCode(err); code != "" {
		return code
	}
	var invalid *protocol.ExecutionValidationError
	if errors.As(err, &invalid) || errors.Is(err, execution.ErrListCursorInvalid) {
		return executionUsageInvalidCode
	}
	switch {
	case errors.Is(err, execution.ErrProgress):
		return "request.in_progress"
	case errors.Is(err, execution.ErrConflict):
		return "request.key_conflict"
	case errors.Is(err, execution.ErrLimit):
		return resourceLimitCode
	case errors.Is(err, execution.ErrNotFound):
		return "request.not_found"
	case errors.Is(err, execution.ErrUnknown):
		return "request.outcome_unknown"
	default:
		return stateUnavailableCode
	}
}

func (l *lifecycle) restoreExecution(ctx context.Context) error {
	if _, err := os.Lstat(filepath.Join(filepath.Dir(l.sessionsDir), "execution")); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return executionBoundaryError("execution.go restoreExecution", err)
	}
	journal, err := l.executionJournal()
	if err != nil {
		return err
	}
	return executionBoundaryError("execution.go restoreExecution", errors.Join(journal.Maintain(ctx), journal.MaintainContexts(ctx)))
}
func (l *lifecycle) closeExecution() {
	if l.executionOutput != nil {
		_ = l.executionOutput.Close()
	}
	if l.execution != nil {
		_ = l.execution.Close()
	}
}

func (l *lifecycle) lookupExecution(ctx context.Context, journal *execution.Journal, key execution.Key) (execution.Receipt, error) {
	r, err := journal.Lookup(ctx, key)
	if err != nil && !errors.Is(err, execution.ErrUnknown) {
		return r, executionBoundaryError("read execution receipt", err)
	}
	if r.Phase == executionTerminalPhase {
		return r, nil
	}
	if r.Binding.Reservation.BootID != worker.BootID() {
		return uncertainExecution(r)
	}
	if r.Binding.Mode() != execution.KindPTY {
		if err := worker.ProbePipe(ctx, l.sessionsDir, r.Binding); err != nil {
			// Finalization may close the socket after the preceding journal read.
			if settled, err := journal.Lookup(ctx, key); err == nil && settled.Phase == executionTerminalPhase {
				return settled, nil
			}
			return uncertainExecution(r)
		}
		return l.confirmExecutionPublication(ctx, journal, r)
	}
	source, observeErr := (worker.RecoveryRuntime{SessionsDir: l.sessionsDir, HostID: string(l.host.ID)}).Inspect(ctx, r.Binding.Reservation.ID)
	if observeErr != nil || !source.Published || source.Reservation == nil || *source.Reservation != r.Binding.Reservation {
		return uncertainExecution(r)
	}
	if source.State != worker.StateRunning && source.State != worker.StateDetached {
		return uncertainExecution(r)
	}
	return l.confirmExecutionPublication(ctx, journal, r)
}
func uncertainExecution(r execution.Receipt) (execution.Receipt, error) {
	r.Phase = "uncertain"
	return r, execution.ErrUnknown
}
func (l *lifecycle) confirmExecutionPublication(ctx context.Context, journal *execution.Journal, r execution.Receipt) (execution.Receipt, error) {
	if r.Publication == "published" && r.Phase == executionLivePhase {
		return r, nil
	}
	files, err := execution.OpenFiles(filepath.Dir(l.sessionsDir))
	if err != nil {
		return r, executionBoundaryError("open execution observation root", err)
	}
	err = errors.Join(files.Confirm(filepath.Join(filepath.Base(l.sessionsDir), r.Binding.Reservation.ID, "meta.json")), files.Close())
	if err != nil {
		return r, err
	}
	if err := l.publishExecutionCatalog(ctx, r.Binding); err != nil {
		return r, executionBoundaryError("publish execution catalog observation", err)
	}
	published, err := journal.Published(ctx, r.Binding)
	if published.Binding != r.Binding {
		published = r
	}
	return published, executionBoundaryError("publish execution receipt observation", err)
}

func executionBoundaryError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func (l *lifecycle) cancelExecutionOnShutdown(cancel context.CancelFunc) func() bool {
	return context.AfterFunc(l.context, cancel)
}

func wireExecutionStreams(streams []execution.StreamResult) []protocol.ExecutionStream {
	out := make([]protocol.ExecutionStream, 0, len(streams))
	for _, s := range streams {
		out = append(out, protocol.ExecutionStream{Stream: s.Stream, Generation: s.Generation, Earliest: s.Earliest, End: s.End, ObservedEnd: s.ObservedEnd, EOF: s.EOF, CloseReason: s.CloseReason, Loss: s.Loss})
	}
	return out
}

// The relay carries the grant of the requesting connection until the frame
// leaves its queue. This also covers partial receipts in error replies.
// executionRequestContext ends an output read when its client disconnects. A
// read may wait up to a minute for a busy stream, and on a local socket the
// client loop's context lives as long as the daemon, so a reader that left
// would otherwise keep waiting and announced until its timer ran out.
func executionRequestContext(ctx context.Context, lookahead *frameLookahead, request protocol.Control) (context.Context, context.CancelFunc) {
	if lookahead == nil {
		return ctx, func() {}
	}
	if request.Type == protocol.TypeExecutionLogs {
		return lookahead.watch(ctx)
	}
	if request.Type == protocol.TypeExecutionCreate && request.WaitMillis > 0 {
		// Only the inline observation stops when the client leaves; the
		// create keeps the connection's own cancellation rules.
		watched, cancel := lookahead.watch(ctx)
		return context.WithValue(ctx, inlineDisconnectKey{}, watched.Done()), cancel
	}
	return ctx, func() {}
}

func (s *clientServer) replyExecution(ctx context.Context, relay *clientRelay, lookahead *frameLookahead, request protocol.Control) error {
	if request.Type == protocol.TypeExecutionRun {
		return s.replyRun(ctx, relay, lookahead, request)
	}
	requestCtx, cancel := executionRequestContext(ctx, lookahead, request)
	reply, _, err := s.lifecycle.HandleControl(requestCtx, request)
	cancel()
	if err != nil {
		reply = clientRequestError(request, err)
		decorateApplicationError(&reply, request, err)
	}
	frame, err := encodeClientControl(reply)
	if err != nil {
		return executionBoundaryError("encode execution reply", err)
	}
	return relay.enqueueAdmittedResponse(frame, func() error { return workerExecutionAdmission(ctx) })
}
