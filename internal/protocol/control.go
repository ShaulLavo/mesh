package protocol

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/shaul/mesh/internal/recovery"
)

// Control message type names.
const (
	TypeAttach             = "session.attach"
	TypeAttachDetached     = "session.attach-detached"
	TypeAttached           = "session.attached"
	TypeDetach             = "session.detach"
	TypeExit               = "session.exit"
	TypeResize             = "terminal.resize"
	TypeSignal             = "session.signal"
	TypeKill               = "session.kill"
	TypeCreate             = "session.create"
	TypeCreated            = "session.created"
	TypeList               = "session.list"
	TypeListed             = "session.listed"
	TypeInspect            = "session.inspect"
	TypeInspected          = "session.inspected"
	TypeContainment        = "session.containment"
	TypeContained          = "session.contained"
	TypeNest               = "session.nest"
	TypeNesting            = "session.nesting"
	TypeLogs               = "session.logs"
	TypeRemove             = "session.remove"
	TypeLogged             = "session.logged"
	TypeHostInfo           = "host.info"
	TypeServicePreview     = "service.preview"
	TypeServicePreviewed   = "service.previewed"
	TypeServiceUpsert      = "service.upsert"
	TypeServiceList        = "service.list"
	TypeServiceDelete      = "service.delete"
	TypeCertificateInstall = "certificate.install"
	TypeEdgeRegister       = "edge.register"
	TypeEdgeRegistered     = "edge.registered"
	TypeEdgeList           = "edge.list"
	TypeEdgeListed         = "edge.listed"

	TypeHostInfoResult       = "host.info.result"
	TypeServiceUpserted      = "service.upserted"
	TypeServiceListed        = "service.listed"
	TypeServiceDeleted       = "service.deleted"
	TypeCertificateInstalled = "certificate.installed"
	TypeOK                   = "ok"
	TypeError                = "error"
)

// Stable machine-readable control error codes. Error Message remains for
// humans and must never be used for trust or control flow.
const (
	ErrorCodeEdgeRouteCollision  = "edge.route_collision"
	ErrorCodeEdgeStaleSequence   = "edge.stale_sequence"
	ErrorCodeEdgeConflict        = "edge.sequence_conflict"
	ErrorCodeEdgeWakeUnavailable = "edge.wake_unavailable"
	ErrorCodeCredentialsFound    = "service.credentials_found"
)

// Log request limits keep one JSON control response below MaxPayload after
// base64 encoding.
const (
	DefaultLogTail = 64 << 10
	MaxLogTail     = 1 << 20
)

// Reasons a worker ends an attachment.
const (
	ReasonStolen   = "stolen"   // another client attached
	ReasonAttached = "attached" // detached-only attachment found an existing client
	ReasonClient   = "client"   // client asked to detach
	ReasonExited   = "exited"   // the session's process ended
	ReasonKilled   = "killed"   // the session was ended on request
)

// SessionInfo is the transport representation of durable session metadata.
type SessionInfo struct {
	Recovery           *recovery.Record `json:"recovery,omitempty"`
	RecoveryError      string           `json:"recoveryError,omitempty"`
	ReplacementID      string           `json:"replacementId,omitempty"`
	RecoveredFrom      string           `json:"recoveredFrom,omitempty"`
	ID                 string           `json:"id"`
	HostID             string           `json:"hostId"`
	Command            []string         `json:"command"`
	Cwd                string           `json:"cwd"`
	State              string           `json:"state"`
	CreatedAt          time.Time        `json:"createdAt"`
	LastAttachedAt     *time.Time       `json:"lastAttachedAt,omitempty"`
	ExitCode           *int             `json:"exitCode,omitempty"`
	LastOutputSequence uint64           `json:"lastOutputSequence"`
}

// HostInfo is the transport representation of one daemon's identity.
type HostInfo struct {
	RecoverySupported bool   `json:"recoverySupported,omitempty"`
	ID                string `json:"id"`
	MeshIdentity      string `json:"meshIdentity"`
	TailscaleName     string `json:"tailscaleName,omitempty"`
	PrivateName       string `json:"privateName,omitempty"`
}

// ServiceInfo is the transport representation of one origin service and its
// current health. The daemon ignores Healthy and Problem in client definitions.
type ServiceInfo struct {
	Name          string `json:"name"`
	Kind          string `json:"kind"`
	Target        string `json:"target"`
	PublicName    string `json:"publicName,omitempty"`
	WakeOnRequest bool   `json:"wakeOnRequest,omitempty"`
	Isolate       bool   `json:"isolate,omitempty"`
	Healthy       bool   `json:"healthy"`
	Problem       string `json:"problem,omitempty"`
}

// ServicePreview is the origin-authoritative interpretation of a requested
// service before mutation. FileCount is populated for public directories.
type ServicePreview struct {
	Service   ServiceInfo `json:"service"`
	FileCount uint64      `json:"fileCount"`
}

// CertificateInstall is a certificate bundle signed by the configured
// renewer for one exact origin identity.
type CertificateInstall struct {
	Profile        string `json:"profile"`
	Environment    string `json:"environment"`
	TargetID       string `json:"targetId"`
	SignerID       string `json:"signerId"`
	PrivateName    string `json:"privateName,omitempty"`
	CertificatePEM []byte `json:"certificatePem"`
	PrivateKeyPEM  []byte `json:"privateKeyPem"`
	Signature      []byte `json:"signature"`
}

