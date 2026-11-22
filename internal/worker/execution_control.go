package worker

import (
	"github.com/shaul/mesh/internal/protocol"
	"net"
	"time"
)

func (w *Worker) executionControl(conn net.Conn, req protocol.Control) {
	response := protocol.Control{Type: protocol.TypeOK, RequestID: req.RequestID, SessionID: w.cfg.ID, Instance: req.Instance}
	if !w.exactExecution(req) {
		response.Type = protocol.TypeError
		response.ErrorCode = protocol.ErrorCodeInstanceMismatch
		response.Message = "execution instance does not match"
		_ = protocol.NewWriter(conn).WriteControlMsg(response)
		return
	}
	if req.Type == protocol.TypeExecutionKill {
		w.mu.Lock()
		waiting := !w.finished
		if waiting {
			w.killResponders.Add(1)
		}
		w.mu.Unlock()
		if waiting {
			defer w.killResponders.Done()
		}
	}
	name := req.Signal
	if req.Type == protocol.TypeExecutionKill {
		name = "hup"
		if w.pipe != nil {
			w.pipe.cancelling.Store(true)
		}
	}
	delivery, err := w.deliverSignal(req.Instance, name)
	response.ExecutionDelivery = delivery
	if err != nil {
		response.Type = protocol.TypeError
		response.Message = err.Error()
	}
	if err == nil && req.Type == protocol.TypeExecutionKill && delivery == "delivered" {
		if w.pipe == nil {
			w.kill()
		} else {
			w.escalatePipe(req.Instance)
		}
	}
	_ = conn.SetWriteDeadline(time.Now().Add(attachmentWriteTimeout))
	_ = protocol.NewWriter(conn).WriteControlMsg(response)
}

func (w *Worker) escalatePipe(instance string) {
	timer := time.NewTimer(killGrace)
	defer timer.Stop()
	select {
	case <-w.exited:
		return
	case <-timer.C:
	}
	_, _ = w.deliverSignal(instance, "kill")
	<-w.exited
}
