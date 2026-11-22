package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session/execution"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/worker"
)

func (l *lifecycle) boundExecutionControl(ctx context.Context, req protocol.Control) (protocol.Control, error) {
	if err := validateRequestID(req); err != nil {
		return protocol.Control{}, executionBoundaryError("execution control", err)
	}
	if _, err := l.executionKey(ctx, "binding-admission"); err != nil {
		return protocol.Control{}, executionBoundaryError("execution control", err)
	}
	journal, err := l.executionJournal()
	if err != nil {
		return protocol.Control{}, executionBoundaryError("execution control", err)
	}
	b, err := l.executionOutput.Binding(req.SessionID, req.Instance, string(l.host.ID))
	if err != nil {
		return protocol.Control{}, executionBoundaryError("execution control", err)
	}
	if b.Mode() == execution.KindLine {
		return executionResponse(req, execution.Receipt{}, execution.ErrInstance)
	}
	if err := l.authorizeBoundExecution(ctx, req, journal, b); err != nil {
		return protocol.Control{}, executionBoundaryError("execution control", err)
	}
	r, err := journal.Exact(admittedContext(ctx), b)
	if err != nil {
		return executionResponse(req, r, err)
	}
	r, err = l.observeBoundExecution(ctx, journal, r)
	if err != nil {
		return executionResponse(req, r, err)
	}
	response, err := l.boundExecution(ctx, req, journal, r)
	if err != nil {
		return response, executionFailure(response, err)
	}
	return response, nil
}

// authorizeBoundExecution applies the owner guard to the stored receipt
// before anything refreshes or changes it. A caller that names no readable
// receipt learns nothing either.
func (l *lifecycle) authorizeBoundExecution(ctx context.Context, req protocol.Control, journal *execution.Journal, b execution.Binding) error {
	r, err := journal.Peek(ctx, b)
	if r.Key.Host != "" {
		return authorizeExecutionOwner(ctx, req, r.Key)
	}
	if req.Caller != "" || req.ExecutionContext != nil {
		return errors.Join(errCallerDenied, err)
	}
	return nil
}

func (l *lifecycle) boundExecution(ctx context.Context, req protocol.Control, j *execution.Journal, r execution.Receipt) (protocol.Control, error) {
	response, _ := executionResponse(req, r, nil)
	switch req.Type {
	case protocol.TypeExecutionInput:
		return l.executionInput(ctx, req, r, response)
	case protocol.TypeExecutionInspect:
		if r.Phase == "uncertain" {
			return response, execution.ErrUnknown
		}
		return response, nil
	case protocol.TypeExecutionLogs:
		return l.executionLogs(ctx, req, r, response)
	case protocol.TypeExecutionWait:
		return l.executionWait(ctx, req, j, r)
	case protocol.TypeExecutionSignal, protocol.TypeExecutionKill:
		if r.Phase == executionTerminalPhase {
			response.ExecutionDelivery = executionAlreadyRetired
			return response, nil
		}
		if err := workerExecutionAdmission(ctx); err != nil {
			return response, err
		}
		if err := markExecutionKill(ctx, req.Type, j, r.Binding); err != nil {
			return response, err
		}
		delivery, err := l.executionDeliver(ctx, req, r.Binding)
		response.ExecutionDelivery = delivery
		return response, executionBoundaryError("execution control", err)
	case protocol.TypeExecutionRemove:
		catalog, ok := l.catalog.(interface {
			RemoveExecution(context.Context, execution.Binding) error
		})
		if !ok {
			return response, fmt.Errorf("daemon: exact retirement unavailable")
		}
		if err := catalog.RemoveExecution(ctx, r.Binding); err != nil {
			return response, executionBoundaryError("execution control", err)
		}
		r, err := j.Exact(ctx, r.Binding)
		return executionResponse(req, r, err)
	default:
		return response, errUnknownControl
	}
}
func (l *lifecycle) executionLogs(ctx context.Context, req protocol.Control, r execution.Receipt, response protocol.Control) (protocol.Control, error) {
	page, err := l.executionLogPage(ctx, req, r)
	if page.Generation != "" {
		if r.Binding.Mode() == execution.KindPipe && r.Result != nil && r.Result.Capture == execution.CaptureNotRecorded && !page.StreamClosed {
			page.StreamClosed, page.CloseReason = true, "interrupted"
		}
		response.ExecutionPage = wireExecutionPage(page)
		if r.Binding.Mode() == execution.KindPipe && !page.Available && errors.Is(err, execution.ErrCursorExpired) {
			err = execution.ErrResultExpired
		}
	}
	return response, executionBoundaryError("execution control", err)
}
func (l *lifecycle) executionLogPage(ctx context.Context, req protocol.Control, r execution.Receipt) (execution.Page, error) {
	if !r.OutputExpired && r.Removal == "present" {
		page, err := l.executionOutput.Page(ctx, r.Binding, req.Stream, req.Cursor, req.MaxBytes)
		return page, executionBoundaryError("read execution page", err)
	}
	if r.Result != nil {
		index := slices.IndexFunc(r.Result.Streams, func(s execution.StreamResult) bool { return s.Stream == req.Stream })
		if index >= 0 {
			page, err := execution.ExpiredPage(r.Binding, r.Result.Streams[index])
			return page, executionBoundaryError("read expired execution page", err)
		}
	}
	return execution.Page{}, execution.ErrResultExpired
}

