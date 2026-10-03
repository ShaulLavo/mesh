package transport

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/shaul/mesh/internal/protocol"
)

func authIdentity(t testing.TB) (string, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(public), private
}

func TestAuthenticatedWebSocketStream(t *testing.T) {
	serverID, serverKey := authIdentity(t)
	clientID, clientKey := authIdentity(t)
	var allowed atomic.Bool
	allowed.Store(true)
	var dispatched atomic.Int32
	auth := &Authentication{Key: serverKey, Authorize: func(id string) bool { return id == clientID && allowed.Load() }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = ServeWithOptions(w, r, ServeOptions{Auth: auth, KeepAlive: KeepAlive{Interval: 10 * time.Millisecond, Timeout: time.Second}}, func(ctx context.Context, conn Conn) error {
			peer, ok := Peer(ctx)
			if !ok || peer.Identity != clientID {
				return fmt.Errorf("unexpected authenticated peer")
			}
			for {
				frame, err := conn.ReadFrame()
				if err != nil {
					return fmt.Errorf("fixture read: %w", err)
				}
				dispatched.Add(1)
				if err := conn.WriteFrame(frame); err != nil {
					return fmt.Errorf("fixture echo: %w", err)
				}
			}
		})
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	clientAuth := &Authentication{Key: clientKey, ExpectedIdentity: serverID}
	conn, err := DialOnce(ctx, server.URL, DialOptions{Auth: clientAuth, KeepAlive: KeepAlive{Interval: 10 * time.Millisecond, Timeout: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close() //nolint:errcheck // test cleanup
	payload, _ := (protocol.Control{Type: protocol.TypeCreate, RequestID: "approved"}).Encode()
	frame := protocol.Frame{Kind: protocol.KindControl, Payload: payload}
	if err := conn.WriteFrame(frame); err != nil {
		t.Fatal(err)
	}
	got, err := conn.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	assertFrameEqual(t, got, frame)
	time.Sleep(40 * time.Millisecond)
	if err := conn.WriteFrame(frame); err != nil {
		t.Fatalf("keepalive lost authenticated stream: %v", err)
	}
	if _, err := conn.ReadFrame(); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []*Authentication{{Key: clientKey, ExpectedIdentity: clientID}, {Key: serverKey, ExpectedIdentity: serverID}} {
		denied, err := DialOnce(ctx, server.URL, DialOptions{Auth: candidate})
		if denied != nil {
			_ = denied.Close()
		}
		if !errors.Is(err, ErrAuthentication) {
			t.Fatalf("untrusted device or pin returned %v; want permanent authentication denial", err)
		}
	}
	allowed.Store(false)
	if _, err := conn.ReadFrame(); err == nil {
		t.Fatal("revoked device dispatched control")
	}
	if dispatched.Load() != 2 {
		t.Fatalf("dispatches=%d, want 2", dispatched.Load())
	}
}

type recordingTLSConn struct {
	net.Conn
	last []byte
}

func (c *recordingTLSConn) Write(p []byte) (int, error) {
	c.last = append(c.last[:0], p...)
	n, err := c.Conn.Write(p)
	return n, err //nolint:wrapcheck // the recorder preserves TLS stream errors
}

func TestAuthenticatedRecordsRejectReplayAndTampering(t *testing.T) {
	for _, tamper := range []bool{false, true} {
		t.Run(fmt.Sprint(tamper), func(t *testing.T) {
			serverID, serverKey := authIdentity(t)
			clientID, clientKey := authIdentity(t)
			var dispatched atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = ServeWithOptions(w, r, ServeOptions{Auth: &Authentication{Key: serverKey, Authorize: func(id string) bool { return id == clientID }}}, func(_ context.Context, conn Conn) error {
					for {
						frame, err := conn.ReadFrame()
						if err != nil {
							return fmt.Errorf("fixture read: %w", err)
						}
						dispatched.Add(1)
						if err := conn.WriteFrame(frame); err != nil {
							return fmt.Errorf("fixture echo: %w", err)
						}
					}
				})
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			ws, _, err := websocket.Dial(ctx, server.URL, &websocket.DialOptions{Subprotocols: []string{AuthProtocol}}) //nolint:bodyclose // websocket.Dial closes its HTTP response body
			if err != nil {
				t.Fatal(err)
			}
			defer ws.CloseNow() //nolint:errcheck // test cleanup
			raw := &recordingTLSConn{Conn: websocket.NetConn(ctx, ws, websocket.MessageBinary)}
			cfg, err := (&Authentication{Key: clientKey, ExpectedIdentity: serverID}).config(false)
			if err != nil {
				t.Fatal(err)
			}
			secure := tls.Client(raw, cfg)
			if err := secure.HandshakeContext(ctx); err != nil {
				t.Fatal(err)
			}
			if err := confirmAuthentication(secure, false); err != nil {
				t.Fatal(err)
			}
			payload, _ := (protocol.Control{Type: protocol.TypeHostInfo}).Encode()
			encoded, err := encodeFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload})
			if err != nil {
				t.Fatal(err)
			}
			// Mesh frames may cross both TLS records and WebSocket message boundaries.
			for _, piece := range [][]byte{encoded[:2], encoded[2:]} {
				if _, err := secure.Write(piece); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := protocol.NewReader(secure).ReadFrame(); err != nil {
				t.Fatal(err)
			}
			record := append([]byte(nil), raw.last...)
			if tamper {
				record[len(record)-1] ^= 1
			}
			if _, err := raw.Conn.Write(record); err != nil {
				t.Fatal(err)
			}
			if _, err := protocol.NewReader(secure).ReadFrame(); err == nil {
				t.Fatal("replayed or tampered TLS record accepted")
			}
			if dispatched.Load() != 1 {
				t.Fatalf("dispatches=%d, want 1", dispatched.Load())
			}
		})
	}
}

func TestAuthenticationHandshakeCancellationAndByteLimit(t *testing.T) {
	serverID, _ := authIdentity(t)
	_, clientKey := authIdentity(t)
	closed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{AuthProtocol}})
		if err != nil {
			return
		}
		defer ws.CloseNow() //nolint:errcheck // fixture cleanup
		for {
			if _, _, err := ws.Read(r.Context()); err != nil {
				close(closed)
				return
			}
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	conn, err := DialOnce(ctx, server.URL, DialOptions{Auth: &Authentication{Key: clientKey, ExpectedIdentity: serverID}})
	if conn != nil || !errors.Is(err, ErrAuthentication) {
		t.Fatalf("silent peer returned connection=%v, error=%v", conn, err)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("canceled handshake retained the socket")
	}
	reader, writer := net.Pipe()
	defer reader.Close() //nolint:errcheck // fixture cleanup
	defer writer.Close() //nolint:errcheck // fixture cleanup
	go func() { _, _ = writer.Write(make([]byte, handshakeByteLimit)) }()
	bounded := &handshakeConn{Conn: reader, remaining: handshakeByteLimit}
	if _, err := io.ReadFull(bounded, make([]byte, handshakeByteLimit)); err != nil {
		t.Fatal(err)
	}
	if _, err := bounded.Read(make([]byte, 1)); err == nil {
		t.Fatal("handshake read past its byte budget")
	}
}
