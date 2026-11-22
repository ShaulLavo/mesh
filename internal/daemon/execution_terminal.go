package daemon

import (
	"context"
	"fmt"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session/execution"
	"github.com/shaul/mesh/internal/worker"
)

// Terminal jobs reach their worker only through these exact-instance
// controls, after the owner guard. They never reach a session that was not
// created as a keyed terminal job, so no agent control can type into,
// resize or attach to a person's own session.

const defaultScreenCols, defaultScreenRows = 80, 24

func terminalJobError(r execution.Receipt) error {
	if r.Binding.Mode() != execution.KindPTY {
		return &protocol.ExecutionValidationError{Err: fmt.Errorf("execution: terminal controls apply to kind pty jobs; this job is kind %s", r.Binding.Mode())}
	}
	return nil
}

// liveTerminal reports whether the job's worker can still answer: it has
// not finished and it runs in this boot.
func liveTerminal(r execution.Receipt) bool {
	return r.Phase != executionTerminalPhase && r.Binding.Reservation.BootID == worker.BootID()
}

func (l *lifecycle) executionResize(ctx context.Context, req protocol.Control, r execution.Receipt, response protocol.Control) (protocol.Control, error) {
	if err := terminalJobError(r); err != nil {
		return response, err
	}
	if !liveTerminal(r) {
		return response, execution.ErrTerminalClosed
	}
	// Recheck the full grant immediately before reaching the worker.
	if err := workerExecutionAdmission(ctx); err != nil {
		return response, executionBoundaryError("execution resize", err)
	}
	forward := protocol.Control{Type: protocol.TypeExecutionResize, RequestID: req.RequestID, SessionID: req.SessionID, Instance: req.Instance, Cols: req.Cols, Rows: req.Rows}
	answer, err := worker.ExchangeExecution(ctx, l.sessionsDir, forward)
	if err != nil {
		return response, executionBoundaryError("execution resize", err)
	}
	if answer.Type == protocol.TypeError {
		return response, workerTerminalError(answer)
	}
	response.Cols, response.Rows = answer.Cols, answer.Rows
	return response, nil
}

// workerTerminalError restores the shared error a worker reported by code.
func workerTerminalError(answer protocol.Control) error {
	switch answer.ErrorCode {
	case execution.CodeTerminalAttached:
		return execution.ErrTerminalAttached
	case execution.CodeTerminalClosed:
		return execution.ErrTerminalClosed
	case protocol.ErrorCodeInstanceMismatch:
		return execution.ErrInstance
	case executionUsageInvalidCode:
		return &protocol.ExecutionValidationError{Err: fmt.Errorf("execution: %s", answer.Message)}
	default:
		return fmt.Errorf("worker terminal control: %s", answer.Message)
	}
}

// executionScreen returns the rendered inspection of a terminal job. It reads
// the screen the way session inspection does and never changes its size.
func (l *lifecycle) executionScreen(ctx context.Context, req protocol.Control, r execution.Receipt, response protocol.Control) (protocol.Control, error) {
	if err := terminalJobError(r); err != nil {
		return response, err
	}
	cols, rows := req.PreviewCols, req.PreviewRows
	if cols == 0 && rows == 0 {
		cols, rows = defaultScreenCols, defaultScreenRows
	}
	if err := protocol.ValidateInspectDimensions(cols, rows); err != nil {
		return response, &protocol.ExecutionValidationError{Err: err}
	}
	inspect := protocol.Control{Type: protocol.TypeInspect, RequestID: req.RequestID, SessionID: req.SessionID, PreviewCols: cols, PreviewRows: rows}
	if liveTerminal(r) {
		live, err := l.forwardRead(ctx, inspect, trustedReadAdmission)
		if err == nil && live.Inspection != nil {
			response.Inspection = live.Inspection
			return response, nil
		}
	}
	saved, err := l.savedOutputPreview(ctx, inspect, trustedReadAdmission)
	if err != nil {
		return response, fmt.Errorf("execution screen %s: %w", req.SessionID, errScreenUnavailable)
	}
	inspection := protocol.SessionInspection{Preview: saved.Preview, StyledPreview: saved.StyledPreview}
	if saved.ObservedAt != nil {
		inspection.ObservedAt = *saved.ObservedAt
	}
	response.Inspection = &inspection
	return response, nil
}

var errScreenUnavailable = fmt.Errorf("no rendered screen is available for this job")
