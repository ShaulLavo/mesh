package cli

import (
	"fmt"
	"io"
	"net"
	"time"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
)

// Kill ends a session. It is not a signal: it means the session is over, and
// the worker escalates until that is true.
func Kill(s Session) error {
	requestID, err := newDaemonRequestID()
	if err != nil {
		return err
	}
	return controlAndWait(s, protocol.Control{
		Type:      protocol.TypeKill,
		RequestID: requestID,
		SessionID: s.ID,
	})
}

func controlAndWait(s Session, msg protocol.Control) error {
	conn, err := net.DialTimeout("unix", paths.Socket(s.Dir), 2*time.Second)
	if err != nil {
		return fmt.Errorf("%s %s: %w", msg.Type, s.ID, err)
	}
	defer conn.Close() //nolint:errcheck // one-shot connection
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return fmt.Errorf("%s %s: set completion deadline: %w", msg.Type, s.ID, err)
	}

	if err := protocol.NewWriter(conn).WriteControlMsg(msg); err != nil {
		return fmt.Errorf("%s %s: %w", msg.Type, s.ID, err)
	}
	frame, err := protocol.NewReader(conn).ReadFrame()
	if err != nil {
		if err == io.EOF {
			return fmt.Errorf("%s %s: worker closed without confirming completion", msg.Type, s.ID)
		}
		return fmt.Errorf("%s %s: read completion: %w", msg.Type, s.ID, err)
	}
	if frame.Kind != protocol.KindControl {
		return fmt.Errorf("%s %s: completion frame has kind %d", msg.Type, s.ID, frame.Kind)
	}
	response, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return fmt.Errorf("%s %s: decode completion: %w", msg.Type, s.ID, err)
	}
	if response.RequestID != msg.RequestID || response.SessionID != s.ID {
		return fmt.Errorf("%s %s: mismatched completion", msg.Type, s.ID)
	}
	switch response.Type {
	case protocol.TypeOK:
		return nil
	case protocol.TypeError:
		return fmt.Errorf("%s %s: %s", msg.Type, s.ID, response.Message)
	default:
		return fmt.Errorf("%s %s: unexpected completion %q", msg.Type, s.ID, response.Type)
	}
}

// Signal delivers a signal to a session's process group without attaching, so
// it works while someone else is using the session and while nobody is.
func Signal(s Session, name string) error {
	return control(s, protocol.Control{
		Type:      protocol.TypeSignal,
		SessionID: s.ID,
		Signal:    name,
	})
}

// control opens a one-shot connection that issues a single command without
// attaching, so it never disturbs whoever is currently using the session.
func control(s Session, msg protocol.Control) error {
	conn, err := net.DialTimeout("unix", paths.Socket(s.Dir), 2*time.Second)
	if err != nil {
		return fmt.Errorf("%s %s: %w", msg.Type, s.ID, err)
	}
	defer conn.Close() //nolint:errcheck // one-shot connection

	if err := protocol.NewWriter(conn).WriteControlMsg(msg); err != nil {
		return fmt.Errorf("%s %s: %w", msg.Type, s.ID, err)
	}
	return nil
}
