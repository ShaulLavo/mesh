package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"syscall"

	"github.com/shaul/mesh/internal/transport"
)

// hostConnectError is a TCP connection that failed before any Mesh
// authentication began. Reporting it as an authentication failure sent people
// to check keys and grants when a sandbox, firewall or stopped daemon had
// refused the socket itself.
type hostConnectError struct {
	host    string
	problem string
	cause   error
}

func (e *hostConnectError) Error() string {
	return "Cannot connect to " + e.host + ": " + e.problem
}

func (e *hostConnectError) Unwrap() error { return e.cause }

// classifyConnectError returns the plain connection failure behind err when the
// TCP dial itself failed, and nil when the failure came later (HTTP upgrade,
// TLS, Mesh identity) or the caller canceled. fallbackAddress names the target
// when the dial error carries no resolved address, as with a DNS failure.
func classifyConnectError(host HostRecord, fallbackAddress string, err error) error {
	var classified *hostConnectError
	if errors.As(err, &classified) {
		return classified
	}
	var dial *net.OpError
	if !errors.As(err, &dial) || dial.Op != "dial" || dial.Err == nil || errors.Is(err, context.Canceled) {
		return nil
	}
	address := fallbackAddress
	if dial.Addr != nil {
		address = dial.Addr.String()
	}
	label := HostLabel(host)
	return &hostConnectError{host: label, problem: connectProblem(label, address, dial), cause: err}
}

func connectProblem(label, address string, dial *net.OpError) string {
	reason := dial.Err.Error()
	var errno syscall.Errno
	if errors.As(dial.Err, &errno) {
		reason = errno.Error()
	}
	switch {
	case errors.Is(dial.Err, syscall.EPERM), errors.Is(dial.Err, syscall.EACCES):
		return fmt.Sprintf("TCP connection to %s was blocked by local permissions (%s). If this shell runs in a sandbox, such as Codex workspace-write, turn on its network access.", address, reason)
	case errors.Is(dial.Err, syscall.ECONNREFUSED):
		return fmt.Sprintf("nothing accepted the TCP connection to %s (%s). Check that Mesh is running on %s.", address, reason, label)
	case dial.Timeout():
		return fmt.Sprintf("TCP connection to %s got no answer (%s). Check that %s is online in Tailscale.", address, reason, label)
	case errors.Is(dial.Err, syscall.ENETUNREACH), errors.Is(dial.Err, syscall.EHOSTUNREACH):
		return fmt.Sprintf("no network route to %s (%s). Check that %s is online in Tailscale.", address, reason, label)
	default:
		return fmt.Sprintf("TCP connection to %s failed (%s).", address, reason)
	}
}

// endpointAddress names the host:port a control endpoint dials.
func endpointAddress(host HostRecord) string {
	parsed, err := url.Parse(host.Endpoint)
	if err != nil || parsed.Host == "" {
		return host.Endpoint
	}
	return parsed.Host
}

// controlAuthenticationFailure reports whether a control dial got as far as
// Mesh authentication: the peer refused, required an upgrade, or presented
// another identity.
func controlAuthenticationFailure(err error) bool {
	return errors.Is(err, transport.ErrAuthentication) ||
		errors.Is(err, transport.ErrAuthenticationRequired) ||
		errors.Is(err, transport.ErrControlAuthenticationRequired) ||
		errors.Is(err, transport.ErrDestinationIdentityMismatch)
}
