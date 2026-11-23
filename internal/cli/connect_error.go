package cli

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode"

	"github.com/shaul/mesh/internal/transport"
)

// hostConnectError is a connection that failed before any Mesh authentication
// began. Reporting it as an authentication failure sent people to check keys
// and grants when a sandbox, firewall or stopped daemon had refused the socket.
type hostConnectError struct {
	host    string
	connect *transport.ConnectError
	cause   error
}

func (e *hostConnectError) Error() string {
	return "Cannot connect to " + e.host + ": " + e.connect.Problem() + "." + connectAction(e.host, e.connect.Failure)
}

func (e *hostConnectError) Unwrap() error { return e.cause }

func connectAction(host string, failure transport.ConnectFailure) string {
	switch failure {
	case transport.ConnectBlocked:
		return " If this shell runs in a sandbox, such as Codex workspace-write, turn on its network access."
	case transport.ConnectRefused:
		return fmt.Sprintf(" Check that Mesh is running on %s.", host)
	case transport.ConnectTimedOut, transport.ConnectNoRoute:
		return fmt.Sprintf(" Check that %s is online in Tailscale.", host)
	case transport.ConnectFailed:
		return ""
	}
	return ""
}

// classifyConnectError presents err as a connection failure to host when a
// dial failed before authentication, and returns nil when the failure came
// later (HTTP upgrade, TLS, Mesh identity) or the caller canceled. An error
// that already carries a presented failure comes back whole, so every part of
// an errors.Join keeps its own cause. fallbackAddress names the target when
// the dial error carries no resolved address.
func classifyConnectError(host HostRecord, fallbackAddress string, err error) error {
	var presented *hostConnectError
	if errors.As(err, &presented) {
		return err
	}
	var connect *transport.ConnectError
	if !errors.As(err, &connect) {
		connect = transport.ClassifyDial(fallbackAddress, err)
	}
	if connect == nil {
		return nil
	}
	return &hostConnectError{host: HostLabel(host), connect: connect, cause: err}
}

// presentJoinPart prepares one failure for errors.Join. A joined error skips
// openVerifiedHostInfo's sanitizing prefix, and a wake failure can carry a
// remote sender's message, so every part gets terminal-safe text.
func presentJoinPart(host HostRecord, err error) error {
	if connectErr := classifyConnectError(host, endpointAddress(host), err); connectErr != nil {
		err = connectErr
	}
	return sanitizedError{cause: err}
}

// sanitizedError shows its cause as terminal-safe text and keeps the cause for
// errors.Is and errors.As. It only replaces characters a terminal would act
// on, so a local path in the same message keeps its exact spelling.
type sanitizedError struct{ cause error }

func (e sanitizedError) Error() string { return terminalSafeText(e.cause.Error()) }

// terminalSafeText replaces invalid UTF-8 and nonprintable characters, such
// as carriage returns and escape sequences, and leaves printable text as it
// is. Unlike safeRemoteText it neither collapses spaces nor truncates.
func terminalSafeText(value string) string {
	value = strings.ToValidUTF8(value, "?")
	return strings.Map(func(character rune) rune {
		if !unicode.IsPrint(character) {
			return ' '
		}
		return character
	}, value)
}

func (e sanitizedError) Unwrap() error { return e.cause }

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
