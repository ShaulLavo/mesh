package worker

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/protocol"
)

type lineAttachment struct {
	conn      net.Conn
	writer    *protocol.Writer
	mu        sync.Mutex
	admission sync.Mutex
	next      uint64
	fault     string
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
	a.next = *req.LastSeq
	detach, fault := w.pipe.lines.attach(a.next)
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
	w.sendLines(ctx, a, reply)
	_ = conn.Close()
	<-done
}

func (w *Worker) sendLines(ctx context.Context, a *lineAttachment, reply protocol.Control) {
	for ctx.Err() == nil {
		seq, line, fault, changed := a.takeLine(w.pipe.lines)
		if changed != nil {
			select {
			case <-changed:
				continue
			case <-ctx.Done():
				return
			}
		}
		if line == nil {
			_ = a.control(lineEnd(reply, fault))
			return
		}
		if err := a.write(func(writer *protocol.Writer) error { return writer.WriteData(w.sid, seq, line) }); err != nil {
			return
		}
	}
}

func lineEnd(reply protocol.Control, fault string) protocol.Control {
	reply.Type, reply.ErrorCode = protocol.TypeExit, fault
	if fault != "" {
		reply.Type = protocol.TypeError
	}
	return reply
}

func (a *lineAttachment) takeLine(output *lineOutput) (uint64, []byte, string, <-chan struct{}) {
	a.admission.Lock()
	defer a.admission.Unlock()
	seq := a.next
	line, fault, changed := output.readFrame(seq)
	if fault != "" {
		a.fault = fault
	}
	if line != nil {
		// The attachment holds this frame while writing it. Only subsequent
		// frames can fall out of the ring.
		a.next++
	}
	return seq, line, fault, changed
}

func (w *Worker) readLineRequests(a *lineAttachment, reader *protocol.Reader, reply protocol.Control) {
	for {
		f, err := reader.ReadFrame()
		if err != nil {
			return
		}
		response, continueReading := w.handleLineRequest(a, f, reply)
		if err := a.control(response); err != nil || !continueReading {
			return
		}
	}
}

func (w *Worker) handleLineRequest(a *lineAttachment, f protocol.Frame, reply protocol.Control) (protocol.Control, bool) {
	if f.Kind == protocol.KindInput && f.Session == w.sid {
		return w.admitLine(a, f.Payload, reply)
	}
	if f.Kind != protocol.KindControl {
		return lineInputFault(reply)
	}
	req, err := protocol.DecodeControl(f.Payload)
	if err != nil || !w.exactExecution(req) || req.InputOffset != nil {
		return lineInputFault(reply)
	}
	reply.RequestID = req.RequestID
	switch req.Type {
	case protocol.TypeWorkerLineInput:
		reply.InputReceipt, err = w.pipeInputRequest(req)
	case protocol.TypeWorkerLineRetire:
		err = w.pipe.input.retireLine(req.WriteKey)
	default:
		return lineInputFault(reply)
	}
	inputReplyError(&reply, err)
	return reply, true
}

func (w *Worker) admitLine(a *lineAttachment, payload []byte, reply protocol.Control) (protocol.Control, bool) {
	intent, data, err := protocol.DecodeLineInput(payload)
	if err != nil {
		return lineInputFault(reply)
	}
	reply.RequestID = intent.WriteKey
	a.admission.Lock()
	defer a.admission.Unlock()
	if a.fault == "" {
		a.fault = w.pipe.lines.inputFault(a.next)
	}
	if a.fault != "" {
		return lineEnd(reply, a.fault), false
	}
	reply.InputReceipt, err = w.pipe.input.admit(intent, data)
	inputReplyError(&reply, err)
	if w.pipe.input.admissionUncertain() {
		w.pipe.lines.finish("input_uncertain")
	}
	return reply, true
}

func lineInputFault(reply protocol.Control) (protocol.Control, bool) {
	reply.Type, reply.ErrorCode = protocol.TypeError, "invalid_line_input"
	return reply, false
}
