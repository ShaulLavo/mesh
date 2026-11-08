package worker

import (
	"github.com/shaul/mesh/internal/protocol"
	"net"
	"time"
)

func (w *Worker) executionControl(conn net.Conn, req protocol.Control) {
	response := protocol.Control{Type: protocol.TypeOK, RequestID: req.RequestID, SessionID: w.cfg.ID, Instance: req.Instance}
	if req.SessionID != w.cfg.ID || req.Instance == "" || w.cfg.Execution == nil || req.Instance != w.cfg.Execution.Reservation.Instance {
		response.Type = protocol.TypeError
		response.ErrorCode = "session.instance_mismatch"
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
	}
	delivery, err := w.deliverSignal(req.Instance, name)
	response.ExecutionDelivery = delivery
	if err != nil {
		response.Type = protocol.TypeError
		response.Message = err.Error()
	}
	if err == nil && req.Type == protocol.TypeExecutionKill && delivery == "delivered" {
		w.kill()
	}
	_ = conn.SetWriteDeadline(time.Now().Add(attachmentWriteTimeout))
	_ = protocol.NewWriter(conn).WriteControlMsg(response)
}
