package cli

import (
	"fmt"
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
	response, err := controlAndWait(s, protocol.Control{
		Type:      protocol.TypeKill,
		RequestID: requestID,
		SessionID: s.ID,
	})
	if err != nil {
		return err
	}
	if response.Type != protocol.TypeOK {
		return fmt.Errorf("kill %s: unexpected completion", s.ID)
	}
	return nil
}

// Logs returns bounded recent terminal output without attaching to the session.
func Logs(s Session, tail int) ([]byte, error) {
	requestID, err := newDaemonRequestID()
	if err != nil {
		return nil, err
	}
	response, err := controlAndWait(s, protocol.Control{
		Type:      protocol.TypeLogs,
		RequestID: requestID,
		SessionID: s.ID,
		Tail:      tail,
	})
	if err != nil {
		return nil, err
	}
	if response.Type != protocol.TypeLogged {
		return nil, fmt.Errorf("logs %s: unexpected completion", s.ID)
	}
	if len(response.Output) > tail {
		return nil, fmt.Errorf("logs %s: worker returned %d bytes, want at most %d", s.ID, len(response.Output), tail)
	}
	return append([]byte(nil), response.Output...), nil
}

func controlAndWait(s Session, msg protocol.Control) (protocol.Control, error) {
	response, err := exchangeWorkerControl(s, msg)
	if err != nil {
		return protocol.Control{}, err
	}
	switch response.Type {
	case protocol.TypeOK, protocol.TypeLogged:
		return response, nil
	case protocol.TypeError:
		return protocol.Control{}, daemonResponseError(msg.Type+" "+s.ID, response.Message)
	default:
		return protocol.Control{}, fmt.Errorf("%s %s: unexpected completion", msg.Type, s.ID)
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
