// Package mcpnative implements Mesh's native stateless MCP tool registry,
// handle grammar, result rendering, and stdio request dispatcher.
package mcpnative

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/shaul/mesh/internal/session"
)

const (
	jobHandlePrefix      = "mj1"
	maxHandleFieldLength = 128
)

// ErrInvalidHandle marks a syntactically malformed opaque Mesh handle.
// Handlers map this error to JSON-RPC -32602 (InvalidParams).
var ErrInvalidHandle = errors.New("malformed Mesh handle")

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

// ValidateHostID checks that id uses only base64url identifier bytes without dots.
func ValidateHostID(id string) error {
	return validateHandleToken(id, "host ID")
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

func validateHandleToken(value, label string) error {
	if len(value) == 0 || len(value) > maxHandleFieldLength {
		return fmt.Errorf("invalid %s length", label)
	}
	for i := 0; i < len(value); i++ {
		if !isBase64URLChar(value[i]) {
			return fmt.Errorf("invalid character in %s", label)
		}
	}
	return nil
}

func isBase64URLChar(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '-' || b == '_'
}
