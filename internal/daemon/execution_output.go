package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"time"

	"github.com/shaul/mesh/internal/paths"
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
	r, err := journal.Exact(ctx, b)
	if err != nil {
		return executionResponse(req, r, err)
	}
	response, err := l.boundExecution(ctx, req, journal, r)
	if err != nil {
		return response, &executionError{receipt: response.ExecutionReceipt, page: response.ExecutionPage, err: err}
	}
	return response, nil
}
func (l *lifecycle) boundExecution(ctx context.Context, req protocol.Control, j *execution.Journal, r execution.Receipt) (protocol.Control, error) {
	response, _ := executionResponse(req, r, nil)
	switch req.Type {
	case protocol.TypeExecutionLogs:
		return l.executionLogs(ctx, req, r, response)
	case protocol.TypeExecutionWait:
		return l.executionWait(ctx, req, j, r)
	case protocol.TypeExecutionSignal, protocol.TypeExecutionKill:
		if r.Phase == "terminal" {
			response.ExecutionDelivery = executionAlreadyRetired
			return response, nil
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
	var page execution.Page
	var err error
	if r.OutputExpired || r.Removal != "present" {
		if r.Result == nil || len(r.Result.Streams) != 1 {
			return response, fmt.Errorf("execution: no durable stream identity")
		}
		page, err = execution.ExpiredPage(r.Binding, r.Result.Streams[0])
	} else {
		page, err = l.executionOutput.Page(ctx, r.Binding, req.Stream, req.Cursor, req.MaxBytes)
	}
	if page.Generation != "" {
		response.ExecutionPage = wireExecutionPage(page)
	}
	return response, executionBoundaryError("execution control", err)
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
		if err != nil || response.ExecutionWait == "completed" {
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
	if _, err := l.executionOutput.Result(ctx, r.Binding); err != nil {
		if execution.IsResultPending(err) {
			return response, nil
		}
		return response, executionBoundaryError("execution control", err)
	}
	r, err := j.Exact(ctx, r.Binding)
	response, _ = executionResponse(req, r, nil)
	if err == nil {
		response.ExecutionWait = "completed"
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
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", paths.Socket(filepath.Join(l.sessionsDir, b.Reservation.ID)))
	if err != nil {
		return executionUnknownOutcome, executionBoundaryError("execution delivery", err)
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if err := protocol.NewWriter(conn).WriteControlMsg(req); err != nil {
		return executionUnknownOutcome, executionBoundaryError("execution delivery", err)
	}
	frame, err := protocol.NewReader(conn).ReadFrame()
	if err != nil {
		return executionUnknownOutcome, executionBoundaryError("execution delivery", err)
	}
	if frame.Kind != protocol.KindControl {
		return executionUnknownOutcome, fmt.Errorf("execution: invalid worker acknowledgement")
	}
	answer, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return executionUnknownOutcome, executionBoundaryError("execution delivery", err)
	}
	if answer.RequestID != req.RequestID || answer.SessionID != req.SessionID || answer.Instance != req.Instance {
		return executionUnknownOutcome, execution.ErrInstance
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
		return "session.instance_mismatch"
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
