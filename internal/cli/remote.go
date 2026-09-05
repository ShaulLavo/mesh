package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/shaul/mesh/internal/dnsname"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/wake"
)

// HostDialer opens one protocol connection to an adopted host.
type HostDialer func(context.Context, HostRecord) (transport.Conn, error)

func dialHost(ctx context.Context, host HostRecord) (transport.Conn, error) {
	return transport.Dial(ctx, host.Endpoint, transport.DialOptions{
		Recover: func(ctx context.Context) error { return recoverHost(ctx, host) },
	})
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
	verifyCtx, cancelVerify := context.WithTimeout(ctx, remoteConnectTimeout)
	defer cancelVerify()
	response, err := controlRequest(verifyCtx, conn, protocol.Control{Type: protocol.TypeHostInfo, RequestID: requestID})
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
	rememberWakeGrant(ctx, response.Host.Wake)
	return conn, *response.Host, nil
}

func validateHostInfo(expected HostRecord, actual protocol.HostInfo) error {
	if actual.ID != expected.ID || actual.MeshIdentity != expected.MeshIdentity {
		return fmt.Errorf("host %s identity changed", expected.Alias)
	}
	if actual.Wake != nil {
		if err := validateHostWake(actual); err != nil {
			return fmt.Errorf("host %s returned invalid wake permission: %w", expected.Alias, err)
		}
	}
	if actual.PrivateName != "" {
		if err := dnsname.ValidatePrivateName(actual.PrivateName); err != nil {
			return fmt.Errorf("host %s returned an invalid private name", expected.Alias)
		}
	}
	return nil
}

func validateHostWake(host protocol.HostInfo) error {
	if host.Wake.TargetID != host.ID {
		return fmt.Errorf("permission names another target")
	}
	return wake.ValidateGrant(*host.Wake, time.Now())
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
	queryCtx, cancelQuery := context.WithTimeout(ctx, remoteConnectTimeout)
	defer cancelQuery()
	response, err := controlRequest(queryCtx, conn, protocol.Control{Type: protocol.TypeList, RequestID: requestID})
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
	return createRemoteSessionInDirectory(ctx, host, dial, command, "", cols, rows)
}

func createRemoteSessionInDirectory(ctx context.Context, host HostRecord, dial HostDialer, command []string, cwd string, cols, rows int) (string, error) {
	conn, err := openVerifiedHost(ctx, host, dial)
	if err != nil {
		return "", err
	}
	defer conn.Close() //nolint:errcheck // one create request
	requestID, err := newDaemonRequestID()
	if err != nil {
		return "", err
	}
	createCtx, cancelCreate := context.WithTimeout(ctx, remoteCreateTimeout)
	defer cancelCreate()
	response, err := controlRequest(createCtx, conn, protocol.Control{
		Type:      protocol.TypeCreate,
		RequestID: requestID,
		Term:      clientTerm(),
		Depth:     SessionDepth() + 1,
		Command:   append([]string(nil), command...),
		Cwd:       cwd,
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

func inspectRemoteSession(ctx context.Context, host HostRecord, dial HostDialer, sessionID string, previewCols, previewRows int) (SessionInspection, error) {
	if ctx == nil {
		return SessionInspection{}, fmt.Errorf("inspect session on host %s with nil context", host.Alias)
	}
	if err := ctx.Err(); err != nil {
		return SessionInspection{}, err
	}
	id, err := session.ParseID(sessionID)
	if err != nil || id != sessionID {
		return SessionInspection{}, fmt.Errorf("inspect invalid session ID %q", sessionID)
	}
	if err := protocol.ValidateInspectDimensions(previewCols, previewRows); err != nil {
		return SessionInspection{}, err
	}
	conn, err := openVerifiedHost(ctx, host, dial)
	if err != nil {
		return SessionInspection{}, err
	}
	defer conn.Close() //nolint:errcheck // one inspect request
	requestID, err := newDaemonRequestID()
	if err != nil {
		return SessionInspection{}, err
	}
	response, err := controlRequest(ctx, conn, protocol.Control{
		Type:        protocol.TypeInspect,
		RequestID:   requestID,
		SessionID:   sessionID,
		PreviewCols: previewCols,
		PreviewRows: previewRows,
	})
	if err != nil {
		return SessionInspection{}, err
	}
	return validateInspectionResponse(host, sessionID, previewCols, previewRows, response)
}

func validateInspectionResponse(host HostRecord, sessionID string, previewCols, previewRows int, response protocol.Control) (SessionInspection, error) {
	switch response.Type {
	case protocol.TypeInspected:
		if response.SessionID != sessionID {
			return SessionInspection{}, fmt.Errorf("host %s inspected a different session", host.Alias)
		}
		if response.Inspection == nil {
			return SessionInspection{}, fmt.Errorf("host %s returned no inspection for session %s", host.Alias, sessionID)
		}
		if err := protocol.ValidateSessionInspection(*response.Inspection); err != nil {
			return SessionInspection{}, fmt.Errorf("host %s returned an invalid inspection for session %s: %w", host.Alias, sessionID, err)
		}
		if len(response.Inspection.Preview) > previewRows {
			return SessionInspection{}, fmt.Errorf("host %s returned %d preview rows for session %s, want at most %d", host.Alias, len(response.Inspection.Preview), sessionID, previewRows)
		}
		for row, line := range response.Inspection.Preview {
			if width := ansi.StringWidth(line); width > previewCols {
				return SessionInspection{}, fmt.Errorf("host %s returned preview row %d with width %d for session %s, want at most %d", host.Alias, row, width, sessionID, previewCols)
			}
		}
		return inspectionFromProtocol(*response.Inspection), nil
	case protocol.TypeError:
		return SessionInspection{}, daemonResponseError("inspect "+sessionID, response.Message)
	default:
		return SessionInspection{}, fmt.Errorf("host %s returned an unexpected inspect response", host.Alias)
	}
}

func inspectionFromProtocol(source protocol.SessionInspection) SessionInspection {
	directorySource := SessionDirectoryUnknown
	switch source.DirectorySource {
	case protocol.DirectorySourceProcess:
		directorySource = SessionDirectoryProcess
	case protocol.DirectorySourceTerminal:
		directorySource = SessionDirectoryTerminal
	}
	var lastOutputAt *time.Time
	if source.LastOutputAt != nil {
		copy := *source.LastOutputAt
		lastOutputAt = &copy
	}
	return SessionInspection{
		ObservedAt:        source.ObservedAt,
		CurrentDirectory:  source.CurrentDirectory,
		DirectorySource:   directorySource,
		ForegroundCommand: source.ForegroundCommand,
		TerminalTitle:     source.TerminalTitle,
		LastOutputAt:      lastOutputAt,
		Attached:          source.Attached,
		Preview:           append([]string(nil), source.Preview...),
		StyledPreview:     copyPreviewLines(source.StyledPreview),
		Nested:            protocol.CloneSessionIdentities(source.Nested),
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
