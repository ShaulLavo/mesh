package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/shaul/mesh/internal/dnsname"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

// HostDialer opens one protocol connection to an adopted host.
type HostDialer func(context.Context, HostRecord) (transport.Conn, error)

func dialHost(ctx context.Context, host HostRecord) (transport.Conn, error) {
	return transport.Dial(ctx, host.Endpoint, transport.DialOptions{})
}

func openVerifiedHost(ctx context.Context, host HostRecord, dial HostDialer) (transport.Conn, error) {
	conn, _, err := openVerifiedHostInfo(ctx, host, dial)
	return conn, err
}

func openVerifiedHostInfo(ctx context.Context, host HostRecord, dial HostDialer) (transport.Conn, protocol.HostInfo, error) {
	conn, err := dial(ctx, host)
	if err != nil {
		return nil, protocol.HostInfo{}, presentError("connect to host "+host.Alias, err)
	}
	requestID, err := newDaemonRequestID()
	if err != nil {
		_ = conn.Close()
		return nil, protocol.HostInfo{}, err
	}
	response, err := controlRequest(ctx, conn, protocol.Control{Type: protocol.TypeHostInfo, RequestID: requestID})
	if err != nil {
		_ = conn.Close()
		return nil, protocol.HostInfo{}, fmt.Errorf("verify host %s: %w", host.Alias, err)
	}
	if response.Type == protocol.TypeError {
		_ = conn.Close()
		return nil, protocol.HostInfo{}, daemonResponseError("host identity", response.Message)
	}
	if response.Type != protocol.TypeHostInfoResult || response.Host == nil {
		_ = conn.Close()
		return nil, protocol.HostInfo{}, fmt.Errorf("verify host %s: response is not host info", host.Alias)
	}
	if err := validateHostInfo(host, *response.Host); err != nil {
		_ = conn.Close()
		return nil, protocol.HostInfo{}, err
	}
	return conn, *response.Host, nil
}

func validateHostInfo(expected HostRecord, actual protocol.HostInfo) error {
	if actual.ID != expected.ID || actual.MeshIdentity != expected.MeshIdentity {
		return fmt.Errorf("host %s identity changed", expected.Alias)
	}
	if actual.PrivateName != "" {
		if err := dnsname.ValidatePrivateName(actual.PrivateName); err != nil {
			return fmt.Errorf("host %s returned an invalid private name", expected.Alias)
		}
	}
	return nil
}

func listRemoteHost(ctx context.Context, host HostRecord, dial HostDialer) ([]protocol.SessionInfo, error) {
	conn, err := openVerifiedHost(ctx, host, dial)
	if err != nil {
		return nil, err
	}
	defer conn.Close() //nolint:errcheck // the request result is authoritative
	requestID, err := newDaemonRequestID()
	if err != nil {
		return nil, err
	}
	response, err := controlRequest(ctx, conn, protocol.Control{Type: protocol.TypeList, RequestID: requestID})
	if err != nil {
		return nil, err
	}
	switch response.Type {
	case protocol.TypeListed:
		for _, listed := range response.Sessions {
			if err := validateDaemonSession(listed); err != nil {
				return nil, err
			}
			if listed.HostID != host.ID {
				return nil, fmt.Errorf("host %s listed a session for a different host", host.Alias)
			}
		}
		return cloneSessionInfo(response.Sessions), nil
	case protocol.TypeError:
		return nil, daemonResponseError("session list", response.Message)
	default:
		return nil, fmt.Errorf("host %s returned an unexpected session-list response", host.Alias)
	}
}

func createRemoteSession(ctx context.Context, host HostRecord, dial HostDialer, command []string, cols, rows int) (string, error) {
	conn, err := openVerifiedHost(ctx, host, dial)
	if err != nil {
		return "", err
	}
	defer conn.Close() //nolint:errcheck // one create request
	requestID, err := newDaemonRequestID()
	if err != nil {
		return "", err
	}
	response, err := controlRequest(ctx, conn, protocol.Control{
		Type:      protocol.TypeCreate,
		RequestID: requestID,
		Term:      clientTerm(),
		Command:   append([]string(nil), command...),
		Cols:      cols,
		Rows:      rows,
	})
	if err != nil {
		return "", err
	}
	return validateDaemonCreateResponse(response)
}

func controlRemoteSession(ctx context.Context, host HostRecord, dial HostDialer, sessionID, controlType, signal string) error {
	conn, err := openVerifiedHost(ctx, host, dial)
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck // one control request
	requestID, err := newDaemonRequestID()
	if err != nil {
		return err
	}
	response, err := controlRequest(ctx, conn, protocol.Control{
		Type:      controlType,
		RequestID: requestID,
		SessionID: sessionID,
		Signal:    signal,
	})
	if err != nil {
		return err
	}
	switch response.Type {
	case protocol.TypeOK:
		if response.SessionID != sessionID {
			return fmt.Errorf("host %s acknowledged a different session", host.Alias)
		}
		return nil
	case protocol.TypeError:
		return daemonResponseError(controlType+" "+sessionID, response.Message)
	default:
		return fmt.Errorf("host %s returned an unexpected %s response", host.Alias, controlType)
	}
}

func logsRemoteSession(ctx context.Context, host HostRecord, dial HostDialer, sessionID string, tail int) ([]byte, error) {
	conn, err := openVerifiedHost(ctx, host, dial)
	if err != nil {
		return nil, err
	}
	defer conn.Close() //nolint:errcheck // one logs request
	requestID, err := newDaemonRequestID()
	if err != nil {
		return nil, err
	}
	response, err := controlRequest(ctx, conn, protocol.Control{
		Type:      protocol.TypeLogs,
		RequestID: requestID,
		SessionID: sessionID,
		Tail:      tail,
	})
	if err != nil {
		return nil, err
	}
	switch response.Type {
	case protocol.TypeLogged:
		if response.SessionID != sessionID {
			return nil, fmt.Errorf("host %s returned logs for a different session", host.Alias)
		}
		if len(response.Output) > tail {
			return nil, fmt.Errorf("host %s returned %d log bytes, want at most %d", host.Alias, len(response.Output), tail)
		}
		return append([]byte(nil), response.Output...), nil
	case protocol.TypeError:
		return nil, daemonResponseError("logs "+sessionID, response.Message)
	default:
		return nil, fmt.Errorf("host %s returned an unexpected logs response", host.Alias)
	}
}

// clientTerm reports the terminal type to run the remote session under. The
// daemon is a service and has no TERM of its own, so a session inherits none
// and shell startup that branches on it silently takes the wrong path.
func clientTerm() string {
	if term := strings.TrimSpace(os.Getenv("TERM")); term != "" && term != "dumb" {
		return term
	}
	return "xterm-256color"
}
