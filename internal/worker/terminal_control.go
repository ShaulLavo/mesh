package worker

import (
	"fmt"
	"net"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/session/execution"
)

// terminalInput keeps the keyed acknowledgments of agent input to a terminal
// job. It is guarded by Worker.mu, so admitting a write and a person
// attaching are one serialized decision.
type terminalInput struct {
	offset    uint64
	uncertain bool
	receipts  map[string]inputAdmission
}

// serveTerminalControl answers one instance-bound input or resize request
// for a terminal job on a one-shot connection.
func (w *Worker) serveTerminalControl(conn net.Conn, req protocol.Control) {
	reply := protocol.Control{Type: protocol.TypeOK, RequestID: req.RequestID, SessionID: w.cfg.ID, Instance: req.Instance}
	switch {
	case !w.exactExecution(req) || w.cfg.Execution.Mode() != execution.KindPTY:
		inputReplyError(&reply, execution.ErrInstance)
	case req.Type == protocol.TypeExecutionInput:
		receipt, err := w.terminalInputRequest(req)
		reply.InputReceipt = receipt
		inputReplyError(&reply, err)
	case req.Type == protocol.TypeExecutionScreen:
		// The screen read is answered by this exact instance, so a reused
		// session ID can never return another job's screen.
		if err := protocol.ValidateInspectDimensions(req.PreviewCols, req.PreviewRows); err != nil {
			inputReplyError(&reply, &protocol.ExecutionValidationError{Err: err})
			break
		}
		inspection := w.inspectSession(req.PreviewCols, req.PreviewRows)
		reply.Inspection = &inspection
	default:
		inputReplyError(&reply, w.terminalResize(req.Cols, req.Rows))
		reply.Cols, reply.Rows = req.Cols, req.Rows
	}
	_ = conn.SetWriteDeadline(time.Now().Add(attachmentWriteTimeout))
	_ = protocol.NewWriter(conn).WriteControlMsg(reply)
}

func (w *Worker) terminalInputRequest(req protocol.Control) (*session.InputReceipt, error) {
	intent, err := req.InputIntent()
	if err != nil {
		return nil, workerExecutionError("validate terminal input", err)
	}
	if intent != nil && intent.Closed {
		return nil, &protocol.ExecutionValidationError{Err: fmt.Errorf("terminal input has no end of file; send the terminal's end-of-file key (ctrl+d) as bytes")}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	state := w.terminalInputLocked()
	if intent == nil {
		return state.lookup(req.WriteKey)
	}
	// An exact retry returns its acknowledgment even after a person attached:
	// the write was admitted before they did.
	if previous, ok := state.receipts[intent.WriteKey]; ok {
		if previous.receipt != *intent || previous.digest != execution.InputDigest(req.InputBytes) {
			return nil, execution.ErrInputConflict
		}
		return intent, nil
	}
	if err := w.admitTerminalWriteLocked(state, *intent); err != nil {
		return nil, err
	}
	return w.recordTerminalInputLocked(state, *intent, req.InputBytes)
}

func (w *Worker) terminalInputLocked() *terminalInput {
	if w.agentInput == nil {
		w.agentInput = &terminalInput{receipts: make(map[string]inputAdmission)}
	}
	return w.agentInput
}

func (t *terminalInput) lookup(key string) (*session.InputReceipt, error) {
	if admission, ok := t.receipts[key]; ok {
		receipt := admission.receipt
		return &receipt, nil
	}
	return nil, execution.ErrInputUnknown
}

// admitTerminalWriteLocked decides whether a new write may enter the PTY.
// w.mu must be held, which orders it against attachment changes.
func (w *Worker) admitTerminalWriteLocked(state *terminalInput, intent session.InputReceipt) error {
	if w.finished {
		return execution.ErrTerminalClosed
	}
	if w.client != nil {
		return execution.ErrTerminalAttached
	}
	if state.uncertain {
		return execution.ErrInputUnknown
	}
	if intent.Offset != state.offset {
		return execution.ErrInputOffset
	}
	if len(state.receipts) >= pipeInputReceipts {
		return execution.ErrInputLimit
	}
	if w.input == nil {
		w.input = newInputRelay(w)
	}
	if !w.input.fits(intent.Length) {
		return execution.ErrInputFull
	}
	return nil
}

// recordTerminalInputLocked makes the acknowledgment durable before the
// bytes reach the PTY, so a lost reply is recovered by key and never
// replayed blindly.
func (w *Worker) recordTerminalInputLocked(state *terminalInput, intent session.InputReceipt, data []byte) (*session.InputReceipt, error) {
	if w.attempt == nil {
		return nil, execution.ErrInstance
	}
	digest := execution.InputDigest(data)
	stage, err := w.attempt.RecordInput(intent, digest)
	if err != nil {
		state.uncertain = true
		// A visible receipt can be confirmed by a recovering reader, so its
		// bytes are delivered even though this reply reports uncertainty.
		if stage == execution.Unconfirmed {
			state.offset += uint64(len(data))
			w.input.enqueue(nil, data)
		}
		return nil, fmt.Errorf("record terminal input: %w", err)
	}
	state.receipts[intent.WriteKey] = inputAdmission{receipt: intent, digest: digest}
	state.offset += uint64(len(data))
	if !w.input.enqueue(nil, data) {
		// fits was checked under the same lock and only the writer drains
		// the queue, so this means the relay stopped with the PTY.
		state.uncertain = true
		return nil, execution.ErrInputUnknown
	}
	return &intent, nil
}

// terminalResize changes a terminal job's size only while no person is
// attached; an attached person's terminal decides the size.
func (w *Worker) terminalResize(cols, rows int) error {
	if err := validateTerminalSize(cols, rows); err != nil {
		return &protocol.ExecutionValidationError{Err: fmt.Errorf("invalid terminal size: %w", err)}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.finished {
		return execution.ErrTerminalClosed
	}
	if w.client != nil {
		return execution.ErrTerminalAttached
	}
	return w.resizeLocked(cols, rows)
}
