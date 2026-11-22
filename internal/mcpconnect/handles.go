// Package mcpconnect implements the direct stdio-to-H3 MCP frontend for
// `mesh mcp connect HOST/NAME`.
package mcpconnect

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const (
	instanceHandlePrefix = "mi1"
	canonicalHostIDLen   = 43
	hexIDLen             = 32
	maxInstanceHandleLen = 120
)

// ErrInvalidHandle marks a malformed opaque hosted-instance handle.
var ErrInvalidHandle = errors.New("malformed hosted instance handle")

// InstanceRef identifies one hosted MCP server instance on its owning host.
type InstanceRef struct {
	HostID     string
	ServiceID  string
	InstanceID string
}

// String renders the canonical opaque mi1.<hostId>.<serviceId>.<instanceId> handle.
func (r InstanceRef) String() string {
	return instanceHandlePrefix + "." + r.HostID + "." + r.ServiceID + "." + r.InstanceID
}

// FormatInstanceHandle validates its components and formats a canonical mi1 handle.
func FormatInstanceHandle(hostID, serviceID, instanceID string) (string, error) {
	if err := validateHostID(hostID); err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidHandle, err)
	}
	if err := validateHexID(serviceID); err != nil {
		return "", fmt.Errorf("%w: service ID: %w", ErrInvalidHandle, err)
	}
	if err := validateHexID(instanceID); err != nil {
		return "", fmt.Errorf("%w: instance ID: %w", ErrInvalidHandle, err)
	}
	return (InstanceRef{HostID: hostID, ServiceID: serviceID, InstanceID: instanceID}).String(), nil
}

// ParseInstanceHandle validates and decodes mi1.<hostId>.<serviceId>.<instanceId>.
func ParseInstanceHandle(raw string) (InstanceRef, error) {
	if len(raw) == 0 || len(raw) > maxInstanceHandleLen {
		return InstanceRef{}, fmt.Errorf("%w: expected mi1.<hostId>.<serviceId>.<instanceId>", ErrInvalidHandle)
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 4 || parts[0] != instanceHandlePrefix {
		return InstanceRef{}, fmt.Errorf("%w: expected mi1.<hostId>.<serviceId>.<instanceId>", ErrInvalidHandle)
	}
	if err := validateHostID(parts[1]); err != nil {
		return InstanceRef{}, fmt.Errorf("%w: %w", ErrInvalidHandle, err)
	}
	if err := validateHexID(parts[2]); err != nil {
		return InstanceRef{}, fmt.Errorf("%w: service ID: %w", ErrInvalidHandle, err)
	}
	if err := validateHexID(parts[3]); err != nil {
		return InstanceRef{}, fmt.Errorf("%w: instance ID: %w", ErrInvalidHandle, err)
	}
	return InstanceRef{HostID: parts[1], ServiceID: parts[2], InstanceID: parts[3]}, nil
}

func validateHostID(id string) error {
	if len(id) != canonicalHostIDLen {
		return errors.New("expected a canonical 43-character Mesh host ID")
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(id)
	if err != nil || len(decoded) != 32 || base64.RawURLEncoding.EncodeToString(decoded) != id {
		return errors.New("expected a canonical base64url Mesh host ID")
	}
	return nil
}

func validateHexID(value string) error {
	if len(value) != hexIDLen {
		return errors.New("expected a 32-character lowercase hex ID")
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != 16 || hex.EncodeToString(decoded) != value {
		return errors.New("expected a canonical 32-character lowercase hex ID")
	}
	return nil
}
