package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"syscall"
)

// ConnectFailure says why a connection failed before Mesh authentication.
type ConnectFailure uint8

const (
	ConnectFailed ConnectFailure = iota
	ConnectBlocked
	ConnectRefused
	ConnectTimedOut
	ConnectNoRoute
)

// ConnectError is a dial that failed before Mesh authentication began, so
// callers can report a blocked or unreachable socket as a connection problem
// and keep authentication wording for real authentication failures.
type ConnectError struct {
	Address string
	Failure ConnectFailure
	// TCP is set when the error came from the TCP dial itself. A request
	// deadline can also expire while net/http is still dialing or waiting for
	// the upgrade, and then the phase is unknown.
	TCP    bool
	Reason string
	Err    error
}

func (e *ConnectError) Error() string { return "transport: " + e.Problem() }

func (e *ConnectError) Unwrap() error { return e.Err }

// Problem describes the failure without a package prefix, for messages that
// name the host themselves.
func (e *ConnectError) Problem() string {
	switch {
	case e.Failure == ConnectBlocked:
		return fmt.Sprintf("TCP connection to %s was blocked by local permissions (%s)", e.Address, e.Reason)
	case e.Failure == ConnectRefused:
		return fmt.Sprintf("nothing accepted the TCP connection to %s (%s)", e.Address, e.Reason)
	case e.Failure == ConnectTimedOut && e.TCP:
		return fmt.Sprintf("TCP connection to %s got no answer (%s)", e.Address, e.Reason)
	case e.Failure == ConnectTimedOut:
		return fmt.Sprintf("control connection to %s timed out before Mesh authentication (%s)", e.Address, e.Reason)
	case e.Failure == ConnectNoRoute:
		return fmt.Sprintf("no network route to %s (%s)", e.Address, e.Reason)
	default:
		return fmt.Sprintf("TCP connection to %s failed (%s)", e.Address, e.Reason)
	}
}

// ClassifyDial returns the connection failure behind err when a TCP dial
// (a *net.OpError with Op "dial") failed, and nil otherwise or when the caller
// canceled. address names the target when the dial error carries no resolved
// address, as with a DNS failure.
func ClassifyDial(address string, err error) *ConnectError {
	var dial *net.OpError
	if !errors.As(err, &dial) || dial.Op != "dial" || dial.Err == nil || errors.Is(err, context.Canceled) {
		return nil
	}
	if dial.Addr != nil {
		address = dial.Addr.String()
	}
	reason := dial.Err.Error()
	var errno syscall.Errno
	if errors.As(dial.Err, &errno) {
		reason = errno.Error()
	}
	return &ConnectError{Address: address, Failure: dialFailureKind(dial), TCP: true, Reason: reason, Err: err}
}

func dialFailureKind(dial *net.OpError) ConnectFailure {
	switch {
	case errors.Is(dial.Err, syscall.EPERM), errors.Is(dial.Err, syscall.EACCES):
		return ConnectBlocked
	case errors.Is(dial.Err, syscall.ECONNREFUSED):
		return ConnectRefused
	case dial.Timeout():
		return ConnectTimedOut
	case errors.Is(dial.Err, syscall.ENETUNREACH), errors.Is(dial.Err, syscall.EHOSTUNREACH):
		return ConnectNoRoute
	default:
		return ConnectFailed
	}
}

// classifyWebSocketDial covers a WebSocket dial that got no HTTP response:
// either the TCP dial failed or the deadline expired before the upgrade.
func classifyWebSocketDial(endpoint string, err error) *ConnectError {
	address := endpointHost(endpoint)
	if connect := ClassifyDial(address, err); connect != nil {
		return connect
	}
	if errors.Is(err, context.Canceled) || !errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return &ConnectError{Address: address, Failure: ConnectTimedOut, Reason: context.DeadlineExceeded.Error(), Err: err}
}

func endpointHost(endpoint string) string {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" {
		return endpoint
	}
	return parsed.Host
}
