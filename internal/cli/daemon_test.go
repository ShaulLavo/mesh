package cli

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

func TestCreateViaDaemonRoundTrip(t *testing.T) {
	wantCommand := []string{"sh", "-c", "printf ready"}
	socketPath, serverDone := startDaemonCreateServer(t, func(conn transport.Conn, request protocol.Control) error {
		if request.Type != protocol.TypeCreate {
			return fmt.Errorf("request type = %q, want %q", request.Type, protocol.TypeCreate)
		}
		requestID, err := hex.DecodeString(request.RequestID)
		if err != nil || len(requestID) != 16 {
			return fmt.Errorf("request ID = %q, want 16 random bytes encoded as hex", request.RequestID)
		}
		if !slices.Equal(request.Command, wantCommand) {
			return fmt.Errorf("command = %q, want %q", request.Command, wantCommand)
		}
		if request.Cwd != "/tmp/project" || request.Cols != 132 || request.Rows != 43 {
			return fmt.Errorf("create options = cwd %q, %dx%d", request.Cwd, request.Cols, request.Rows)
		}
		return writeDaemonControl(conn, protocol.Control{
			Type:      protocol.TypeCreated,
			RequestID: request.RequestID,
			SessionID: "7K3D",
		})
	})

	got, err := CreateViaDaemon(context.Background(), DaemonCreateOptions{
		SocketPath: socketPath,
		Command:    wantCommand,
		Cwd:        "/tmp/project",
		Cols:       132,
		Rows:       43,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "7K3D" {
		t.Fatalf("session ID = %q, want 7K3D", got)
	}
	awaitDaemonServer(t, serverDone)
}

func TestCreateViaDaemonRejectsInvalidOptionsBeforeDial(t *testing.T) {
	missingSocket := filepath.Join(t.TempDir(), "missing.sock")
	tests := []struct {
		name string
		ctx  context.Context
		opts DaemonCreateOptions
	}{
		{name: "nil context", opts: DaemonCreateOptions{SocketPath: missingSocket, Command: []string{"sh"}}},
		{name: "empty socket", ctx: context.Background(), opts: DaemonCreateOptions{Command: []string{"sh"}}},
		{name: "blank socket", ctx: context.Background(), opts: DaemonCreateOptions{SocketPath: " \t", Command: []string{"sh"}}},
		{name: "missing command", ctx: context.Background(), opts: DaemonCreateOptions{SocketPath: missingSocket}},
		{name: "empty executable", ctx: context.Background(), opts: DaemonCreateOptions{SocketPath: missingSocket, Command: []string{"", "arg"}}},
		{name: "negative columns", ctx: context.Background(), opts: DaemonCreateOptions{SocketPath: missingSocket, Command: []string{"sh"}, Cols: -1}},
		{name: "negative rows", ctx: context.Background(), opts: DaemonCreateOptions{SocketPath: missingSocket, Command: []string{"sh"}, Rows: -1}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := CreateViaDaemon(test.ctx, test.opts)
			if err == nil {
				t.Fatal("invalid options were accepted")
			}
			if errors.Is(err, ErrDaemonUnavailable) {
				t.Fatalf("validation reached the daemon dial: %v", err)
			}
		})
	}
}

func TestCreateViaDaemonMapsUnavailableDialFailures(t *testing.T) {
	t.Run("missing socket", func(t *testing.T) {
		_, err := CreateViaDaemon(context.Background(), DaemonCreateOptions{
			SocketPath: filepath.Join(t.TempDir(), "missing.sock"),
			Command:    []string{"sh"},
		})
		if !errors.Is(err, ErrDaemonUnavailable) {
			t.Fatalf("error = %v, want ErrDaemonUnavailable", err)
		}
	})

	t.Run("refused socket", func(t *testing.T) {
		socketPath := filepath.Join(t.TempDir(), "stale.sock")
		listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		listener.SetUnlinkOnClose(false)
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Remove(socketPath) })

		_, err = CreateViaDaemon(context.Background(), DaemonCreateOptions{
			SocketPath: socketPath,
			Command:    []string{"sh"},
		})
		if !errors.Is(err, ErrDaemonUnavailable) {
			t.Fatalf("error = %v, want ErrDaemonUnavailable", err)
		}
	})
}

