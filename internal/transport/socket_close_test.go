package transport

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestSocketConnClosePeerEOF(t *testing.T) {
	testSocketCloseReply(t, nil, "EOF")
}

func TestSocketConnClosePreservesTruncatedFrames(t *testing.T) {
	for _, row := range []struct {
		name  string
		reply []byte
	}{
		{"header", []byte{0x88}},
		{"close payload absent", []byte{0x88, 0x02}},
		{"close payload partial", []byte{0x88, 0x02, 0x03}},
		{"data payload partial", []byte{0x82, 0x02, 'a'}},
	} {
		t.Run(row.name, func(t *testing.T) { testSocketCloseReply(t, row.reply, "EOF") })
	}
}

func TestSocketConnClosePreservesInvalidReply(t *testing.T) {
	// A reserved opcode is a genuine protocol failure during the handshake.
	testSocketCloseReply(t, []byte{0x83, 0}, "received unknown opcode")
}

func testSocketCloseReply(t *testing.T, reply []byte, wantErr string) {
	t.Helper()
	peerDone := make(chan error, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peerDone <- replyToSocketClose(w, r, reply)
	}))
	server.Config.ConnContext = func(ctx context.Context, conn net.Conn) context.Context {
		return context.WithValue(ctx, closePeerKey{}, conn)
	}
	server.Start()
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	t.Cleanup(cancel)
	ws, _, err := websocket.Dial(ctx, server.URL, nil) //nolint:bodyclose // websocket owns the upgrade response
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })
	stream := &closeReadGate{ReadWriteCloser: websocket.NetConn(ctx, ws, websocket.MessageBinary), started: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(stream.unblock)
	conn := newSecureSocketConn(ws, stream, KeepAlive{Interval: time.Hour, Timeout: time.Second})
	// Hold the Mesh reader between records so Close owns the WebSocket reader.
	stop := context.AfterFunc(conn.ctx, stream.unblock)
	t.Cleanup(func() { stop() })
	select {
	case <-stream.started:
	case <-ctx.Done():
		t.Fatal("Mesh reader did not reach the record boundary")
	}
	err = conn.Close()
	if wantErr == "" && err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
	if wantErr != "" && (err == nil || !strings.Contains(err.Error(), wantErr)) {
		t.Errorf("Close = %v, want %v", err, wantErr)
	}
	if err := <-peerDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-conn.done:
	default:
		t.Fatal("Close returned before the Mesh reader stopped")
	}
}

type closePeerKey struct{}

func replyToSocketClose(w http.ResponseWriter, r *http.Request, reply []byte) error {
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return fmt.Errorf("accept close peer: %w", err)
	}
	defer ws.CloseNow() //nolint:errcheck // deliberately terminate the peer without a close reply
	raw := r.Context().Value(closePeerKey{}).(net.Conn)
	if err := raw.SetDeadline(time.Now().Add(time.Second)); err != nil {
		return fmt.Errorf("bound close peer: %w", err)
	}
	var frame [8]byte
	if _, err := io.ReadFull(raw, frame[:]); err != nil {
		return fmt.Errorf("read close frame: %w", err)
	}
	if frame[0] != 0x88 || frame[1] != 0x82 {
		return fmt.Errorf("expected masked close frame, got %x", frame)
	}
	if len(reply) == 0 {
		return nil
	}
	_, err = raw.Write(reply)
	if err != nil {
		return fmt.Errorf("write close reply: %w", err)
	}
	return nil
}

type closeReadGate struct {
	io.ReadWriteCloser
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *closeReadGate) Read(p []byte) (int, error) {
	close(g.started)
	<-g.release
	return g.ReadWriteCloser.Read(p) //nolint:wrapcheck // preserve the controlled stream's read error
}

func (g *closeReadGate) unblock() {
	g.once.Do(func() { close(g.release) })
}
