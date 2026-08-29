package protocol

import (
	"encoding/json"
	"fmt"
)

// Control message type names.
const (
	TypeAttach   = "session.attach"
	TypeAttached = "session.attached"
	TypeDetach   = "session.detach"
	TypeExit     = "session.exit"
	TypeResize   = "terminal.resize"
	TypeSignal   = "session.signal"
	TypeKill     = "session.kill"
	TypeError    = "error"
)

// Reasons a worker ends an attachment.
const (
	ReasonStolen = "stolen" // another client attached
	ReasonClient = "client" // client asked to detach
	ReasonExited = "exited" // the session's process ended
	ReasonKilled = "killed" // the session was ended on request
)

// Control is the envelope for every JSON control message. Unused fields are
// omitted so messages stay readable on the wire during debugging.
type Control struct {
	Type      string `json:"type"`
	RequestID string `json:"requestId,omitempty"`
	SessionID string `json:"sessionId,omitempty"`

	// Attach
	LastSeq *uint64 `json:"lastSeq,omitempty"` // exact resume point; nil requests a screen snapshot
	Tail    int     `json:"tail,omitempty"`    // trailing bytes wanted by non-attach consumers
	Cols    int     `json:"cols,omitempty"`
	Rows    int     `json:"rows,omitempty"`

	// Attached
	Seq      uint64 `json:"seq,omitempty"`      // next byte offset the client will receive
	Snapshot bool   `json:"snapshot,omitempty"` // one rendered screen frame follows before live data

	// Exit / detach
	ExitCode *int   `json:"exitCode,omitempty"`
	Reason   string `json:"reason,omitempty"`

	// Signal
	Signal string `json:"signal,omitempty"`

	// Error
	Message string `json:"message,omitempty"`
}

// Encode marshals c for a control frame.
func (c Control) Encode() ([]byte, error) { return json.Marshal(c) }

// DecodeControl parses a control frame payload.
func DecodeControl(payload []byte) (Control, error) {
	var c Control
	if err := json.Unmarshal(payload, &c); err != nil {
		return c, fmt.Errorf("protocol: decode control: %w", err)
	}
	if c.Type == "" {
		return c, fmt.Errorf("protocol: control message without type")
	}
	return c, nil
}

// WriteControlMsg encodes and sends c.
func (fw *Writer) WriteControlMsg(c Control) error {
	b, err := c.Encode()
	if err != nil {
		return err
	}
	return fw.WriteControl(b)
}
