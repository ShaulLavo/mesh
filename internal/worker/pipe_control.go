package worker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/session/execution"
)

func (w *Worker) exactExecution(req protocol.Control) bool {
	return req.SessionID == w.cfg.ID && req.Instance != "" && w.cfg.Execution != nil && req.Instance == w.cfg.Execution.Reservation.Instance
}

func (w *Worker) servePipe(conn net.Conn, reader *protocol.Reader, req protocol.Control) {
	switch req.Type {
	case protocol.TypeExecutionSignal, protocol.TypeExecutionKill:
		w.executionControl(conn, req)
	case protocol.TypeWorkerPipeProbe:
		w.probePipe(conn, req)
	case protocol.TypeExecutionInput:
		w.servePipeInput(conn, req)
	case protocol.TypeWorkerLineAttach:
		w.serveLineAttach(conn, reader, req)
	default:
		reply := protocol.Control{Type: protocol.TypeError, RequestID: req.RequestID, SessionID: w.cfg.ID, ErrorCode: protocol.ErrorCodeInstanceMismatch, Message: "pipe execution requires an exact instance control"}
		_ = conn.SetWriteDeadline(time.Now().Add(attachmentWriteTimeout))
		_ = protocol.NewWriter(conn).WriteControlMsg(reply)
	}
}

func (w *Worker) servePipeInput(conn net.Conn, req protocol.Control) {
	reply := protocol.Control{Type: protocol.TypeOK, RequestID: req.RequestID, SessionID: w.cfg.ID, Instance: req.Instance}
	if !w.exactExecution(req) || w.cfg.Execution.Mode() != execution.KindPipe {
		inputReplyError(&reply, execution.ErrInstance)
	} else {
		receipt, err := w.pipeInputRequest(req)
		reply.InputReceipt = receipt
		inputReplyError(&reply, err)
	}
	_ = conn.SetWriteDeadline(time.Now().Add(attachmentWriteTimeout))
	_ = protocol.NewWriter(conn).WriteControlMsg(reply)
}

func (w *Worker) pipeInputRequest(req protocol.Control) (*session.InputReceipt, error) {
	intent, err := req.InputIntent()
	if err != nil {
		return nil, workerExecutionError("validate pipe input", err)
	}
	if w.pipe.input == nil {
		return nil, execution.ErrInputClosed
	}
	if intent == nil {
		return w.pipe.input.lookup(req.WriteKey)
	}
	return w.pipe.input.admit(*intent, req.InputBytes)
}

func inputReplyError(reply *protocol.Control, err error) {
	if err == nil {
		return
	}
	reply.Type, reply.Message = protocol.TypeError, err.Error()
	if errors.Is(err, execution.ErrInstance) {
		reply.ErrorCode = protocol.ErrorCodeInstanceMismatch
		return
	}
	reply.ErrorCode = execution.InputErrorCode(err)
	if reply.ErrorCode == "" {
		reply.ErrorCode = "usage.invalid"
	}
}

func (w *Worker) probePipe(conn net.Conn, req protocol.Control) {
	reply := protocol.Control{Type: protocol.TypeOK, RequestID: req.RequestID, SessionID: w.cfg.ID, Instance: req.Instance}
	if !w.exactExecution(req) || req.LaunchDigest != w.cfg.Execution.LaunchDigest {
		inputReplyError(&reply, execution.ErrInstance)
	}
	_ = conn.SetWriteDeadline(time.Now().Add(attachmentWriteTimeout))
	_ = protocol.NewWriter(conn).WriteControlMsg(reply)
}

// ProbePipe observes one exact published worker, independently of the daemon's
// terminal catalog and without attaching to its stdio.
func ProbePipe(ctx context.Context, sessionsDir string, b execution.Binding) error {
	dir := filepath.Join(sessionsDir, b.Reservation.ID)
	if _, err := os.Lstat(paths.Launching(dir)); !errors.Is(err, os.ErrNotExist) {
		return execution.ErrUnknown
	}
	meta, err := ReadMeta(dir)
	if err != nil {
		return fmt.Errorf("read pipe worker metadata: %w", err)
	}
	if meta.Reservation == nil || *meta.Reservation != b.Reservation || meta.ID != b.Reservation.ID || meta.ExecutionKind != b.Mode() {
		return execution.ErrInstance
	}
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	req := protocol.Control{Type: protocol.TypeWorkerPipeProbe, SessionID: b.Reservation.ID, Instance: b.Reservation.Instance, RequestID: "probe", LaunchDigest: b.LaunchDigest}
	reply, err := ExchangeExecution(ctx, sessionsDir, req)
	if err != nil {
		return err
	}
	if reply.Type != protocol.TypeOK {
		return execution.ErrUnknown
	}
	return nil
}

// ExchangeExecution sends one instance-bound request to the local worker.
func ExchangeExecution(ctx context.Context, sessionsDir string, req protocol.Control) (protocol.Control, error) {
	if id, err := session.ParseID(req.SessionID); err != nil || id != req.SessionID {
		return protocol.Control{}, execution.ErrInstance
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", paths.Socket(filepath.Join(sessionsDir, req.SessionID)))
	if err != nil {
		return protocol.Control{}, fmt.Errorf("dial execution worker: %w", err)
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if err := protocol.NewWriter(conn).WriteControlMsg(req); err != nil {
		return protocol.Control{}, fmt.Errorf("write worker request: %w", err)
	}
	frame, err := protocol.NewReader(conn).ReadFrame()
	if err != nil {
		return protocol.Control{}, fmt.Errorf("read worker reply: %w", err)
	}
	if frame.Kind != protocol.KindControl {
		return protocol.Control{}, fmt.Errorf("execution: invalid worker acknowledgement")
	}
	reply, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return protocol.Control{}, fmt.Errorf("decode worker reply: %w", err)
	}
	if reply.RequestID != req.RequestID || reply.SessionID != req.SessionID || reply.Instance != req.Instance {
		return protocol.Control{}, execution.ErrInstance
	}
	return reply, nil
}
