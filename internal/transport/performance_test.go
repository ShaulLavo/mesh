package transport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Exercise the same timeout and Ping/Pong work as keepAliveLoop, without
// waiting fifteen seconds between samples or including connection setup.
func BenchmarkKeepAlivePing(b *testing.B) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
		if err != nil {
			b.Error(err)
			return
		}
		defer ws.CloseNow() //nolint:errcheck // benchmark cleanup
		_, _, _ = ws.Read(r.Context())
	}))
	b.Cleanup(server.Close)
	ws, _, err := websocket.Dial(context.Background(), server.URL, &websocket.DialOptions{CompressionMode: websocket.CompressionDisabled}) //nolint:bodyclose // websocket owns the response
	if err != nil {
		b.Fatal(err)
	}
	conn := newSocketConn(ws, KeepAlive{Interval: time.Hour, Timeout: time.Second})
	b.Cleanup(func() { _ = conn.Close() })
	b.ReportAllocs()
	for b.Loop() {
		ctx, cancel := context.WithTimeout(conn.ctx, time.Second)
		err := ws.Ping(ctx)
		cancel()
		if err != nil {
			b.Fatal(err)
		}
	}
}
