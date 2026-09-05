package worker

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"time"

	"github.com/shaul/mesh/internal/protocol"
)

func (w *Worker) serveNesting(conn net.Conn, request protocol.Control) {
	response := protocol.Control{
		Type: protocol.TypeNesting, RequestID: request.RequestID,
		SessionID: w.cfg.ID, NestingSupported: true,
	}
	w.mu.Lock()
	err := w.validateNestingLocked(request)
	if err != nil {
		w.mu.Unlock()
		response.Type = protocol.TypeError
		response.NestingSupported = !errors.Is(err, errNestingUnsupported)
		response.Message = fmt.Sprintf("register nested session: %v", err)
		_ = conn.SetWriteDeadline(time.Now().Add(attachmentWriteTimeout))
		_ = protocol.NewWriter(conn).WriteControlMsg(response)
		return
	}
	if w.nesting == nil {
		w.nesting = make(map[net.Conn]protocol.SessionIdentity)
	}
	w.nesting[conn] = *request.NestedSession
	w.nestReaders.Add(1)
	response.Nested = w.nestedLocked()
	w.pushNestingLocked(response.Nested)
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		delete(w.nesting, conn)
		if !w.finished {
			w.pushNestingLocked(w.nestedLocked())
		}
		w.mu.Unlock()
		w.nestReaders.Done()
	}()

	_ = conn.SetWriteDeadline(time.Now().Add(attachmentWriteTimeout))
	if err := protocol.NewWriter(conn).WriteControlMsg(response); err != nil {
		return
	}
	_ = conn.SetWriteDeadline(time.Time{})
	// No heartbeat is needed on a local socket: process death closes its file
	// descriptors, and a disconnected registration must never survive in state.
	_, _ = io.Copy(io.Discard, conn)
}

var errNestingUnsupported = errors.New("containing attachment does not support nested detach keys")

func (w *Worker) validateNestingLocked(request protocol.Control) error {
	if w.finished {
		return fmt.Errorf("session has exited")
	}
	if request.SessionID != w.cfg.ID {
		return fmt.Errorf("requested session %q from worker %s", request.SessionID, w.cfg.ID)
	}
	if request.NestedSession == nil {
		return fmt.Errorf("missing nested session identity")
	}
	if err := protocol.ValidateSessionIdentity(*request.NestedSession); err != nil {
		return err
	}
	self, err := w.sessionIdentity()
	if err != nil {
		return err
	}
	if *request.NestedSession == self {
		return fmt.Errorf("nested session is this worker %s/%s", self.HostID, self.SessionID)
	}
	if w.client != nil {
		for _, parent := range w.client.containingSessions {
			if parent == *request.NestedSession {
				return fmt.Errorf("nested session %s/%s is already a containing session", parent.HostID, parent.SessionID)
			}
		}
	}
	if len(w.nesting) >= protocol.MaxNestedSessions {
		return fmt.Errorf("at most %d nested registrations are allowed", protocol.MaxNestedSessions)
	}
	if w.client != nil && !w.client.nestingSupported {
		return errNestingUnsupported
	}
	return nil
}

func (w *Worker) validateNestingContainmentLocked(upstream []protocol.SessionIdentity) error {
	for _, parent := range upstream {
		for _, nested := range w.nesting {
			if parent == nested {
				return fmt.Errorf("containing session %s/%s is already nested in this worker", parent.HostID, parent.SessionID)
			}
		}
	}
	return nil
}

func (w *Worker) nestedLocked() []protocol.SessionIdentity {
	seen := make(map[protocol.SessionIdentity]struct{}, len(w.nesting))
	identities := make([]protocol.SessionIdentity, 0, len(w.nesting))
	for _, identity := range w.nesting {
		if _, exists := seen[identity]; !exists {
			identities = append(identities, identity)
			seen[identity] = struct{}{}
		}
	}
	slices.SortFunc(identities, func(a, b protocol.SessionIdentity) int {
		if result := cmp.Compare(a.HostID, b.HostID); result != 0 {
			return result
		}
		return cmp.Compare(a.SessionID, b.SessionID)
	})
	return identities
}

func (w *Worker) pushNestingLocked(nested []protocol.SessionIdentity) {
	if c := w.client; c != nil {
		if !c.enqueueControl(protocol.Control{
			Type: protocol.TypeNesting, SessionID: w.cfg.ID,
			Nested: nested, NestingSupported: true,
		}, false) {
			w.dropLocked(c, "")
		}
	}
}

func (w *Worker) closeNesting() {
	w.mu.Lock()
	connections := make([]net.Conn, 0, len(w.nesting))
	for conn := range w.nesting {
		connections = append(connections, conn)
	}
	w.mu.Unlock()
	for _, conn := range connections {
		_ = conn.Close()
	}
	w.nestReaders.Wait()
}
