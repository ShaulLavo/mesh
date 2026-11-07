package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/session/execution"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/updategate"
	"github.com/shaul/mesh/internal/worker"
)

const executionLivePhase = "live"
const executionUsageInvalidCode = "usage.invalid"

type executionError struct {
	receipt *protocol.ExecutionReceipt
	page    *protocol.ExecutionPage
	err     error
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
	key, err := l.executionKey(ctx, request.RequestKey)
	if err != nil {
		return protocol.Control{}, err
	}
	journal, err := l.executionJournal()
	if err != nil {
		return protocol.Control{}, err
	}
	if request.Type == protocol.TypeExecutionRequest {
		r, err := l.lookupExecution(ctx, journal, key)
		return executionResponse(request, r, err)
	}
	return l.executionCreateControl(ctx, request, journal, key)
}
func (l *lifecycle) executionCreateControl(ctx context.Context, request protocol.Control, journal *execution.Journal, key execution.Key) (protocol.Control, error) {
	if err := updategate.Check(filepath.Dir(l.sessionsDir)); err != nil {
		return protocol.Control{}, executionBoundaryError("execution.go executionControl", err)
	}
	intent, err := execution.Normalize(execution.Intent{Kind: request.ExecutionKind, Command: request.Command, Cwd: request.Cwd, Cols: request.Cols, Rows: request.Rows, Term: request.Term})
	if err != nil {
		return protocol.Control{}, executionBoundaryError("execution.go executionControl", err)
	}
	return l.createExecution(ctx, request, journal, key, intent)
}
func (l *lifecycle) createExecution(ctx context.Context, request protocol.Control, journal *execution.Journal, key execution.Key, intent execution.Intent) (protocol.Control, error) {
	r, owner, env, err := l.reserveExecution(ctx, journal, key, intent)
	if !owner && (err == nil || errors.Is(err, execution.ErrProgress) || errors.Is(err, execution.ErrUnknown)) {
		r, err = l.lookupExecution(ctx, journal, key)
	}
	if err != nil || !owner {
		return executionResponse(request, r, err)
	}
	// Caller loss cannot cancel work after the acceptance barrier.
	owned, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := l.cancelExecutionOnShutdown(cancel)
	defer func() { stop(); cancel() }()
	if err := journal.Dispatch(owned, r.Binding, intent); err != nil {
		return executionResponse(request, r, err)
	}
	r.Phase = "dispatched"
	r.LaunchOutcome = executionUnknownOutcome
	if err := owned.Err(); err != nil {
		return executionResponse(request, r, err)
	}
	ref := r.Binding.Reservation
	launched, err := l.launch(worker.LaunchConfig{SessionsDir: l.sessionsDir, HostID: string(l.host.ID), Executable: l.executable, Reservation: &ref, Execution: &r.Binding, Command: intent.Command, Cwd: intent.Cwd, Env: env, Cols: intent.Cols, Rows: intent.Rows, Term: intent.Term, Label: "agent request " + ref.Instance})
	if err != nil {
		return l.executionLaunchFailure(request, journal, r, err)
	}
	if launched.Meta.Reservation == nil || *launched.Meta.Reservation != ref {
		return executionResponse(request, r, execution.ErrUnknown)
	}
	r.Phase = executionLivePhase
	r.LaunchOutcome = "started"
	publishCtx, cancel := context.WithTimeout(owned, l.publishTimeout)
	defer cancel()
	if err := l.catalog.Reconcile(publishCtx); err != nil {
		return executionResponse(request, r, err)
	}
	r, err = journal.Published(owned, r.Binding)
	if err == nil {
		r, err = journal.Lookup(owned, key)
	}
	return executionResponse(request, r, err)
}
func (l *lifecycle) reserveExecution(ctx context.Context, journal *execution.Journal, key execution.Key, intent execution.Intent) (execution.Receipt, bool, []string, error) {
	var env []string
	r, owner, err := journal.Reserve(ctx, key, intent, func() (execution.Binding, error) {
		ref, err := worker.Reserve(l.sessionsDir, session.Execution)
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
	return r, owner, env, executionBoundaryError("reserve detached execution", err)
}
func (l *lifecycle) executionKey(ctx context.Context, opaque string) (execution.Key, error) {
	key := execution.Key{Host: string(l.host.ID), Opaque: opaque}
	if local, _ := ctx.Value(localClientKey{}).(bool); local {
		key.Class = "local-account"
		key.Principal = "daemon-account"
		return key, executionBoundaryError("execution.go executionKey", key.Validate())
	}
	peer, ok := transport.Peer(ctx)
	if !ok || peer.Identity == "" {
		return key, fmt.Errorf("execution: trusted connection required")
	}
	key.Class = "device"
	key.Principal = peer.Identity
	return key, executionBoundaryError("execution.go executionKey", key.Validate())
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
		return response, &executionError{receipt: response.ExecutionReceipt, err: err}
	}
	return response, nil
}
func executionWireReceipt(r execution.Receipt) *protocol.ExecutionReceipt {
	out := &protocol.ExecutionReceipt{HostID: r.Binding.Host, SessionID: r.Binding.Reservation.ID, Instance: r.Binding.Reservation.Instance, RequestKey: r.Key.Opaque, KeyScope: r.Key.Class, Kind: "pty", Phase: r.Phase, Acceptance: r.Acceptance, LaunchOutcome: r.LaunchOutcome, Publication: r.Publication, Removal: r.Removal, ReceiptRetainUntil: r.RetainUntil, RetentionReason: r.RetentionReason, OutputRetainUntil: r.OutputRetainUntil, OutputExpired: r.OutputExpired, KillRequested: r.KillRequested}
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
	if errors.As(err, &invalid) {
		return executionUsageInvalidCode
	}
	switch {
	case errors.Is(err, execution.ErrProgress):
		return "request.in_progress"
	case errors.Is(err, execution.ErrConflict):
		return "request.key_conflict"
	case errors.Is(err, execution.ErrLimit):
		return "resource.limit"
	case errors.Is(err, execution.ErrNotFound):
		return "request.not_found"
	case errors.Is(err, execution.ErrUnknown):
		return "request.outcome_unknown"
	default:
		return "state.unavailable"
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
	return executionBoundaryError("execution.go restoreExecution", journal.Maintain(ctx))
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
	if r.Phase == "terminal" {
		return r, nil
	}
	if r.Binding.Reservation.BootID != worker.BootID() {
		return uncertainExecution(r)
	}
	source, observeErr := (worker.RecoveryRuntime{SessionsDir: l.sessionsDir, HostID: string(l.host.ID)}).Inspect(ctx, r.Binding.Reservation.ID)
	if observeErr != nil || !source.Published || source.Reservation == nil || *source.Reservation != r.Binding.Reservation {
		return uncertainExecution(r)
	}
	if source.State != worker.StateRunning && source.State != worker.StateDetached {
		return uncertainExecution(r)
	}
	return l.confirmExecutionPublication(ctx, journal, key, r)
}
func uncertainExecution(r execution.Receipt) (execution.Receipt, error) {
	r.Phase = "uncertain"
	return r, execution.ErrUnknown
}
func (l *lifecycle) confirmExecutionPublication(ctx context.Context, journal *execution.Journal, key execution.Key, r execution.Receipt) (execution.Receipt, error) {
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
	if err := l.catalog.Reconcile(ctx); err != nil {
		return r, executionBoundaryError("publish execution catalog observation", err)
	}
	if _, err := journal.Published(ctx, r.Binding); err != nil {
		return r, executionBoundaryError("publish execution receipt observation", err)
	}
	result, err := journal.Lookup(ctx, key)
	return result, executionBoundaryError("read published execution receipt", err)
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