func TestCreateViaDaemonPreservesOtherDialFailures(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := CreateViaDaemon(cancelled, DaemonCreateOptions{
		SocketPath: filepath.Join(t.TempDir(), "missing.sock"),
		Command:    []string{"sh"},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if errors.Is(err, ErrDaemonUnavailable) {
		t.Fatalf("cancelled dial was mapped to ErrDaemonUnavailable: %v", err)
	}

	tooLong := filepath.Join(t.TempDir(), strings.Repeat("x", 256))
	_, err = CreateViaDaemon(context.Background(), DaemonCreateOptions{
		SocketPath: tooLong,
		Command:    []string{"sh"},
	})
	if err == nil {
		t.Fatal("overlong Unix socket path was accepted")
	}
	if errors.Is(err, ErrDaemonUnavailable) {
		t.Fatalf("non-ENOENT/non-ECONNREFUSED dial failure was mapped to ErrDaemonUnavailable: %v", err)
	}
}

func TestCreateViaDaemonReturnsDaemonError(t *testing.T) {
	socketPath, serverDone := startDaemonCreateServer(t, func(conn transport.Conn, request protocol.Control) error {
		return writeDaemonControl(conn, protocol.Control{
			Type:      protocol.TypeError,
			RequestID: request.RequestID,
			Message:   "worker could not start",
		})
	})

	_, err := CreateViaDaemon(context.Background(), DaemonCreateOptions{
		SocketPath: socketPath,
		Command:    []string{"false"},
	})
	if err == nil || !strings.Contains(err.Error(), "worker could not start") {
		t.Fatalf("error = %v, want daemon message", err)
	}
	if errors.Is(err, ErrDaemonUnavailable) {
		t.Fatalf("daemon response was mapped to ErrDaemonUnavailable: %v", err)
	}
	awaitDaemonServer(t, serverDone)
}

func TestCreateViaDaemonRejectsInvalidResponses(t *testing.T) {
	tests := []struct {
		name  string
		frame func(protocol.Control) protocol.Frame
	}{
		{
			name: "non-control frame",
			frame: func(protocol.Control) protocol.Frame {
				return protocol.Frame{Kind: protocol.KindData, Payload: []byte("not a response")}
			},
		},
		{
			name: "malformed control",
			frame: func(protocol.Control) protocol.Frame {
				return protocol.Frame{Kind: protocol.KindControl, Payload: []byte("{")}
			},
		},
		{
			name: "mismatched request ID",
			frame: daemonControlFrame(func(request protocol.Control) protocol.Control {
				return protocol.Control{Type: protocol.TypeCreated, RequestID: request.RequestID + "-other", SessionID: "7K3D"}
			}),
		},
		{
			name: "unexpected response type",
			frame: daemonControlFrame(func(request protocol.Control) protocol.Control {
				return protocol.Control{Type: protocol.TypeOK, RequestID: request.RequestID}
			}),
		},
		{
			name: "non-canonical session ID",
			frame: daemonControlFrame(func(request protocol.Control) protocol.Control {
				return protocol.Control{Type: protocol.TypeCreated, RequestID: request.RequestID, SessionID: "7k3d"}
			}),
		},
		{
			name: "invalid session ID",
			frame: daemonControlFrame(func(request protocol.Control) protocol.Control {
				return protocol.Control{Type: protocol.TypeCreated, RequestID: request.RequestID, SessionID: "IIII"}
			}),
		},
		{
			name: "mismatched error request ID",
			frame: daemonControlFrame(func(request protocol.Control) protocol.Control {
				return protocol.Control{Type: protocol.TypeError, RequestID: request.RequestID + "-other", Message: "failure"}
			}),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			socketPath, serverDone := startDaemonCreateServer(t, func(conn transport.Conn, request protocol.Control) error {
				return conn.WriteFrame(test.frame(request))
			})
			_, err := CreateViaDaemon(context.Background(), DaemonCreateOptions{
				SocketPath: socketPath,
				Command:    []string{"sh"},
			})
			if err == nil {
				t.Fatal("invalid daemon response was accepted")
			}
			if errors.Is(err, ErrDaemonUnavailable) {
				t.Fatalf("protocol failure was mapped to ErrDaemonUnavailable: %v", err)
			}
			awaitDaemonServer(t, serverDone)
		})
	}
}

func TestCreateViaDaemonCancellationUnblocksRead(t *testing.T) {
	requestRead := make(chan struct{})
	socketPath, serverDone := startDaemonCreateServer(t, func(conn transport.Conn, _ protocol.Control) error {
		close(requestRead)
		if _, err := conn.ReadFrame(); err == nil {
			return errors.New("client connection remained open after cancellation")
		}
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	clientDone := make(chan error, 1)
	go func() {
		_, err := CreateViaDaemon(ctx, DaemonCreateOptions{
			SocketPath: socketPath,
			Command:    []string{"sh"},
		})
		clientDone <- err
	}()

	select {
	case <-requestRead:
	case <-time.After(time.Second):
		t.Fatal("daemon did not receive create request")
	}
	cancel()
	select {
	case err := <-clientDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("CreateViaDaemon remained blocked after cancellation")
	}
	awaitDaemonServer(t, serverDone)
}

func startDaemonCreateServer(t *testing.T, handle func(transport.Conn, protocol.Control) error) (string, <-chan error) {
	t.Helper()
	socketPath := filepath.Join(t.TempDir(), "daemon.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	done := make(chan error, 1)
	go func() {
		stream, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		conn, err := transport.NewStreamConn(stream)
		if err != nil {
			_ = stream.Close()
			done <- err
			return
		}
		defer conn.Close() //nolint:errcheck // test connection cleanup

		frame, err := conn.ReadFrame()
		if err != nil {
			done <- err
			return
		}
		if frame.Kind != protocol.KindControl {
			done <- fmt.Errorf("request frame kind = %d, want control", frame.Kind)
			return
		}
		request, err := protocol.DecodeControl(frame.Payload)
		if err != nil {
			done <- err
			return
		}
		done <- handle(conn, request)
	}()
	return socketPath, done
}

func awaitDaemonServer(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("daemon test server did not finish")
	}
}

func writeDaemonControl(conn transport.Conn, control protocol.Control) error {
	payload, err := control.Encode()
	if err != nil {
		return err
	}
	return conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload})
}

func daemonControlFrame(build func(protocol.Control) protocol.Control) func(protocol.Control) protocol.Frame {
	return func(request protocol.Control) protocol.Frame {
		payload, err := build(request).Encode()
		if err != nil {
			panic(err)
		}
		return protocol.Frame{Kind: protocol.KindControl, Payload: payload}
	}
}
