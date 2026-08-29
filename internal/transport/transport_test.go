package transport

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/shaul/mesh/internal/protocol"
)

func TestWebSocketFrameRoundTrip(t *testing.T) {
	t.Parallel()

	serverErr := make(chan error, 1)
	var extension string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		extension = r.Header.Get("Sec-WebSocket-Extensions")
		serverErr <- Serve(w, r, func(_ context.Context, conn Conn) error {
			for range 3 {
				frame, err := conn.ReadFrame()
				if err != nil {
					return err
				}
				if err := conn.WriteFrame(frame); err != nil {
					return err
				}
			}
			return nil
		})
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := Dial(ctx, server.URL, DialOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close() //nolint:errcheck // test cleanup

	sid, err := protocol.NewSessionID("7K3D")
	if err != nil {
		t.Fatal(err)
	}
	control, err := (protocol.Control{
		Type:      protocol.TypeAttach,
		SessionID: sid.String(),
		Cols:      120,
		Rows:      40,
	}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	want := []protocol.Frame{
		{Kind: protocol.KindControl, Payload: control},
		{Kind: protocol.KindData, Session: sid, Seq: 19, Payload: []byte("hello\x1b[0m")},
		{Kind: protocol.KindInput, Session: sid, Payload: []byte{0x03}},
	}

	for _, frame := range want {
		if err := conn.WriteFrame(frame); err != nil {
			t.Fatalf("write %v: %v", frame.Kind, err)
		}
		got, err := conn.ReadFrame()
		if err != nil {
			t.Fatalf("read %v: %v", frame.Kind, err)
		}
		assertFrameEqual(t, got, frame)
	}

	if err := <-serverErr; err != nil {
		t.Fatalf("serve: %v", err)
	}
	if extension != "" {
		t.Fatalf("compression negotiated in request: %q", extension)
	}
}

func TestStreamConnUsesTheSameFramesAsWebSocket(t *testing.T) {
	t.Parallel()

	left, right := net.Pipe()
	client, err := NewStreamConn(left)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewStreamConn(right)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close() //nolint:errcheck // test cleanup
	defer server.Close() //nolint:errcheck // test cleanup

	sid, err := protocol.NewSessionID("7K3D")
	if err != nil {
		t.Fatal(err)
	}
	want := protocol.Frame{Kind: protocol.KindData, Session: sid, Seq: 91, Payload: []byte("stream")}
	serverErr := make(chan error, 1)
	go func() {
		frame, err := server.ReadFrame()
		if err == nil {
			err = server.WriteFrame(frame)
		}
		serverErr <- err
	}()

	if err := client.WriteFrame(want); err != nil {
		t.Fatal(err)
	}
	got, err := client.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	assertFrameEqual(t, got, want)
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestServeDoesNotNegotiateCompression(t *testing.T) {
	t.Parallel()

	serverErr := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverErr <- Serve(w, r, func(context.Context, Conn) error { return nil })
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ws, response, err := websocket.Dial(ctx, server.URL, &websocket.DialOptions{
		CompressionMode: websocket.CompressionContextTakeover,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow() //nolint:errcheck // test cleanup after an assertion failure
	if extension := response.Header.Get("Sec-WebSocket-Extensions"); extension != "" {
		t.Fatalf("server negotiated compression: %q", extension)
	}
	if err := ws.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatal(err)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("serve: %v", err)
	}
}

func TestReconnectResumesEverySessionAtItsNextByte(t *testing.T) {
	t.Parallel()

	type attachRecord struct {
		connection int
		sessionID  string
		lastSeq    uint64
	}
	var (
		mu          sync.Mutex
		connections int
		attaches    []attachRecord
	)
	serverErr := make(chan error, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		connections++
		generation := connections
		mu.Unlock()

		serverErr <- Serve(w, r, func(_ context.Context, conn Conn) error {
			for range 2 {
				frame, err := conn.ReadFrame()
				if err != nil {
					return err
				}
				msg, err := protocol.DecodeControl(frame.Payload)
				if err != nil {
					return err
				}
				if msg.Type != protocol.TypeAttach || msg.LastSeq == nil {
					return fmt.Errorf("connection %d: attach = %+v", generation, msg)
				}
				mu.Lock()
				attaches = append(attaches, attachRecord{generation, msg.SessionID, *msg.LastSeq})
				mu.Unlock()
			}

			for _, sessionID := range []string{"7K3D", "8M4E"} {
				sid, err := protocol.NewSessionID(sessionID)
				if err != nil {
					return err
				}
				seq := uint64(0)
				payload := []byte(sessionID + "-one")
				if generation > 1 {
					seq = uint64(len(payload))
					payload = []byte(sessionID + "-two")
				}
				attached, err := (protocol.Control{
					Type:      protocol.TypeAttached,
					SessionID: sessionID,
					Seq:       seq,
				}).Encode()
				if err != nil {
					return err
				}
				if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: attached}); err != nil {
					return err
				}
				if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindData, Session: sid, Seq: seq, Payload: payload}); err != nil {
					return err
				}
			}
			if generation == 1 {
				return errTestDisconnect
			}
			return nil
		})
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := Dial(ctx, server.URL, DialOptions{
		Backoff: Backoff{Initial: time.Millisecond, Max: 5 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close() //nolint:errcheck // test cleanup

	for _, sessionID := range []string{"7K3D", "8M4E"} {
		zero := uint64(0)
		payload, err := (protocol.Control{
			Type:      protocol.TypeAttach,
			SessionID: sessionID,
			LastSeq:   &zero,
		}).Encode()
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload}); err != nil {
			t.Fatal(err)
		}
	}

	got := map[string][]byte{"7K3D": nil, "8M4E": nil}
	for len(got["7K3D"]) < len("7K3D-one7K3D-two") || len(got["8M4E"]) < len("8M4E-one8M4E-two") {
		frame, err := conn.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		if frame.Kind == protocol.KindData {
			got[frame.Session.String()] = append(got[frame.Session.String()], frame.Payload...)
		}
	}

	if string(got["7K3D"]) != "7K3D-one7K3D-two" {
		t.Fatalf("7K3D output = %q", got["7K3D"])
	}
	if string(got["8M4E"]) != "8M4E-one8M4E-two" {
		t.Fatalf("8M4E output = %q", got["8M4E"])
	}

	mu.Lock()
	defer mu.Unlock()
	wantAttaches := []attachRecord{
		{1, "7K3D", 0},
		{1, "8M4E", 0},
		{2, "7K3D", uint64(len("7K3D-one"))},
		{2, "8M4E", uint64(len("8M4E-one"))},
	}
	if fmt.Sprint(attaches) != fmt.Sprint(wantAttaches) {
		t.Fatalf("attaches = %#v, want %#v", attaches, wantAttaches)
	}
	for range 2 {
		if err := <-serverErr; err != nil && err != errTestDisconnect {
			t.Fatalf("serve: %v", err)
		}
	}
}

func TestKeepAliveClosesPeerThatWithholdsPong(t *testing.T) {
	t.Parallel()

	serverErr := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			CompressionMode: websocket.CompressionDisabled,
			OnPingReceived: func(context.Context, []byte) bool {
				return false
			},
		})
		if err != nil {
			serverErr <- err
			return
		}
		defer ws.CloseNow() //nolint:errcheck // test peer deliberately dies
		for {
			if _, _, err := ws.Read(context.Background()); err != nil {
				serverErr <- nil
				return
			}
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, server.URL, &websocket.DialOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	conn := newSocketConn(ws, KeepAlive{Interval: 10 * time.Millisecond, Timeout: 20 * time.Millisecond})
	defer conn.Close() //nolint:errcheck // test cleanup

	started := time.Now()
	_, err = conn.ReadFrame()
	if err == nil || !strings.Contains(err.Error(), "keepalive") {
		t.Fatalf("read error = %v, want keepalive failure", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("dead peer detected after %v, want at most 250ms", elapsed)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("server: %v", err)
	}
}

func TestOlderGenerationCannotMoveResumePositionBackward(t *testing.T) {
	t.Parallel()

	zero := uint64(0)
	conn := &reconnectingConn{resumes: map[string]resumeState{
		"7K3D": {
			attach: protocol.Control{
				Type:      protocol.TypeAttach,
				SessionID: "7K3D",
				LastSeq:   &zero,
			},
			next:     10,
			haveNext: true,
		},
	}}
	attached, err := (protocol.Control{
		Type:      protocol.TypeAttached,
		SessionID: "7K3D",
		Seq:       5,
	}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.observeFrame(protocol.Frame{Kind: protocol.KindControl, Payload: attached}); err != nil {
		t.Fatal(err)
	}

	sid, err := protocol.NewSessionID("7K3D")
	if err != nil {
		t.Fatal(err)
	}
	frame, drop, err := conn.observeFrame(protocol.Frame{
		Kind:    protocol.KindData,
		Session: sid,
		Seq:     5,
		Payload: []byte("abcdefgh"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if drop || frame.Seq != 10 || string(frame.Payload) != "fgh" {
		t.Fatalf("trimmed frame = %+v, drop = %v", frame, drop)
	}

	frames, err := conn.resumeFrames()
	if err != nil {
		t.Fatal(err)
	}
	msg, err := protocol.DecodeControl(frames[0].Payload)
	if err != nil {
		t.Fatal(err)
	}
	if msg.LastSeq == nil || *msg.LastSeq != 13 {
		t.Fatalf("resume sequence = %v, want 13", msg.LastSeq)
	}
}

func TestEncoderPreservesAdditiveSessionFrameKinds(t *testing.T) {
	t.Parallel()

	sid, err := protocol.NewSessionID("7K3D")
	if err != nil {
		t.Fatal(err)
	}
	kind := protocol.Kind(0x7e)
	wire, err := encodeFrame(protocol.Frame{Kind: kind, Session: sid, Payload: []byte("snapshot")})
	if err != nil {
		t.Fatal(err)
	}
	if wire[0] != byte(kind) {
		t.Fatalf("wire kind = 0x%02x, want 0x%02x", wire[0], kind)
	}
	// The current protocol reader does not know the additive kind yet. Reading
	// the same bytes as KindInput verifies the shared session+payload layout.
	wire[0] = byte(protocol.KindInput)
	frame, err := protocol.NewReader(bytes.NewReader(wire)).ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if frame.Session != sid || string(frame.Payload) != "snapshot" {
		t.Fatalf("session frame = %+v", frame)
	}
}

func TestBackoffDelayIsJitteredAndCapped(t *testing.T) {
	t.Parallel()

	opts := Backoff{Initial: 100 * time.Millisecond, Max: 250 * time.Millisecond, Jitter: 0.2}
	if got := backoffDelay(0, opts, 0); got != 80*time.Millisecond {
		t.Fatalf("minimum initial delay = %v, want 80ms", got)
	}
	if got := backoffDelay(1, opts, 0.5); got != 200*time.Millisecond {
		t.Fatalf("middle second delay = %v, want 200ms", got)
	}
	if got := backoffDelay(math.MaxInt, opts, 1); got != opts.Max {
		t.Fatalf("capped delay = %v, want %v", got, opts.Max)
	}
}

func assertFrameEqual(t *testing.T, got, want protocol.Frame) {
	t.Helper()
	if got.Kind != want.Kind || got.Session != want.Session || got.Seq != want.Seq || !bytes.Equal(got.Payload, want.Payload) {
		t.Fatalf("frame = %+v, want %+v", got, want)
	}
}

type testError string

func (e testError) Error() string { return string(e) }

const errTestDisconnect = testError("test disconnect")