func (l *lifecycle) executionWait(ctx context.Context, req protocol.Control, j *execution.Journal, r execution.Receipt) (protocol.Control, error) {
	if req.TimeoutMillis < 1 || req.TimeoutMillis > 60000 {
		return executionResponse(req, r, fmt.Errorf("execution: invalid wait deadline"))
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(req.TimeoutMillis)*time.Millisecond)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		response, err := l.executionWaitResult(ctx, req, j, r)
		if err != nil || response.ExecutionWait == executionWaitCompleted {
			return response, executionBoundaryError("execution control", err)
		}
		select {
		case <-ctx.Done():
			response.ExecutionWait = "timed_out"
			return response, nil
		case <-ticker.C:
		}
	}
}
func (l *lifecycle) executionWaitResult(ctx context.Context, req protocol.Control, j *execution.Journal, r execution.Receipt) (protocol.Control, error) {
	response, _ := executionResponse(req, r, nil)
	if r.Removal != "present" {
		return response, execution.ErrResultExpired
	}
	if r.Phase == executionTerminalPhase {
		response.ExecutionWait = executionWaitCompleted
		return response, nil
	}
	if _, err := l.executionOutput.Result(ctx, r.Binding); err != nil {
		if execution.IsResultPending(err) {
			return response, nil
		}
		return response, executionBoundaryError("execution control", err)
	}
	r, err := j.Exact(ctx, r.Binding)
	response, _ = executionResponse(req, r, nil)
	if err == nil {
		response.ExecutionWait = executionWaitCompleted
	}
	return response, executionBoundaryError("execution control", err)
}
func (l *lifecycle) executionDeliver(ctx context.Context, req protocol.Control, b execution.Binding) (string, error) {
	if b.Reservation.BootID != worker.BootID() {
		return executionAlreadyRetired, nil
	}
	if req.Type == protocol.TypeExecutionSignal && !worker.SupportsSignal(req.Signal) {
		return "failed", fmt.Errorf("execution: invalid signal")
	}
	answer, err := worker.ExchangeExecution(ctx, l.sessionsDir, req)
	if err != nil {
		return executionUnknownOutcome, executionBoundaryError("execution delivery", err)
	}
	if answer.Type != protocol.TypeOK {
		return answer.ExecutionDelivery, fmt.Errorf("execution: signal failed: %s", answer.Message)
	}
	if answer.ExecutionDelivery != "delivered" && answer.ExecutionDelivery != executionAlreadyRetired {
		return executionUnknownOutcome, fmt.Errorf("execution: invalid delivery outcome")
	}
	return answer.ExecutionDelivery, nil
}
func wireExecutionPage(p execution.Page) *protocol.ExecutionPage {
	return &protocol.ExecutionPage{HostID: p.HostID, SessionID: p.SessionID, Instance: p.Instance, Stream: p.Stream, Generation: p.Generation, Bytes: p.Bytes, StartCursor: p.StartCursor, NextCursor: p.NextCursor, EarliestCursor: p.EarliestCursor, EndCursor: p.EndCursor, ObservedEnd: p.ObservedEnd, StreamClosed: p.StreamClosed, EOF: p.EOF, CloseReason: p.CloseReason, Loss: p.Loss, Available: p.Available, Gap: p.Gap, Reason: p.Reason}
}

func (c *Catalog) RemoveExecution(ctx context.Context, b execution.Binding) error {
	select {
	case c.reconcileGate <- struct{}{}:
		defer func() { <-c.reconcileGate }()
	case <-ctx.Done():
		return executionBoundaryError("retire execution", ctx.Err())
	}
	if ownerPinned(filepath.Join(c.sessionsDir, b.Reservation.ID)) {
		return execution.ErrProtected
	}
	j, err := execution.Open(filepath.Dir(c.sessionsDir), c.bootID(), "exact-retirement")
	if err != nil {
		return executionBoundaryError("retire execution", err)
	}
	defer func() { _ = j.Close() }()
	err = j.RetireExact(ctx, c.sessionsDir, b, func() error { return c.removeBoundSession(ctx, storage.SessionID(b.Reservation.ID)) })
	return executionBoundaryError("retire execution", err)
}

func executionOutputCode(err error) string {
	if code := applicationErrorCode(err); code != "" {
		return code
	}
	if code := execution.InputErrorCode(err); code != "" {
		return code
	}
	switch {
	case errors.Is(err, execution.ErrCursorExpired):
		return "logs.cursor_expired"
	case errors.Is(err, execution.ErrCursorInvalid):
		return "logs.cursor_invalid"
	case errors.Is(err, execution.ErrCursorFormat):
		return executionUsageInvalidCode
	case errors.Is(err, execution.ErrResultExpired):
		return "result.expired"
	case errors.Is(err, execution.ErrInstance):
		return protocol.ErrorCodeInstanceMismatch
	default:
		return ""
	}
}

const executionUnknownOutcome = "unknown"
const executionAlreadyRetired = "already_retired"

func markExecutionKill(ctx context.Context, kind string, j *execution.Journal, b execution.Binding) error {
	if kind != protocol.TypeExecutionKill {
		return nil
	}
	return executionBoundaryError("record explicit kill intent", j.MarkKill(ctx, b))
}

func (l *lifecycle) observeBoundExecution(ctx context.Context, j *execution.Journal, r execution.Receipt) (execution.Receipt, error) {
	if r.Phase == executionTerminalPhase {
		return r, nil
	}
	observed, err := l.lookupExecution(ctx, j, r.Key)
	if errors.Is(err, execution.ErrUnknown) || errors.Is(err, execution.ErrProgress) {
		return observed, nil
	}
	return observed, err
}

const executionTerminalPhase = "terminal"
const executionWaitCompleted = "completed"

func (l *lifecycle) pipeSession(id string) bool {
	if !isCanonicalSessionID(id) {
		return false
	}
	return worker.ReservedPurpose(filepath.Join(l.sessionsDir, id)).IsPipe()
}
