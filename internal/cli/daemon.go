package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/transport"
)

// ErrDaemonUnavailable identifies the two local dial failures for which a CLI
// may safely fall back to direct session recovery.
var ErrDaemonUnavailable = errors.New("cli: daemon unavailable")

// DaemonCreateOptions describes a session to create through the local daemon.
// Zero terminal dimensions leave the daemon's defaults in effect.
type DaemonCreateOptions struct {
	SocketPath string
	Command    []string
	Cwd        string
	Cols       int
	Rows       int
}

// CreateViaDaemon asks the local daemon to create one detached session. The
// connection is deliberately one-shot: one request produces one response.
func CreateViaDaemon(ctx context.Context, opts DaemonCreateOptions) (string, error) {
	if err := validateDaemonCreateOptions(ctx, opts); err != nil {
		return "", err
	}

	requestID, err := newDaemonRequestID()
	if err != nil {
		return "", err
	}
	request := protocol.Control{
		Type:      protocol.TypeCreate,
		RequestID: requestID,
		Command:   append([]string(nil), opts.Command...),
		Term:      clientTerm(),
		Depth:     SessionDepth() + 1,
		Cwd:       opts.Cwd,
		Cols:      opts.Cols,
		Rows:      opts.Rows,
	}
	response, err := daemonControlRequest(ctx, opts.SocketPath, request)
	if err != nil {
		return "", err
	}
	return validateDaemonCreateResponse(response)
}

// ListViaDaemon returns the sessions in the daemon's durable local catalog.
func ListViaDaemon(ctx context.Context, socketPath string) ([]protocol.SessionInfo, error) {
	if err := validateDaemonBoundary(ctx, socketPath, "list"); err != nil {
		return nil, err
	}
	requestID, err := newDaemonRequestID()
	if err != nil {
		return nil, err
	}
	response, err := daemonControlRequest(ctx, socketPath, protocol.Control{
		Type:      protocol.TypeList,
		RequestID: requestID,
	})
	if err != nil {
		return nil, err
	}
	switch response.Type {
	case protocol.TypeListed:
		for _, listed := range response.Sessions {
			if err := validateDaemonSession(listed); err != nil {
				return nil, err
			}
		}
		return response.Sessions, nil
	case protocol.TypeError:
		return nil, daemonResponseError("session list", response.Message)
	default:
		return nil, errors.New("cli: daemon returned an unexpected session-list response")
	}
}

func daemonControlRequest(ctx context.Context, socketPath string, request protocol.Control) (protocol.Control, error) {
	stream, err := (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		return protocol.Control{}, daemonDialError(socketPath, err)
	}
	conn, err := transport.NewStreamConn(stream)
	if err != nil {
		_ = stream.Close()
		return protocol.Control{}, fmt.Errorf("cli: adapt daemon connection: %w", err)
	}
	defer func() {
		_ = conn.Close()
	}()
	return controlRequest(ctx, conn, request)
}

func controlRequest(ctx context.Context, conn transport.Conn, request protocol.Control) (protocol.Control, error) {
	payload, err := request.Encode()
	if err != nil {
		return protocol.Control{}, fmt.Errorf("cli: encode %s request: %w", request.Type, err)
	}
	stopCancellation := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCancellation()

	if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload}); err != nil {
		return protocol.Control{}, daemonOperationError(ctx, "write "+request.Type+" request", err)
	}
	frame, err := conn.ReadFrame()
	if err != nil {
		return protocol.Control{}, daemonOperationError(ctx, "read "+request.Type+" response", err)
	}
	if frame.Kind != protocol.KindControl {
		return protocol.Control{}, fmt.Errorf("cli: %s response has frame kind %d, want control", request.Type, frame.Kind)
	}
	response, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return protocol.Control{}, presentError("cli: decode "+request.Type+" response", err)
	}
	if response.RequestID != request.RequestID {
		return protocol.Control{}, fmt.Errorf("cli: %s response has a mismatched request ID", request.Type)
	}
	return response, nil
}

func validateDaemonCreateOptions(ctx context.Context, opts DaemonCreateOptions) error {
	if err := validateDaemonBoundary(ctx, opts.SocketPath, "create"); err != nil {
		return err
	}
	if len(opts.Command) == 0 || opts.Command[0] == "" {
		return errors.New("cli: create via daemon with no command")
	}
	if opts.Cols < 0 || opts.Rows < 0 {
		return fmt.Errorf("cli: create via daemon with negative terminal size %dx%d", opts.Cols, opts.Rows)
	}
	return nil
}

func validateDaemonBoundary(ctx context.Context, socketPath, operation string) error {
	if ctx == nil {
		return fmt.Errorf("cli: %s via daemon with nil context", operation)
	}
	if strings.TrimSpace(socketPath) == "" {
		return fmt.Errorf("cli: %s via daemon with empty socket path", operation)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("cli: %s via daemon: %w", operation, err)
	}
	return nil
}

func newDaemonRequestID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("cli: generate daemon request ID: %w", err)
	}
	return hex.EncodeToString(random[:]), nil
}

func daemonDialError(socketPath string, err error) error {
	if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("cli: dial daemon %s: %w: %w", socketPath, ErrDaemonUnavailable, err)
	}
	return fmt.Errorf("cli: dial daemon %s: %w", socketPath, err)
}

func daemonOperationError(ctx context.Context, operation string, err error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		err = contextErr
	}
	return presentError("cli: "+operation, err)
}

func validateDaemonCreateResponse(response protocol.Control) (string, error) {
	switch response.Type {
	case protocol.TypeCreated:
		id, err := session.ParseID(response.SessionID)
		if err != nil || id != response.SessionID {
			return "", errors.New("cli: daemon returned an invalid session ID")
		}
		return id, nil
	case protocol.TypeError:
		return "", daemonResponseError("session creation", response.Message)
	default:
		return "", errors.New("cli: daemon returned an unexpected session-create response")
	}
}

func daemonResponseError(operation, message string) error {
	message = safeRemoteText(message)
	if message == "" {
		return fmt.Errorf("cli: daemon rejected %s", operation)
	}
	return fmt.Errorf("cli: daemon rejected %s: %s", operation, message)
}

func validateDaemonSession(listed protocol.SessionInfo) error {
	id, err := session.ParseID(listed.ID)
	if err != nil || id != listed.ID {
		return errors.New("cli: daemon listed an invalid session ID")
	}
	if strings.TrimSpace(listed.HostID) == "" {
		return errors.New("cli: daemon listed a session without a host ID")
	}
	if len(listed.Command) == 0 || listed.Command[0] == "" {
		return errors.New("cli: daemon listed a session without a command")
	}
	if listed.CreatedAt.IsZero() || listed.CreatedAt.UnixMilli() < 0 {
		return errors.New("cli: daemon listed a session with an invalid creation time")
	}
	switch storage.SessionState(listed.State) {
	case storage.StateRunning, storage.StateDetached, storage.StateInterrupted:
		if listed.ExitCode != nil {
			return errors.New("cli: daemon listed an active session with an exit code")
		}
	case storage.StateExited:
		if listed.ExitCode == nil {
			return errors.New("cli: daemon listed an exited session without an exit code")
		}
	default:
		return errors.New("cli: daemon listed a session with an invalid state")
	}
	return nil
}
