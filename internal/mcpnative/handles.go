// Package mcpnative implements Mesh's native stateless MCP tool registry,
// handle grammar, result rendering, and stdio request dispatcher.
package mcpnative

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session"
)

const (
	jobHandlePrefix        = "mj1"
	canonicalHostIDLen     = 43 // 32-byte Ed25519 public key in unpadded base64url
	maxJobHandleLength     = 96
	maxContextHandleLength = 80 // "mc1." (4) + hostID (43) + "." (1) + contextID (32)
)

// ErrInvalidHandle marks a syntactically malformed opaque Mesh handle.
// Handlers map this error to JSON-RPC -32602 (InvalidParams).
var ErrInvalidHandle = errors.New("malformed Mesh handle")

// ContextRef identifies one durable execution context on a host.
type ContextRef struct {
	HostID    string
	ContextID string
}

// String renders the canonical opaque context handle.
func (r ContextRef) String() string {
	return protocol.ContextHandle(r.HostID, r.ContextID)
}

// ParseContextHandle validates and decodes a mc1.<hostId>.<contextId> handle.
func ParseContextHandle(raw string) (ContextRef, error) {
	if len(raw) == 0 || len(raw) > maxContextHandleLength {
		return ContextRef{}, fmt.Errorf("%w: expected mc1.<hostId>.<contextId>", ErrInvalidHandle)
	}
	hostID, contextID, err := protocol.ParseContextHandle(raw)
	if err != nil {
		return ContextRef{}, fmt.Errorf("%w: expected mc1.<hostId>.<contextId>", ErrInvalidHandle)
	}
	if err := ValidateHostID(hostID); err != nil {
		return ContextRef{}, fmt.Errorf("%w: %w", ErrInvalidHandle, err)
	}
	if err := ValidateHexID(contextID); err != nil {
		return ContextRef{}, fmt.Errorf("%w: %w", ErrInvalidHandle, err)
	}
	return ContextRef{HostID: hostID, ContextID: contextID}, nil
}

// JobRef identifies one exact durable execution receipt on a host.
type JobRef struct {
	HostID    string
	SessionID string
	Instance  string
}

// String renders the canonical opaque job handle.
func (r JobRef) String() string {
	return jobHandlePrefix + "." + r.HostID + "." + r.SessionID + "." + r.Instance
}

// FormatJobHandle builds and validates a canonical mj1.<hostId>.<sessionId>.<instance> handle.
func FormatJobHandle(hostID, sessionID, instance string) (string, error) {
	if err := ValidateHostID(hostID); err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidHandle, err)
	}
	parsed, err := session.ParseID(sessionID)
	if err != nil || parsed != sessionID {
		return "", fmt.Errorf("%w: invalid session ID", ErrInvalidHandle)
	}
	if err := ValidateHexID(instance); err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidHandle, err)
	}
	return (JobRef{HostID: hostID, SessionID: sessionID, Instance: instance}).String(), nil
}

// ParseJobHandle validates and decodes a mj1.<hostId>.<sessionId>.<instance> handle.
func ParseJobHandle(raw string) (JobRef, error) {
	if len(raw) == 0 || len(raw) > maxJobHandleLength {
		return JobRef{}, fmt.Errorf("%w: expected mj1.<hostId>.<sessionId>.<instance>", ErrInvalidHandle)
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 4 || parts[0] != jobHandlePrefix {
		return JobRef{}, fmt.Errorf("%w: expected mj1.<hostId>.<sessionId>.<instance>", ErrInvalidHandle)
	}
	if err := ValidateHostID(parts[1]); err != nil {
		return JobRef{}, fmt.Errorf("%w: %w", ErrInvalidHandle, err)
	}
	parsed, err := session.ParseID(parts[2])
	if err != nil || parsed != parts[2] {
		return JobRef{}, fmt.Errorf("%w: invalid job session ID", ErrInvalidHandle)
	}
	if err := ValidateHexID(parts[3]); err != nil {
		return JobRef{}, fmt.Errorf("%w: %w", ErrInvalidHandle, err)
	}
	return JobRef{HostID: parts[1], SessionID: parts[2], Instance: parts[3]}, nil
}

// ValidateHostID checks that id is a canonical 32-byte Ed25519 Mesh host ID
// encoded in unpadded base64url (43 characters).
func ValidateHostID(id string) error {
	if len(id) != canonicalHostIDLen {
		return errors.New("expected a canonical 43-character Mesh host ID")
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(id)
	if err != nil || len(decoded) != 32 || base64.RawURLEncoding.EncodeToString(decoded) != id {
		return errors.New("expected a canonical base64url Mesh host ID")
	}
	return nil
}

// ValidateHexID checks that value is a canonical 32-character lowercase hex ID (16 bytes).
func ValidateHexID(value string) error {
	if len(value) != 32 {
		return errors.New("expected a 32-character lowercase hex ID")
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != 16 || hex.EncodeToString(decoded) != value {
		return errors.New("expected a canonical 32-character lowercase hex ID")
	}
	return nil
}
