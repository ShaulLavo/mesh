package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/terminal"
	"github.com/shaul/mesh/internal/transport"
)

func TestAttachmentPreservesTypedTerminalErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		fail bool
	}{
		{"stream EOF", fmt.Errorf("read stream: %w", io.EOF), false},
		{"stream closed", fmt.Errorf("read stream: %w", net.ErrClosed), false},
		{"authentication EOF", fmt.Errorf("%w: read Mesh peer acceptance: %w", transport.ErrAuthentication, io.EOF), true},
		{"authentication closed", fmt.Errorf("%w: handshake: %w", transport.ErrAuthentication, net.ErrClosed), true},
		{"authentication denied", fmt.Errorf("%w: device key is not approved", transport.ErrAuthentication), true},
		{"invalid frame EOF", fmt.Errorf("%w: decode protocol frame: %w", transport.ErrInvalidFrame, io.EOF), true},
	} {
		t.Run(test.name, func(t *testing.T) { checkAttachmentTerminalError(t, test.err, test.fail) })
	}
}

func checkAttachmentTerminalError(t *testing.T, cause error, fail bool) {
	t.Helper()
	conn := &terminalErrorConn{err: cause}
	var cancellations atomic.Int32
	var diagnostics bytes.Buffer
	result, err := AttachWithTerminal(t.Context(), AttachOptions{SessionID: "7K3D", Conn: conn, Stderr: &diagnostics}, AttachTerminal{
		Input: bytes.NewReader(nil), Output: io.Discard, Size: terminal.Size{Cols: 80, Rows: 24},
		CancelInput: func() { cancellations.Add(1) },
	})
	if !result.Established || result.Detached || result.Exited || result.LastSeq != 42 {
		t.Fatalf("attachment ending = %#v", result)
	}
	if (err != nil) != fail || fail && !errors.Is(err, cause) {
		t.Fatalf("attachment error = %v, want failure=%v preserving %v", err, fail, cause)
	}
	if !conn.closed.Load() || cancellations.Load() != 1 {
		t.Fatal("attachment retained its connection or input relay")
	}
	for _, want := range []string{"stage=established-stream", "classification=terminal", fmt.Sprintf("failed=%t", fail), fmt.Sprintf("error=%q", cause)} {
		if !strings.Contains(diagnostics.String(), want) {
			t.Errorf("diagnostic %q missing %q", diagnostics.String(), want)
		}
	}
}

func TestAttachmentCloseLogsUnexpectedEOFBeforeAcknowledgment(t *testing.T) {
	var diagnostics bytes.Buffer
	conn := &terminalErrorConn{read: true, err: fmt.Errorf("attach response: %w", io.EOF)}
	state := attachmentOutput{opts: AttachOptions{SessionID: "7K3D", Stderr: &diagnostics}}
	result, err := state.read(t.Context(), conn, make(chan struct{}))
	if err != nil || result != (AttachResult{}) {
		t.Fatalf("logging changed EOF outcome: %#v, %v", result, err)
	}
	if !strings.Contains(diagnostics.String(), `stage=attach-response classification=terminal failed=false error="attach response: EOF"`) {
		t.Fatalf("attachment close cause missing: %q", diagnostics.String())
	}
}

type terminalErrorConn struct {
	err    error
	read   bool
	closed atomic.Bool
}

func (c *terminalErrorConn) ReadFrame() (protocol.Frame, error) {
	if !c.read {
		c.read = true
		return controlFrame(protocol.Control{Type: protocol.TypeAttached, SessionID: "7K3D", Seq: 42}), nil
	}
	return protocol.Frame{}, c.err
}

func (c *terminalErrorConn) WriteFrame(protocol.Frame) error { return nil }

func (c *terminalErrorConn) Close() error {
	c.closed.Store(true)
	return nil
}
