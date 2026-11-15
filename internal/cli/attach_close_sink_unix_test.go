//go:build unix

package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/term"
	"github.com/creack/pty"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/terminal"
	"github.com/shaul/mesh/internal/transport"
)

func TestAttachmentClosePreservesInteractiveScreenAndError(t *testing.T) {
	for _, cause := range []error{io.EOF, fmt.Errorf("%w: device key is not approved", transport.ErrAuthentication)} {
		t.Run(cause.Error(), func(t *testing.T) { checkAttachmentCloseScreen(t, cause, false) })
	}
}

func TestAttachmentCloseKeepsRedirectedStderrWithTerminalOutput(t *testing.T) {
	checkAttachmentCloseScreen(t, io.EOF, true)
}

func checkAttachmentCloseScreen(t *testing.T, cause error, redirect bool) {
	t.Helper()
	master, screen, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = master.Close(); _ = screen.Close() })
	if _, err := term.MakeRaw(screen.Fd()); err != nil {
		t.Fatal(err)
	}
	sink := screen
	if redirect {
		sink, err = os.Create(filepath.Join(t.TempDir(), "client.err"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = sink.Close() })
	}
	const before = "\x1b[2J\x1b[HFixture Beta"
	const after = "\x1b[2;1Hready"
	_, _ = io.WriteString(screen, before)
	conn := &closeSinkConn{terminalErrorConn: terminalErrorConn{err: cause}}
	result, err := AttachWithTerminal(t.Context(), AttachOptions{SessionID: "7K3D", Conn: conn, Stderr: sink, Raw: true}, AttachTerminal{
		Input: bytes.NewReader(nil), Output: screen, Size: terminal.Size{Cols: 80, Rows: 24},
		CancelInput: func() {},
	})
	if !result.Established || result.Exited || result.Detached || result.LastSeq != 53 || !conn.closed.Load() {
		t.Fatalf("attachment close outcome = %+v, %v, closed=%t", result, err, conn.closed.Load())
	}
	checkCloseSinkError(t, cause, err)
	_, _ = io.WriteString(screen, after)
	want := before + `{"ok":true}` + restoreTerminalState + after
	got := make([]byte, len(want))
	if _, err := io.ReadFull(master, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("interactive attachment bytes = %q, want %q", got, want)
	}
	if !redirect {
		return
	}
	if _, err := sink.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	diagnostics, err := io.ReadAll(sink)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(diagnostics), `stage=established-stream classification=terminal failed=false error="EOF"`) {
		t.Fatalf("redirected close cause missing: %q", diagnostics)
	}
}

func checkCloseSinkError(t *testing.T, cause, got error) {
	t.Helper()
	if errors.Is(cause, io.EOF) {
		if got != nil {
			t.Fatalf("ordinary close returned %v", got)
		}
		return
	}
	want := presentError("session 7K3D", cause)
	if !errors.Is(got, cause) || got.Error() != want.Error() {
		t.Fatalf("command error changed: %v, want %v preserving cause", got, want)
	}
}

type closeSinkConn struct {
	terminalErrorConn
	data bool
}

func (c *closeSinkConn) ReadFrame() (protocol.Frame, error) {
	if !c.read {
		return c.terminalErrorConn.ReadFrame()
	}
	if !c.data {
		c.data = true
		return protocol.Frame{Kind: protocol.KindData, Seq: 42, Payload: []byte(`{"ok":true}`)}, nil
	}
	return c.terminalErrorConn.ReadFrame()
}
