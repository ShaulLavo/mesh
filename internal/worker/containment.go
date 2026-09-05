package worker

import (
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/protocol"
)

func workerHostID(cfg Config) (string, error) {
	hostID := cfg.HostID
	if hostID == "" {
		for _, entry := range cfg.Env {
			if value, found := strings.CutPrefix(entry, MeshHostIDVariable+"="); found {
				hostID = value
			}
		}
	}
	if hostID == "" {
		return "", nil
	}
	if err := protocol.ValidateSessionIdentity(protocol.SessionIdentity{
		HostID: hostID, SessionID: "0000",
	}); err != nil {
		return "", fmt.Errorf("worker: invalid host identity: %w", err)
	}
	return hostID, nil
}

func (w *Worker) sessionIdentity() (protocol.SessionIdentity, error) {
	identity := protocol.SessionIdentity{HostID: w.cfg.HostID, SessionID: w.cfg.ID}
	if err := protocol.ValidateSessionIdentity(identity); err != nil {
		return protocol.SessionIdentity{}, fmt.Errorf("worker: invalid session identity: %w", err)
	}
	return identity, nil
}

func (w *Worker) validateAttachmentContainment(upstream []protocol.SessionIdentity) ([]protocol.SessionIdentity, error) {
	if err := protocol.ValidateContainingSessions(upstream); err != nil {
		return nil, err
	}
	if len(upstream) >= protocol.MaxContainingSessions {
		return nil, fmt.Errorf(
			"upstream path has %d sessions and leaves no room for this worker; maximum complete path is %d",
			len(upstream),
			protocol.MaxContainingSessions,
		)
	}
	if w.cfg.HostID != "" {
		self, err := w.sessionIdentity()
		if err != nil {
			return nil, err
		}
		for _, identity := range upstream {
			if identity == self {
				return nil, fmt.Errorf("path contains this worker %s/%s", self.HostID, self.SessionID)
			}
		}
	}
	return protocol.CloneSessionIdentities(upstream), nil
}

func (w *Worker) writeContainment(conn net.Conn, request protocol.Control) {
	response := protocol.Control{
		Type:      protocol.TypeContained,
		RequestID: request.RequestID,
		SessionID: w.cfg.ID,
	}
	self, err := w.sessionIdentity()
	if err != nil {
		response.Type = protocol.TypeError
		response.Message = err.Error()
	} else if request.SessionID != w.cfg.ID {
		response.Type = protocol.TypeError
		response.Message = fmt.Sprintf("containment requested session %q from worker %s", request.SessionID, w.cfg.ID)
	} else {
		w.mu.Lock()
		upstream := []protocol.SessionIdentity(nil)
		if w.client != nil {
			upstream = protocol.CloneSessionIdentities(w.client.containingSessions)
		}
		w.mu.Unlock()

		response.ContainingSessions = make([]protocol.SessionIdentity, 1, 1+len(upstream))
		response.ContainingSessions[0] = self
		response.ContainingSessions = append(response.ContainingSessions, upstream...)
		if err := protocol.ValidateContainingSessions(response.ContainingSessions); err != nil {
			response.Type = protocol.TypeError
			response.Message = fmt.Sprintf("worker: invalid stored containment: %v", err)
			response.ContainingSessions = nil
		}
	}

	_ = conn.SetWriteDeadline(time.Now().Add(attachmentWriteTimeout))
	defer conn.SetWriteDeadline(time.Time{}) //nolint:errcheck // one-shot connection closes next
	_ = protocol.NewWriter(conn).WriteControlMsg(response)
}
