package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session/execution"
	"github.com/shaul/mesh/internal/worker"
)

func (l *lifecycle) executionInput(ctx context.Context, req protocol.Control, r execution.Receipt, response protocol.Control) (protocol.Control, error) {
	intent, err := req.InputIntent()
	if err != nil {
		return response, executionBoundaryError("execution stdin", err)
	}
	if r.Binding.Mode() != execution.KindPipe {
		return response, execution.ErrInstance
	}
	if r.Binding.Stdin != execution.StdinOpen {
		return response, execution.ErrInputClosed
	}
	if intent == nil || r.Phase == executionTerminalPhase || r.Binding.Reservation.BootID != worker.BootID() {
		response.InputReceipt, err = l.executionOutput.Input(r.Binding, req.WriteKey, intent, req.InputBytes)
		return response, executionBoundaryError("execution stdin", err)
	}

	// Recheck the full grant immediately before reaching the worker.
	if err := workerExecutionAdmission(ctx); err != nil {
		return response, executionBoundaryError("execution stdin", err)
	}
	answer, err := worker.ExchangeExecution(ctx, l.sessionsDir, req)
	if err != nil {
		response.InputReceipt, err = l.executionOutput.Input(r.Binding, req.WriteKey, intent, req.InputBytes)
		return response, executionBoundaryError("execution stdin", err)
	}
	if answer.Type == protocol.TypeError {
		if inputErr := executionInputError(answer.ErrorCode); !errors.Is(inputErr, execution.ErrInputUnknown) {
			return response, inputErr
		}
		response.InputReceipt, err = l.executionOutput.Input(r.Binding, req.WriteKey, intent, req.InputBytes)
		return response, executionBoundaryError("recover uncertain stdin", err)
	}
	if answer.Type != protocol.TypeOK || answer.InputReceipt == nil || *answer.InputReceipt != *intent {
		return response, execution.ErrInputUnknown
	}
	response.InputReceipt = answer.InputReceipt
	return response, nil
}

func executionInputError(code string) error {
	switch code {
	case protocol.ErrorCodeInstanceMismatch:
		return execution.ErrInstance
	case "input.key_conflict":
		return execution.ErrInputConflict
	case "input.offset_mismatch":
		return execution.ErrInputOffset
	case "input.closed":
		return execution.ErrInputClosed
	case "input.backpressure":
		return execution.ErrInputFull
	case "resource.limit":
		return execution.ErrInputLimit
	default:
		return fmt.Errorf("worker input: %w", execution.ErrInputUnknown)
	}
}
