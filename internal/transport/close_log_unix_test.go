//go:build unix

package transport

import (
	"io"
	"log"
	"testing"

	"github.com/charmbracelet/x/term"
	"github.com/creack/pty"
)

func TestTransportCloseLogsLeaveInteractiveScreenUntouched(t *testing.T) {
	master, screen, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = master.Close(); _ = screen.Close() })
	if _, err := term.MakeRaw(screen.Fd()); err != nil {
		t.Fatal(err)
	}
	previous := log.Writer()
	log.SetOutput(screen)
	t.Cleanup(func() { log.SetOutput(previous) })
	const before = "\x1b[2J\x1b[HFixture Beta"
	const after = "\x1b[2;1Hconnected"
	_, _ = io.WriteString(screen, before)
	checkClientHandshakeEOF(t)
	_, _ = io.WriteString(screen, after)
	got := make([]byte, len(before+after))
	if _, err := io.ReadFull(master, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != before+after {
		t.Fatalf("handshake polluted interactive screen: %q", got)
	}
}
