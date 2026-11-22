package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"testing"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

// A planned shutdown while the client has a resize queued must end the
// attachment socket like a close, not like a reset: the client reads EOF.
func TestServePlannedShutdownEndsQueuedClientWithEOF(t *testing.T) {
	stateDir := compactSocketTempDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	handlerRead := make(chan struct{})
	done := runRuntime(t, ctx, ListenerConfig{StateDir: stateDir}, func(ctx context.Context, conn transport.Conn) error {
		if _, err := conn.ReadFrame(); err != nil {
			return fmt.Errorf("read first frame: %w", err)
		}
		close(handlerRead)
		// The handler is between reads, as it is while it serves a request.
		<-ctx.Done()
		return nil
	})
	client := dialUnixRuntime(t, filepath.Join(stateDir, daemonSocketName))
	t.Cleanup(func() { _ = client.Close() })
	resize, err := (protocol.Control{Type: protocol.TypeResize, SessionID: "7K3D", Cols: 100, Rows: 30}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	frame := protocol.Frame{Kind: protocol.KindControl, Payload: resize}
	if err := client.WriteFrame(frame); err != nil {
		t.Fatal(err)
	}
	waitSignal(t, handlerRead, "handler to read the first frame")
	if err := client.WriteFrame(frame); err != nil {
		t.Fatal(err)
	}

	cancel()
	if err := waitRuntime(t, done); err != nil {
		t.Fatalf("Serve after cancellation: %v", err)
	}
	if _, err := client.ReadFrame(); !errors.Is(err, io.EOF) {
		t.Fatalf("client read after planned shutdown = %v, want EOF", err)
	}
}
