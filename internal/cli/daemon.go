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
		Cwd:       opts.Cwd,
		Cols:      opts.Cols,
		Rows:      opts.Rows,
	}
	payload, err := request.Encode()
	if err != nil {
		return "", fmt.Errorf("cli: encode %s request: %w", protocol.TypeCreate, err)
	}

	stream, err := (&net.Dialer{}).DialContext(ctx, "unix", opts.SocketPath)
	if err != nil {
		return "", daemonDialError(opts.SocketPath, err)
	}
	conn, err := transport.NewStreamConn(stream)
	if err != nil {
		_ = stream.Close()
		return "", fmt.Errorf("cli: adapt daemon connection: %w", err)
	}
	stopCancellation := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer func() {
		stopCancellation()
		_ = conn.Close()
	}()

	if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload}); err != nil {
		return "", daemonOperationError(ctx, "write create request", err)
	}
	frame, err := conn.ReadFrame()
	if err != nil {
		return "", daemonOperationError(ctx, "read create response", err)
	}
	return validateDaemonCreateResponse(requestID, frame)
}

func validateDaemonCreateOptions(ctx context.Context, opts DaemonCreateOptions) error {
	if ctx == nil {
		return errors.New("cli: create via daemon with nil context")
	}
	if strings.TrimSpace(opts.SocketPath) == "" {
		return errors.New("cli: create via daemon with empty socket path")
	}
	if len(opts.Command) == 0 || opts.Command[0] == "" {
		return errors.New("cli: create via daemon with no command")
	}
	if opts.Cols < 0 || opts.Rows < 0 {
		return fmt.Errorf("cli: create via daemon with negative terminal size %dx%d", opts.Cols, opts.Rows)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("cli: create via daemon: %w", err)
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
	return fmt.Errorf("cli: %s: %w", operation, err)
}

func validateDaemonCreateResponse(requestID string, frame protocol.Frame) (string, error) {
	if frame.Kind != protocol.KindControl {
		return "", fmt.Errorf("cli: create response has frame kind %d, want control", frame.Kind)
	}
	response, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return "", fmt.Errorf("cli: decode create response: %w", err)
	}
	if response.RequestID != requestID {
		return "", fmt.Errorf("cli: create response request ID %q does not match %q", response.RequestID, requestID)
	}

	switch response.Type {
	case protocol.TypeCreated:
		id, err := session.ParseID(response.SessionID)
		if err != nil || id != response.SessionID {
			return "", fmt.Errorf("cli: daemon returned invalid session ID %q", response.SessionID)
		}
		return id, nil
	case protocol.TypeError:
		if response.Message == "" {
			return "", errors.New("cli: daemon rejected session creation")
		}
		return "", fmt.Errorf("cli: daemon rejected session creation: %s", response.Message)
	default:
		return "", fmt.Errorf("cli: create response has type %q, want %q or %q", response.Type, protocol.TypeCreated, protocol.TypeError)
	}
}
