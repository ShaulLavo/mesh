package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Execution context controls. Each request carries the application caller's
// credential in Control.Caller and its parameters in Control.ExecutionContext.
// Each reply has type TypeContextResult with Control.ContextInfo, or
// Control.ContextInfos for list.
const (
	TypeContextOpen    = "context.open"
	TypeContextGet     = "context.get"
	TypeContextUpdate  = "context.update"
	TypeContextList    = "context.list"
	TypeContextClose   = "context.close"
	TypeContextReceipt = "context.receipt"
	TypeContextResult  = "context.result"
)

// Context shells. ContextShellLogin runs a command string as
// [loginShell, "-l", "-c", command] with the login shell resolved when the
// context opens; ContextShellSh runs ["/bin/sh", "-c", command];
// ContextShellNone accepts argv only.
const (
	ContextShellLogin = "login"
	ContextShellSh    = "sh"
	ContextShellNone  = "none"
)

// Context states.
const (
	ContextStateOpen   = "open"
	ContextStateClosed = "closed"
)

// Context limits from the plan: overlay size, open contexts per caller and
// per host, and expiry after 30 days without use while no job runs.
const (
	MaxContextEnvVars       = 128
	MaxContextEnvBytes      = 32 << 10
	MaxContextLabelBytes    = 256
	MaxOpenContextsPerOwner = 64
	MaxOpenContextsPerHost  = 1024
	ContextIdleExpiry       = 30 * 24 * time.Hour
)

// IsContextControl identifies the host context requests.
func IsContextControl(kind string) bool {
	switch kind {
	case TypeContextOpen, TypeContextGet, TypeContextUpdate, TypeContextList, TypeContextClose, TypeContextReceipt:
		return true
	default:
		return false
	}
}

// ExecutionContextRequest carries context parameters. Which fields apply
// depends on Control.Type:
//
//   - context.open: Open, plus Control.RequestKey.
//   - context.get, context.close: ContextID; close also takes an optional
//     ExpectedRevision.
//   - context.update: ContextID, ExpectedRevision and Change.
//   - context.list: no fields.
//   - context.receipt: Control.RequestKey only.
//   - execution.create in a context: ContextID, then exactly one of
//     Control.Command (argv) or ShellCommand, an optional relative or absolute
//     Control.Cwd resolved against the context directory, and an optional
//     per-job Env overlay.
//   - execution.request in a context: ContextID, plus Control.RequestKey.
//   - raw execution controls (logs, wait, input, signal, kill, remove,
//     inspect): an optional ContextID that must match the job's context.
type ExecutionContextRequest struct {
	ContextID        string            `json:"contextId,omitempty"`
	ExpectedRevision *uint64           `json:"expectedRevision,string,omitempty"`
	Open             *ContextSpec      `json:"open,omitempty"`
	Change           *ContextChange    `json:"change,omitempty"`
	ShellCommand     string            `json:"shellCommand,omitempty"`
	Env              map[string]string `json:"env,omitempty"`
	// Cursor continues context.list from Control.ContextNextCursor.
	Cursor string `json:"cursor,omitempty"`
}

// ContextSpec is the canonical creation intent of a context.
type ContextSpec struct {
	Cwd   string            `json:"cwd"`
	Shell string            `json:"shell,omitempty"`
	Env   map[string]string `json:"env,omitempty"`
	Label string            `json:"label,omitempty"`
}

// ContextChange edits a context at an expected revision. Empty Cwd keeps the
// directory; SetEnv and UnsetEnv edit the overlay; a nil Label keeps it.
type ContextChange struct {
	Cwd      string            `json:"cwd,omitempty"`
	SetEnv   map[string]string `json:"setEnv,omitempty"`
	UnsetEnv []string          `json:"unsetEnv,omitempty"`
	Label    *string           `json:"label,omitempty"`
}

// ExecutionContextInfo is what a host returns about one context. It names
// environment variables and a digest of their values, never the values.
type ExecutionContextInfo struct {
	HostID      string   `json:"hostId"`
	ContextID   string   `json:"contextId"`
	Handle      string   `json:"handle"`
	RequestKey  string   `json:"requestKey"`
	State       string   `json:"state"`
	Revision    uint64   `json:"revision,string"`
	Cwd         string   `json:"cwd"`
	Shell       string   `json:"shell"`
	ShellPath   string   `json:"shellPath,omitempty"`
	Label       string   `json:"label,omitempty"`
	EnvNames    []string `json:"envNames"`
	EnvDigest   string   `json:"envDigest"`
	RunningJobs int      `json:"runningJobs"`
	// UncertainJobs were accepted but cannot be confirmed running or ended,
	// such as a job whose daemon stopped before launch. They keep the
	// context from expiring like running jobs do.
	UncertainJobs int        `json:"uncertainJobs"`
	CreatedAt     time.Time  `json:"createdAt"`
	LastUsedAt    time.Time  `json:"lastUsedAt"`
	ClosedAt      *time.Time `json:"closedAt,omitempty"`
	// ExpiresAt is the wall-clock date from which the record may expire if it
	// stays unused and no job runs. A running job postpones expiry, and the
	// host's uptime clock must also show the idle period, so the record can
	// go away later than this date, never earlier.
	ExpiresAt time.Time `json:"expiresAt"`
}

const (
	callerField           = "caller"
	administrationField   = "administration"
	executionContextField = "executionContext"
)

func executionBool(raw []byte) error {
	if bytes.Equal(raw, []byte("true")) || bytes.Equal(raw, []byte("false")) {
		return nil
	}
	return fmt.Errorf("execution: invalid boolean")
}

// executionObject bounds the nested context request; its own fields are
// checked by the daemon, which knows the context limits.
func executionObject(raw []byte) error {
	var probe map[string]json.RawMessage
	if json.Unmarshal(raw, &probe) != nil || probe == nil {
		return fmt.Errorf("execution: invalid context object")
	}
	return executionEscapes(raw)
}

// CallerCredential is an encoded application credential on the wire. Its
// String and GoString hide the value so formatted controls never leak it.
type CallerCredential string

func (c CallerCredential) String() string {
	if c == "" {
		return ""
	}
	return "[application credential]"
}
func (c CallerCredential) GoString() string { return c.String() }

const contextHandlePrefix = "mc1."

// ContextHandle is the opaque handle tools show for a context. The host ID
// inside lets a fresh frontend route the call without local state.
func ContextHandle(hostID, contextID string) string {
	return contextHandlePrefix + hostID + "." + contextID
}

// ParseContextHandle splits a handle made by ContextHandle.
func ParseContextHandle(handle string) (hostID, contextID string, err error) {
	body, ok := strings.CutPrefix(handle, contextHandlePrefix)
	host, id, found := strings.Cut(body, ".")
	if !ok || !found || host == "" || !ValidContextID(id) {
		return "", "", fmt.Errorf("context handle %q is malformed", handle)
	}
	return host, id, nil
}

// ValidContextID accepts the 32 lowercase hex characters of a 16-byte ID.
func ValidContextID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, ch := range []byte(id) {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}
