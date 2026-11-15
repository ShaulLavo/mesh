package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/coder/websocket"
	"github.com/shaul/mesh/internal/protocol"
)

func TestReconnectReturnsAuthenticationEOFWithoutRetry(t *testing.T) {
	logs := captureCloseLogs(t)
	old := newTestLink(false)
	conn := reconnectTestConnection(t, old, nil)
	cause := fmt.Errorf("%w: read Mesh peer acceptance: %w", ErrAuthentication, io.EOF)
	var attempts atomic.Int32
	conn.dial = func(context.Context, string, websocket.DialOptions, KeepAlive) (linkConn, error) {
		attempts.Add(1)
		return nil, cause
	}
	writeTestControl(t, conn, protocol.Control{Type: protocol.TypeAttach, SessionID: "7K3D"})
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := conn.ReadFrame()
	if !errors.Is(err, ErrAuthentication) || !errors.Is(err, io.EOF) {
		t.Fatalf("reconnect error = %v, want authentication and EOF", err)
	}
	if !strings.Contains(err.Error(), "reconnect:") || !strings.Contains(err.Error(), "peer acceptance:") {
		t.Fatalf("reconnect error lost its stage: %v", err)
	}
	if attempts.Load() != 1 || conn.current.link != nil {
		t.Fatalf("reconnect retained link or retried authentication: attempts=%d", attempts.Load())
	}
	for _, want := range []string{"stage=stream-read", "reconnect=true", "stage=reconnect-dial", "reconnect=false", "read Mesh peer acceptance: EOF"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("transport log %q missing %q", logs.String(), want)
		}
	}
}
