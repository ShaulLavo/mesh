package transport

import (
	"bufio"
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
	testSocketCloseReply(t, nil, "EOF", false)
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
		t.Run(row.name, func(t *testing.T) { testSocketCloseReply(t, row.reply, "EOF", false) })
	}
}

func TestSocketConnClosePreservesInvalidReply(t *testing.T) {
	// A reserved opcode is a genuine protocol failure during the handshake.
	testSocketCloseReply(t, []byte{0x83, 0}, "received unknown opcode", false)
}

func TestSocketConnClosePreservesBufferedTruncatedFrame(t *testing.T) {
	testSocketCloseReply(t, []byte{0x88}, "EOF", true)
}

func testSocketCloseReply(t *testing.T, reply []byte, wantErr string, bufferPrefix bool) {
	t.Helper()
	peerDone := make(chan error, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peerDone <- replyToSocketClose(w, r, reply, bufferPrefix)
	}))
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

type closePeerWriter struct {
	http.ResponseWriter
	raw          net.Conn
	reader       *bufio.Reader
	bufferPrefix bool
}

func (w *closePeerWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	raw, buffer, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, fmt.Errorf("hijack close peer: %w", err)
	}
	w.raw, w.reader = raw, buffer.Reader
	if err := w.prepareReader(); err != nil {
		_ = raw.Close()
		return nil, nil, err
	}
	return raw, buffer, nil
}

func (w *closePeerWriter) prepareReader() error {
	if err := w.raw.SetDeadline(time.Now().Add(time.Second)); err != nil {
		return fmt.Errorf("bound close peer: %w", err)
	}
	if !w.bufferPrefix {
		return nil
	}
	// Force the Hijacker contract's buffered-data case before Accept returns.
	if _, err := w.reader.Peek(1); err != nil {
		return fmt.Errorf("buffer close prefix: %w", err)
	}
	return nil
}

func replyToSocketClose(w http.ResponseWriter, r *http.Request, reply []byte, bufferPrefix bool) error {
	peer := &closePeerWriter{ResponseWriter: w, bufferPrefix: bufferPrefix}
	ws, err := websocket.Accept(peer, r, nil)
	if err != nil {
		return fmt.Errorf("accept close peer: %w", err)
	}
	defer ws.CloseNow() //nolint:errcheck // deliberately terminate the peer without a close reply
	var frame [8]byte
	if _, err := io.ReadFull(peer.reader, frame[:]); err != nil {
		return fmt.Errorf("read close frame: %w", err)
	}
	if frame[0] != 0x88 || frame[1] != 0x82 {
		return fmt.Errorf("expected masked close frame, got %x", frame)
	}
	if len(reply) == 0 {
		return nil
	}
	_, err = peer.raw.Write(reply)
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
