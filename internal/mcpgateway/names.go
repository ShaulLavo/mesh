// Package mcpgateway merges native Mesh tools and selected hosted MCP servers
// into one stateless MCP 2026-07-28 entry (PLAN section 7). It owns the
// profiles file, the exposed names and URIs with their exact reverse mapping,
// merged listings and their private cache, subscription fan-in, and routing of
// every request to its owning host.
package mcpgateway

import (
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Kinds of exposed names. Resources use URIs, not names.
const (
	KindTool   = "t"
	KindPrompt = "p"
)

const (
	hostedPrefix   = "h_"
	overflowPrefix = "hx_"
	// MaxNameLength is the MCP limit on tool and prompt names.
	MaxNameLength = 128
	// URIScheme prefixes every hosted resource URI and template.
	URIScheme = "mesh-mcp://"

	hostIDBytes    = 32
	serviceIDBytes = 16
)

var (
	// ErrUnknownName marks a name or URI that maps to no hosted identity.
	ErrUnknownName = errors.New("unknown gateway name")

	lowerBase32 = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)
)

// Identity is the exact upstream object behind an exposed name.
type Identity struct {
	HostID    string `json:"hostId"`
	ServiceID string `json:"serviceId"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
}

// EncodedName returns the deterministic h_<host>_<service>_<kind>_<name>
// form, or false when it exceeds the MCP name limit and needs an hx_
// allocation. The IDs are encoded from their canonical bytes (a host ID is 32
// base64url bytes, a service ID 16 hex bytes) so the prefix stays short
// enough to leave room for ordinary names.
func EncodedName(id Identity) (string, bool, error) {
	host, err := hostBytes(id.HostID)
	if err != nil {
		return "", false, err
	}
	service, err := serviceBytes(id.ServiceID)
	if err != nil {
		return "", false, err
	}
	if id.Kind != KindTool && id.Kind != KindPrompt {
		return "", false, fmt.Errorf("gateway name kind %q is not t or p", id.Kind)
	}
	if id.Name == "" {
		return "", false, errors.New("gateway name: upstream name is empty")
	}
	name := hostedPrefix + lowerBase32.EncodeToString(host) + "_" + lowerBase32.EncodeToString(service) + "_" + id.Kind + "_" +
		lowerBase32.EncodeToString([]byte(id.Name))
	return name, len(name) <= MaxNameLength, nil
}

// DecodeName reverses an h_ name exactly. An hx_ name needs the registry.
func DecodeName(name string) (Identity, error) {
	rest, ok := strings.CutPrefix(name, hostedPrefix)
	if !ok || len(name) > MaxNameLength {
		return Identity{}, fmt.Errorf("%w: %q", ErrUnknownName, name)
	}
	parts := strings.Split(rest, "_")
	if len(parts) != 4 || (parts[2] != KindTool && parts[2] != KindPrompt) {
		return Identity{}, fmt.Errorf("%w: %q", ErrUnknownName, name)
	}
	host, hostOK := decodeField(parts[0], hostIDBytes)
	service, serviceOK := decodeField(parts[1], serviceIDBytes)
	original, nameOK := decodeField(parts[3], -1)
	if !hostOK || !serviceOK || !nameOK || len(original) == 0 {
		return Identity{}, fmt.Errorf("%w: %q", ErrUnknownName, name)
	}
	return Identity{
		HostID:    base64.RawURLEncoding.EncodeToString(host),
		ServiceID: hex.EncodeToString(service),
		Kind:      parts[2],
		Name:      string(original),
	}, nil
}

// IsHostedName reports whether name belongs to the hosted namespaces.
func IsHostedName(name string) bool {
	return strings.HasPrefix(name, hostedPrefix) || strings.HasPrefix(name, overflowPrefix)
}

// IsOverflowName reports whether name is an hx_ registry allocation.
func IsOverflowName(name string) bool { return strings.HasPrefix(name, overflowPrefix) }

// EncodeURI prefixes an original resource URI or URI template with its route.
// The original is kept byte for byte, including its percent-encoding and any
// template variables.
func EncodeURI(hostID, serviceID, original string) (string, error) {
	host, err := hostBytes(hostID)
	if err != nil {
		return "", err
	}
	service, err := serviceBytes(serviceID)
	if err != nil {
		return "", err
	}
	if original == "" {
		return "", errors.New("gateway URI: upstream URI is empty")
	}
	return URIScheme + lowerBase32.EncodeToString(host) + "/" + lowerBase32.EncodeToString(service) + "/" + original, nil
}

// DecodeURI parses only the fixed prefix and returns the original URI
// untouched. template allows URI-template syntax in the original.
func DecodeURI(uri string, template bool) (hostID, serviceID, original string, err error) {
	rest, ok := strings.CutPrefix(uri, URIScheme)
	if !ok {
		return "", "", "", fmt.Errorf("%w: %q is not a mesh-mcp URI", ErrUnknownName, uri)
	}
	hostPart, rest, ok1 := strings.Cut(rest, "/")
	servicePart, original, ok2 := strings.Cut(rest, "/")
	host, hostOK := decodeField(hostPart, hostIDBytes)
	service, serviceOK := decodeField(servicePart, serviceIDBytes)
	if !ok1 || !ok2 || !hostOK || !serviceOK || original == "" {
		return "", "", "", fmt.Errorf("%w: %q", ErrUnknownName, uri)
	}
	if !template && !absoluteURI(original) {
		return "", "", "", fmt.Errorf("%w: %q does not wrap an absolute URI", ErrUnknownName, uri)
	}
	return base64.RawURLEncoding.EncodeToString(host), hex.EncodeToString(service), original, nil
}

// absoluteURI checks the RFC 3986 scheme and rejects whitespace and control
// characters. It deliberately does not reparse the rest, so an upstream's
// own encoding (such as %2F in an authority) round-trips untouched.
func absoluteURI(uri string) bool {
	scheme, _, ok := strings.Cut(uri, ":")
	if !ok || scheme == "" || !isAlpha(scheme[0]) {
		return false
	}
	for i := range len(scheme) {
		c := scheme[i]
		if !isAlpha(c) && !(c >= '0' && c <= '9') && c != '+' && c != '-' && c != '.' {
			return false
		}
	}
	return !strings.ContainsFunc(uri, func(r rune) bool { return r <= ' ' || r == 0x7f })
}

func isAlpha(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

// decodeField accepts only the canonical lowercase unpadded encoding, so
// every exposed name has exactly one decoding and one encoding.
func decodeField(field string, size int) ([]byte, bool) {
	if field == "" {
		return nil, false
	}
	raw, err := lowerBase32.DecodeString(field)
	if err != nil || lowerBase32.EncodeToString(raw) != field {
		return nil, false
	}
	if size >= 0 && len(raw) != size {
		return nil, false
	}
	return raw, true
}

func hostBytes(hostID string) ([]byte, error) {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(hostID)
	if err != nil || len(raw) != hostIDBytes || base64.RawURLEncoding.EncodeToString(raw) != hostID {
		return nil, fmt.Errorf("gateway: host ID %q is not a canonical Mesh host ID", hostID)
	}
	return raw, nil
}

func serviceBytes(serviceID string) ([]byte, error) {
	raw, err := hex.DecodeString(serviceID)
	if err != nil || len(raw) != serviceIDBytes || hex.EncodeToString(raw) != serviceID {
		return nil, fmt.Errorf("gateway: service ID %q is not a canonical hosted service ID", serviceID)
	}
	return raw, nil
}
