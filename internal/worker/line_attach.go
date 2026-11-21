package worker

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/protocol"
)

type lineAttachment struct {
	conn   net.Conn
	writer *protocol.Writer
	mu     sync.Mutex
}

func (a *lineAttachment) write(send func(*protocol.Writer) error) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	_ = a.conn.SetWriteDeadline(time.Now().Add(attachmentWriteTimeout))
	return send(a.writer)
}

func (a *lineAttachment) control(reply protocol.Control) error {
	return a.write(func(w *protocol.Writer) error { return w.WriteControlMsg(reply) })
}

func (w *Worker) serveLineAttach(conn net.Conn, reader *protocol.Reader, req protocol.Control) {
	reply := protocol.Control{Type: protocol.TypeOK, SessionID: w.cfg.ID, Instance: req.Instance, LaunchDigest: req.LaunchDigest}
	a := &lineAttachment{conn: conn, writer: protocol.NewWriter(conn)}
	if !w.exactExecution(req) || w.pipe.lines == nil || req.LaunchDigest != w.cfg.Execution.LaunchDigest || req.LastSeq == nil {
		reply.Type, reply.ErrorCode = protocol.TypeError, protocol.ErrorCodeInstanceMismatch
		_ = a.control(reply)
		return
	}
	next := *req.LastSeq
	detach, fault := w.pipe.lines.attach(next)
	if fault != "" {
		reply.Type, reply.ErrorCode = protocol.TypeError, fault
		_ = a.control(reply)
		return
	}
	defer detach()
	if err := a.control(reply); err != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()
		w.readLineRequests(a, reader, reply)
	}()
	w.sendLines(ctx, a, reply, next)
	_ = conn.Close()
	<-done
}

func (w *Worker) sendLines(ctx context.Context, a *lineAttachment, reply protocol.Control, next uint64) {
	for ctx.Err() == nil {
		line, fault := w.pipe.lines.receive(ctx, next)
		if line == nil {
			reply.Type, reply.ErrorCode = protocol.TypeExit, fault
			if fault != "" {
				reply.Type = protocol.TypeError
			}
			_ = a.control(reply)
			return
		}
		err := a.write(func(writer *protocol.Writer) error { return writer.WriteData(w.sid, next, line) })
		if err != nil {
			return
		}
		next++
	}
}

func (w *Worker) readLineRequests(a *lineAttachment, reader *protocol.Reader, reply protocol.Control) {
	for {
		f, err := reader.ReadFrame()
		if err != nil {
			return
		}
		response, continueReading := w.handleLineRequest(f, reply)
		if err := a.control(response); err != nil || !continueReading {
			return
		}
	}
}

func (w *Worker) handleLineRequest(f protocol.Frame, reply protocol.Control) (protocol.Control, bool) {
	if f.Kind == protocol.KindInput && f.Session == w.sid {
		if fault := w.pipe.lines.inputFault(); fault != "" {
			reply.Type, reply.ErrorCode = protocol.TypeError, fault
			return reply, false
		}
		intent, data, err := protocol.DecodeLineInput(f.Payload)
		if err != nil {
			return lineInputFault(reply)
		}
		reply.RequestID = intent.WriteKey
		reply.InputReceipt, err = w.pipe.input.admit(intent, data)
		inputReplyError(&reply, err)
		if reply.ErrorCode == "input.outcome_unknown" {
			w.pipe.lines.finish("input_uncertain")
		}
		return reply, true
	}
	if f.Kind != protocol.KindControl {
		return lineInputFault(reply)
	}
	req, err := protocol.DecodeControl(f.Payload)
	if err != nil || req.Type != protocol.TypeWorkerLineInput || !w.exactExecution(req) || req.InputOffset != nil {
		return lineInputFault(reply)
	}
	reply.RequestID = req.RequestID
	reply.InputReceipt, err = w.pipeInputRequest(req)
	inputReplyError(&reply, err)
	return reply, true
}

func lineInputFault(reply protocol.Control) (protocol.Control, bool) {
	reply.Type, reply.ErrorCode = protocol.TypeError, "invalid_line_input"
	return reply, false
}