// EdgeRoute is one signed public route. An origin endpoint is deliberately
// absent; the edge resolves it from its pinned allowlist.
type EdgeRoute struct {
	PublicName    string `json:"publicName"`
	ServiceName   string `json:"serviceName"`
	WakeOnRequest bool   `json:"wakeOnRequest,omitempty"`
}

// EdgeSnapshot is one origin's complete signed public desired state.
type EdgeSnapshot struct {
	TargetID  string      `json:"targetId"`
	OriginID  string      `json:"originId"`
	Sequence  uint64      `json:"sequence"`
	IssuedAt  time.Time   `json:"issuedAt"`
	ExpiresAt time.Time   `json:"expiresAt"`
	Routes    []EdgeRoute `json:"routes"`
	Signature []byte      `json:"signature"`
}

// EdgeRouteInfo is the safe status returned by edge.list.
type EdgeRouteInfo struct {
	PublicName    string    `json:"publicName"`
	ServiceName   string    `json:"serviceName"`
	WakeOnRequest bool      `json:"wakeOnRequest,omitempty"`
	DisplayAlias  string    `json:"displayAlias"`
	LastSeenAt    time.Time `json:"lastSeenAt"`
	Online        bool      `json:"online"`
}

// EdgeListProof authenticates one bounded edge.list page request. Its
// signature covers the request ID, cursor, limit, target, origin, and time.
type EdgeListProof struct {
	TargetID  string    `json:"targetId"`
	OriginID  string    `json:"originId"`
	IssuedAt  time.Time `json:"issuedAt"`
	Signature []byte    `json:"signature"`
}

// Control is the envelope for every JSON control message. Unused fields are
// omitted so messages stay readable on the wire during debugging.
type Control struct {
	RecoveryAction       string            `json:"recoveryAction,omitempty"`
	Recovery             *recovery.Record  `json:"recovery,omitempty"`
	RecoveryResult       *recovery.Result  `json:"recoveryResult,omitempty"`
	RecoverySupported    bool              `json:"recoverySupported,omitempty"`
	RecoveryCommand      *recovery.Command `json:"recoveryCommand,omitempty"`
	ClearRecoveryCommand bool              `json:"clearRecoveryCommand,omitempty"`
	ShellPID             int               `json:"shellPid,omitempty"`
	ShellDirectory       string            `json:"shellDirectory,omitempty"`
	ShellExecutable      string            `json:"shellExecutable,omitempty"`
	Type                 string            `json:"type"`
	RequestID            string            `json:"requestId,omitempty"`
	SessionID            string            `json:"sessionId,omitempty"`

	// Attach / create / inspect / containment
	LastSeq     *uint64  `json:"lastSeq,omitempty"` // exact resume point; nil requests a screen snapshot
	Tail        int      `json:"tail,omitempty"`    // trailing bytes wanted by non-attach consumers
	Cols        int      `json:"cols,omitempty"`
	Rows        int      `json:"rows,omitempty"`
	PreviewCols int      `json:"previewCols,omitempty"`
	PreviewRows int      `json:"previewRows,omitempty"`
	Command     []string `json:"command,omitempty"`
	Cwd         string   `json:"cwd,omitempty"`
	Term        string   `json:"term,omitempty"`  // client's TERM; a session without one is not a terminal
	Depth       int      `json:"depth,omitempty"` // nesting level of the session being created
	// ContainingSessions is ordered from the client process's immediate Mesh
	// session outward. Attach carries the upstream path; a containment response
	// prepends the worker that answered it.
	ContainingSessions []SessionIdentity `json:"containingSessions,omitempty"`
	NestedSession      *SessionIdentity  `json:"nestedSession,omitempty"`
	Nested             []SessionIdentity `json:"nested,omitempty"`
	// Attach requests promise dynamic detach keys across the full upstream path.
	// Responses distinguish supported nesting from a legacy worker or attacher.
	NestingSupported bool `json:"nestingSupported,omitempty"`

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

	// Inspect
	Inspection *SessionInspection `json:"inspection,omitempty"`

	// Logs
	Output []byte `json:"output,omitempty"`

	// Services
	ServiceName      string          `json:"serviceName,omitempty"`
	Service          *ServiceInfo    `json:"service,omitempty"`
	Services         []ServiceInfo   `json:"services,omitempty"`
	ServicePreview   *ServicePreview `json:"servicePreview,omitempty"`
	AllowCredentials bool            `json:"allowCredentials,omitempty"`

	// Certificate distribution
	Certificate            *CertificateInstall `json:"certificate,omitempty"`
	CertificateFingerprint string              `json:"certificateFingerprint,omitempty"`
	CertificateEnvironment string              `json:"certificateEnvironment,omitempty"`
	CertificateProfile     string              `json:"certificateProfile,omitempty"`
	CertificatePrivateName string              `json:"certificatePrivateName,omitempty"`

	// Public edge registration and safe status.
	EdgeSnapshot   *EdgeSnapshot   `json:"edgeSnapshot,omitempty"`
	EdgeSequence   uint64          `json:"edgeSequence,omitempty"`
	EdgeDigest     string          `json:"edgeDigest,omitempty"`
	EdgeRoutes     []EdgeRouteInfo `json:"edgeRoutes,omitempty"`
	EdgeCursor     string          `json:"edgeCursor,omitempty"`
	EdgeNextCursor string          `json:"edgeNextCursor,omitempty"`
	EdgeLimit      int             `json:"edgeLimit,omitempty"`
	EdgeListProof  *EdgeListProof  `json:"edgeListProof,omitempty"`

	// Error
	ErrorCode string `json:"errorCode,omitempty"`
	Message   string `json:"message,omitempty"`
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
