package transport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Abort drops a connection whose peer never answers the close handshake,
// and the connection reports itself dead before its reader has finished.
func TestAbortDoesNotWaitForASilentPeer(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
		if err != nil {
			return
		}
		// Silent: never reads, so a close frame is never answered.
		<-release
		_ = ws.CloseNow()
	}))
	defer server.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, server.URL, &websocket.DialOptions{CompressionMode: websocket.CompressionDisabled}) //nolint:bodyclose // websocket.Dial owns and closes its HTTP response body
	if err != nil {
		t.Fatal(err)
	}
	conn := newSocketConn(ws, KeepAlive{Interval: time.Hour, Timeout: time.Hour})
	if Stopped(conn) {
		t.Fatal("a live connection reported itself stopped")
	}
	aborted := make(chan error, 1)
	go func() { aborted <- Abort(conn) }()
	select {
	case err := <-aborted:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("Abort waited for the silent peer")
	}
	if !Stopped(conn) {
		t.Fatal("an aborted connection did not report itself stopped")
	}
	if _, err := conn.ReadFrame(); err == nil {
		t.Fatal("read on an aborted connection succeeded")
	}
}
