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
	return control(s, protocol.Control{
		Type:      protocol.TypeKill,
		SessionID: s.ID,
	})
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
