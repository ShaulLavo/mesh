package cli

import (
	"context"
	"fmt"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

// HostDialer opens one protocol connection to an adopted host.
type HostDialer func(context.Context, HostRecord) (transport.Conn, error)

func dialHost(ctx context.Context, host HostRecord) (transport.Conn, error) {
	conn, err := transport.Dial(ctx, host.Endpoint, transport.DialOptions{})
	if err != nil {
		return nil, fmt.Errorf("connect to host %s: %w", host.Alias, err)
	}
	return conn, nil
}

func openVerifiedHost(ctx context.Context, host HostRecord, dial HostDialer) (transport.Conn, error) {
	conn, err := dial(ctx, host)
	if err != nil {
		return nil, err
	}
	requestID, err := newDaemonRequestID()
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	response, err := controlRequest(ctx, conn, protocol.Control{Type: protocol.TypeHostInfo, RequestID: requestID})
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("verify host %s: %w", host.Alias, err)
	}
	if response.Type == protocol.TypeError {
		_ = conn.Close()
		return nil, daemonResponseError("host identity", response.Message)
	}
	if response.Type != protocol.TypeHostInfoResult || response.Host == nil {
		_ = conn.Close()
		return nil, fmt.Errorf("verify host %s: response has type %q without host info", host.Alias, response.Type)
	}
	if err := validateHostInfo(host, *response.Host); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func validateHostInfo(expected HostRecord, actual protocol.HostInfo) error {
	if actual.ID != expected.ID || actual.MeshIdentity != expected.MeshIdentity {
		return fmt.Errorf("host %s identity changed: expected %s (%s), received %s (%s)", expected.Alias, expected.ID, expected.MeshIdentity, actual.ID, actual.MeshIdentity)
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
				return nil, fmt.Errorf("host %s listed session %s for host %s", host.Alias, listed.ID, listed.HostID)
			}
		}
		return cloneSessionInfo(response.Sessions), nil
	case protocol.TypeError:
		return nil, daemonResponseError("session list", response.Message)
	default:
		return nil, fmt.Errorf("host %s list response has type %q", host.Alias, response.Type)
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
			return fmt.Errorf("host %s acknowledged session %s, want %s", host.Alias, response.SessionID, sessionID)
		}
		return nil
	case protocol.TypeError:
		return daemonResponseError(controlType+" "+sessionID, response.Message)
	default:
		return fmt.Errorf("host %s returned %q for %s", host.Alias, response.Type, controlType)
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
			return nil, fmt.Errorf("host %s returned logs for session %s, want %s", host.Alias, response.SessionID, sessionID)
		}
		if len(response.Output) > tail {
			return nil, fmt.Errorf("host %s returned %d log bytes, want at most %d", host.Alias, len(response.Output), tail)
		}
		return append([]byte(nil), response.Output...), nil
	case protocol.TypeError:
		return nil, daemonResponseError("logs "+sessionID, response.Message)
	default:
		return nil, fmt.Errorf("host %s returned %q for logs", host.Alias, response.Type)
	}
}
