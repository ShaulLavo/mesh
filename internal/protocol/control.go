package protocol

import (
	"encoding/json"
	"fmt"
	"time"
)

// Control message type names.
const (
	TypeAttach        = "session.attach"
	TypeAttached      = "session.attached"
	TypeDetach        = "session.detach"
	TypeExit          = "session.exit"
	TypeResize        = "terminal.resize"
	TypeSignal        = "session.signal"
	TypeKill          = "session.kill"
	TypeCreate        = "session.create"
	TypeCreated       = "session.created"
	TypeList          = "session.list"
	TypeListed        = "session.listed"
	TypeLogs          = "session.logs"
	TypeLogged        = "session.logged"
	TypeHostInfo      = "host.info"
	TypeServiceUpsert = "service.upsert"
	TypeServiceList   = "service.list"
	TypeServiceDelete = "service.delete"

	TypeHostInfoResult  = "host.info.result"
	TypeServiceUpserted = "service.upserted"
	TypeServiceListed   = "service.listed"
	TypeServiceDeleted  = "service.deleted"
	TypeOK              = "ok"
	TypeError           = "error"
)

// Log request limits keep one JSON control response below MaxPayload after
// base64 encoding.
const (
	DefaultLogTail = 64 << 10
	MaxLogTail     = 1 << 20
)

// Reasons a worker ends an attachment.
const (
	ReasonStolen = "stolen" // another client attached
	ReasonClient = "client" // client asked to detach
	ReasonExited = "exited" // the session's process ended
	ReasonKilled = "killed" // the session was ended on request
)

// SessionInfo is the transport representation of durable session metadata.
type SessionInfo struct {
	ID                 string     `json:"id"`
	HostID             string     `json:"hostId"`
	Command            []string   `json:"command"`
	Cwd                string     `json:"cwd"`
	State              string     `json:"state"`
	CreatedAt          time.Time  `json:"createdAt"`
	LastAttachedAt     *time.Time `json:"lastAttachedAt,omitempty"`
	ExitCode           *int       `json:"exitCode,omitempty"`
	LastOutputSequence uint64     `json:"lastOutputSequence"`
}

// HostInfo is the transport representation of one daemon's identity.
type HostInfo struct {
	ID            string `json:"id"`
	MeshIdentity  string `json:"meshIdentity"`
	TailscaleName string `json:"tailscaleName,omitempty"`
}

// ServiceInfo is the transport representation of one origin service and its
// current health. The daemon ignores Healthy and Problem in client definitions.
type ServiceInfo struct {
	Name          string `json:"name"`
	Kind          string `json:"kind"`
	Target        string `json:"target"`
	PublicName    string `json:"publicName,omitempty"`
	WakeOnRequest bool   `json:"wakeOnRequest,omitempty"`
	Healthy       bool   `json:"healthy"`
	Problem       string `json:"problem,omitempty"`
}

// Control is the envelope for every JSON control message. Unused fields are
// omitted so messages stay readable on the wire during debugging.
type Control struct {
	Type      string `json:"type"`
	RequestID string `json:"requestId,omitempty"`
	SessionID string `json:"sessionId,omitempty"`

	// Attach / create
	LastSeq *uint64  `json:"lastSeq,omitempty"` // exact resume point; nil requests a screen snapshot
	Tail    int      `json:"tail,omitempty"`    // trailing bytes wanted by non-attach consumers
	Cols    int      `json:"cols,omitempty"`
	Rows    int      `json:"rows,omitempty"`
	Command []string `json:"command,omitempty"`
	Cwd     string   `json:"cwd,omitempty"`

	// Attached
	Seq      uint64 `json:"seq,omitempty"`      // next byte offset the client will receive
	Snapshot bool   `json:"snapshot,omitempty"` // one rendered screen frame follows before live data

	// Exit / detach
	ExitCode *int   `json:"exitCode,omitempty"`
	Reason   string `json:"reason,omitempty"`

	// Signal
	Signal string `json:"signal,omitempty"`

	// List / host info
	Sessions []SessionInfo `json:"sessions,omitempty"`
	Host     *HostInfo     `json:"host,omitempty"`

	// Logs
	Output []byte `json:"output,omitempty"`

	// Services
	ServiceName string        `json:"serviceName,omitempty"`
	Service     *ServiceInfo  `json:"service,omitempty"`
	Services    []ServiceInfo `json:"services,omitempty"`

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
