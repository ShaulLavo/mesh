package transport

import (
	"fmt"

	"github.com/shaul/mesh/internal/protocol"
)

// WriteAdmittedControl rechecks admission after any transport write ordering
// or buffered data flush. Ordinary terminal frames use the normal write path.
func WriteAdmittedControl(conn Conn, frame protocol.Frame, admit func() error) error {
	if frame.Kind != protocol.KindControl || admit == nil {
		return fmt.Errorf("transport: control frame and delivery admission required")
	}
	if guarded, ok := conn.(interface {
		writeAdmittedControl(protocol.Frame, func() error) error
	}); ok {
		return guarded.writeAdmittedControl(frame, admit)
	}
	if err := admit(); err != nil {
		return err
	}
	if err := conn.WriteFrame(frame); err != nil {
		return fmt.Errorf("transport: write admitted control: %w", err)
	}
	return nil
}
