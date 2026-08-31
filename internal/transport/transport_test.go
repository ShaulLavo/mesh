package transport

import (
	"bytes"
	"context"
	"errors"
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
			for range 4 {
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
		{Kind: protocol.KindSnapshot, Session: sid, Payload: []byte("\x1b[2Jrendered")},
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
	dialOptions := &websocket.DialOptions{
		CompressionMode: websocket.CompressionContextTakeover,
	}
	ws, response, err := websocket.Dial(ctx, server.URL, dialOptions) //nolint:bodyclose // websocket.Dial owns and closes its HTTP response body
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

func TestReconnectBeforeSnapshotResumesFromLastDeliveredData(t *testing.T) {
	t.Parallel()

	var (
		mu          sync.Mutex
		connections int
	)
	resumedAt := make(chan uint64, 1)
	serverErr := make(chan error, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		connections++
		generation := connections
		mu.Unlock()

		serverErr <- Serve(w, r, func(_ context.Context, conn Conn) error {
			frame, err := conn.ReadFrame()
			if err != nil {
				return err
			}
			attach, err := protocol.DecodeControl(frame.Payload)
			if err != nil {
				return err
			}
			if attach.LastSeq == nil {
				return fmt.Errorf("connection %d: attach has no resume offset", generation)
			}

			if generation == 1 {
				payload, err := (protocol.Control{
					Type:      protocol.TypeAttached,
					SessionID: attach.SessionID,
					Seq:       100,
					Snapshot:  true,
				}).Encode()
				if err != nil {
					return err
				}
				if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload}); err != nil {
					return err
				}
				return errTestDisconnect
			}

			resumedAt <- *attach.LastSeq
			payload, err := (protocol.Control{
				Type:      protocol.TypeAttached,
				SessionID: attach.SessionID,
				Seq:       *attach.LastSeq,
			}).Encode()
			if err != nil {
				return err
			}
			return conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload})
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

	lastSeq := uint64(10)
	payload, err := (protocol.Control{
		Type:      protocol.TypeAttach,
		SessionID: "7K3D",
		LastSeq:   &lastSeq,
	}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload}); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		frame, err := conn.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		if frame.Kind != protocol.KindControl {
			t.Fatalf("frame kind = %v, want control", frame.Kind)
		}
	}
	if got := <-resumedAt; got != lastSeq {
		t.Fatalf("resume offset after incomplete snapshot = %d, want %d", got, lastSeq)
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
	dialOptions := &websocket.DialOptions{
		CompressionMode: websocket.CompressionDisabled,
	}
	ws, _, err := websocket.Dial(ctx, server.URL, dialOptions) //nolint:bodyclose // websocket.Dial owns and closes its HTTP response body
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

func TestSocketConnCloseInterruptsBlockedWrite(t *testing.T) {
	t.Parallel()

	serverRelease := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			CompressionMode: websocket.CompressionDisabled,
		})
		if err != nil {
			return
		}
		defer ws.CloseNow() //nolint:errcheck // test cleanup closes the peer immediately
		<-serverRelease
	}))
	var releaseOnce sync.Once
	releaseServer := func() { releaseOnce.Do(func() { close(serverRelease) }) }
	defer func() {
		releaseServer()
		server.Close()
	}()

	gated := make(chan *blockingWriteConn, 1)
	httpTransport := &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}
			wrapped := newBlockingWriteConn(conn)
			gated <- wrapped
			return wrapped, nil
		},
	}
	defer httpTransport.CloseIdleConnections()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := dialSocket(ctx, server.URL, websocket.DialOptions{
		HTTPClient:      &http.Client{Transport: httpTransport},
		CompressionMode: websocket.CompressionDisabled,
	}, KeepAlive{Interval: time.Hour, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	networkConn := <-gated
	defer networkConn.Close() //nolint:errcheck // test cleanup after an assertion failure
	networkConn.blockWrites()

	writeDone := make(chan error, 1)
	go func() {
		writeDone <- conn.WriteFrame(protocol.Frame{
			Kind:    protocol.KindControl,
			Payload: []byte(`{"type":"barrier"}`),
		})
	}()
	select {
	case <-networkConn.writeStarted:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("WebSocket write did not reach the blocked network connection")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- conn.Close() }()
	var closeErr error
	select {
	case closeErr = <-closeDone:
	case <-time.After(250 * time.Millisecond):
		_ = networkConn.Close()
		<-closeDone
		<-writeDone
		t.Fatal("socket Close waited for the WebSocket close-frame timeout")
	}
	if err := <-writeDone; err == nil {
		t.Fatal("blocked WebSocket write succeeded after Close")
	}
	if closeErr != nil {
		t.Fatalf("Close error = %v, want nil", closeErr)
	}
	releaseServer()
}

func TestOlderGenerationCannotMoveResumePositionBackward(t *testing.T) {
	t.Parallel()

	zero := uint64(0)
	ref := connectionRef{link: newTestLink(false), generation: 1}
	conn := &reconnectingConn{current: ref, generation: 1, resumes: map[string]resumeState{
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
	if _, _, err := conn.observeFrame(ref, protocol.Frame{Kind: protocol.KindControl, Payload: attached}); err != nil {
		t.Fatal(err)
	}

	sid, err := protocol.NewSessionID("7K3D")
	if err != nil {
		t.Fatal(err)
	}
	frame, drop, err := conn.observeFrame(ref, protocol.Frame{
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

func TestSnapshotAdvancesResumeOnlyAfterSnapshotFrame(t *testing.T) {
	t.Parallel()

	lastSeq := uint64(10)
	ref := connectionRef{link: newTestLink(false), generation: 3}
	conn := &reconnectingConn{
		current:    ref,
		generation: ref.generation,
		resumes: map[string]resumeState{
			"7K3D": {
				attach: protocol.Control{
					Type:      protocol.TypeAttach,
					SessionID: "7K3D",
					LastSeq:   &lastSeq,
				},
				next:     lastSeq,
				haveNext: true,
			},
		},
	}
	attached, err := (protocol.Control{
		Type:      protocol.TypeAttached,
		SessionID: "7K3D",
		Seq:       100,
		Snapshot:  true,
	}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.observeFrame(ref, protocol.Frame{Kind: protocol.KindControl, Payload: attached}); err != nil {
		t.Fatal(err)
	}
	if got := resumeOffset(t, conn); got != lastSeq {
		t.Fatalf("resume offset after snapshot announcement = %d, want %d", got, lastSeq)
	}

	sid, err := protocol.NewSessionID("7K3D")
	if err != nil {
		t.Fatal(err)
	}
	frame, drop, err := conn.observeFrame(ref, protocol.Frame{
		Kind:    protocol.KindSnapshot,
		Session: sid,
		Payload: []byte("rendered bytes do not advance the PTY sequence"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if drop || frame.Kind != protocol.KindSnapshot {
		t.Fatalf("snapshot frame = %+v, drop = %v", frame, drop)
	}
	if got := resumeOffset(t, conn); got != 100 {
		t.Fatalf("resume offset after complete snapshot = %d, want 100", got)
	}
}

func TestStaleGenerationCannotDetachCurrentResume(t *testing.T) {
	t.Parallel()

	lastSeq := uint64(10)
	current := connectionRef{link: newTestLink(false), generation: 2}
	stale := connectionRef{link: newTestLink(false), generation: 1}
	conn := &reconnectingConn{
		current:    current,
		generation: current.generation,
		resumes: map[string]resumeState{
			"7K3D": {
				attach: protocol.Control{
					Type:      protocol.TypeAttach,
					SessionID: "7K3D",
					LastSeq:   &lastSeq,
				},
				next:     lastSeq,
				haveNext: true,
			},
		},
	}
	payload, err := (protocol.Control{
		Type:      protocol.TypeDetach,
		SessionID: "7K3D",
		Reason:    protocol.ReasonStolen,
	}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	frame, drop, err := conn.observeFrame(
		stale,
		protocol.Frame{Kind: protocol.KindControl, Payload: payload},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !drop || frame.Kind != 0 || frame.Session != (protocol.SessionID{}) || frame.Seq != 0 || len(frame.Payload) != 0 {
		t.Fatalf("stale frame = %+v, drop = %v, want dropped", frame, drop)
	}
	if got := resumeOffset(t, conn); got != lastSeq {
		t.Fatalf("resume offset after stale stolen frame = %d, want %d", got, lastSeq)
	}
}

func TestControlWriteLinearizesWithReconnect(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name        string
		initial     map[string]resumeState
		control     protocol.Control
		wantResumes int
	}{
		{
			name: "attach is included",
			control: protocol.Control{
				Type:      protocol.TypeAttach,
				SessionID: "7K3D",
			},
			wantResumes: 1,
		},
		{
			name: "detach is excluded",
			initial: map[string]resumeState{
				"7K3D": {
					attach: protocol.Control{Type: protocol.TypeAttach, SessionID: "7K3D"},
				},
			},
			control: protocol.Control{
				Type:      protocol.TypeDetach,
				SessionID: "7K3D",
				Reason:    protocol.ReasonClient,
			},
			wantResumes: 0,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			old := newTestLink(true)
			next := newTestLink(false)
			ref := connectionRef{link: old, generation: 1}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			conn := &reconnectingConn{
				ctx:        ctx,
				cancel:     cancel,
				dial:       func(context.Context, string, websocket.DialOptions, KeepAlive) (linkConn, error) { return next, nil },
				backoff:    Backoff{Initial: time.Millisecond, Max: time.Millisecond},
				current:    ref,
				generation: ref.generation,
				resumes:    test.initial,
			}
			if conn.resumes == nil {
				conn.resumes = make(map[string]resumeState)
			}
			payload, err := test.control.Encode()
			if err != nil {
				t.Fatal(err)
			}

			writeDone := make(chan error, 1)
			go func() {
				writeDone <- conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload})
			}()
			<-old.writeStarted
			transitionHeld := !conn.reconnectMu.TryLock()
			if !transitionHeld {
				conn.reconnectMu.Unlock()
			}

			reconnectDone := make(chan error, 1)
			go func() {
				_, err := conn.connection(ref)
				reconnectDone <- err
			}()
			old.unblock()
			if err := <-writeDone; err != nil {
				t.Fatal(err)
			}
			if err := <-reconnectDone; err != nil {
				t.Fatal(err)
			}
			if !transitionHeld {
				t.Fatal("reconnect transition was not held across the socket write")
			}
			if got := len(next.snapshot()); got != test.wantResumes {
				t.Fatalf("resume writes after concurrent %s = %d, want %d", test.control.Type, got, test.wantResumes)
			}
		})
	}
}

func TestCloseInterruptsBlockedResumeWrite(t *testing.T) {
	t.Parallel()

	old := newTestLink(false)
	next := newTestLink(true)
	ref := connectionRef{link: old, generation: 1}
	ctx, cancel := context.WithCancel(context.Background())
	conn := &reconnectingConn{
		ctx:        ctx,
		cancel:     cancel,
		dial:       func(context.Context, string, websocket.DialOptions, KeepAlive) (linkConn, error) { return next, nil },
		backoff:    Backoff{Initial: time.Millisecond, Max: time.Millisecond},
		current:    ref,
		generation: ref.generation,
		resumes: map[string]resumeState{
			"7K3D": {attach: protocol.Control{Type: protocol.TypeAttach, SessionID: "7K3D"}},
		},
	}
	reconnectDone := make(chan error, 1)
	go func() {
		_, err := conn.connection(ref)
		reconnectDone <- err
	}()
	<-next.writeStarted

	closeDone := make(chan error, 1)
	go func() { closeDone <- conn.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(250 * time.Millisecond):
		next.unblock()
		t.Fatal("Close did not interrupt the blocked resume write")
	}
	if err := <-reconnectDone; !errors.Is(err, ErrClosed) {
		t.Fatalf("reconnect error = %v, want ErrClosed", err)
	}
}

func resumeOffset(t *testing.T, conn *reconnectingConn) uint64 {
	t.Helper()
	frames, err := conn.resumeFrames()
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 {
		t.Fatalf("resume frames = %d, want 1", len(frames))
	}
	msg, err := protocol.DecodeControl(frames[0].Payload)
	if err != nil {
		t.Fatal(err)
	}
	if msg.LastSeq == nil {
		t.Fatal("resume frame has no exact offset")
	}
	return *msg.LastSeq
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

type testLink struct {
	mu           sync.Mutex
	writes       []protocol.Frame
	blockWrites  bool
	writeStarted chan struct{}
	releaseWrite chan struct{}
	startOnce    sync.Once
	releaseOnce  sync.Once
	stoppedLink  bool
}

type blockingWriteConn struct {
	net.Conn

	mu           sync.Mutex
	blocked      bool
	writeStarted chan struct{}
	releaseWrite chan struct{}
	writeOnce    sync.Once
	closeOnce    sync.Once
	closeErr     error
}

func newBlockingWriteConn(conn net.Conn) *blockingWriteConn {
	return &blockingWriteConn{
		Conn:         conn,
		writeStarted: make(chan struct{}),
		releaseWrite: make(chan struct{}),
	}
}

func (c *blockingWriteConn) blockWrites() {
	c.mu.Lock()
	c.blocked = true
	c.mu.Unlock()
}

func (c *blockingWriteConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	blocked := c.blocked
	c.mu.Unlock()
	if !blocked {
		return c.Conn.Write(p)
	}
	c.writeOnce.Do(func() { close(c.writeStarted) })
	<-c.releaseWrite
	return 0, net.ErrClosed
}

func (c *blockingWriteConn) Close() error {
	c.closeOnce.Do(func() {
		close(c.releaseWrite)
		c.closeErr = c.Conn.Close()
	})
	return c.closeErr
}

func newTestLink(blockWrites bool) *testLink {
	return &testLink{
		blockWrites:  blockWrites,
		writeStarted: make(chan struct{}),
		releaseWrite: make(chan struct{}),
	}
}

func (c *testLink) ReadFrame() (protocol.Frame, error) {
	return protocol.Frame{}, errTestDisconnect
}

func (c *testLink) WriteFrame(frame protocol.Frame) error {
	frame.Payload = bytes.Clone(frame.Payload)
	c.mu.Lock()
	c.writes = append(c.writes, frame)
	c.mu.Unlock()
	if c.blockWrites {
		c.startOnce.Do(func() { close(c.writeStarted) })
		<-c.releaseWrite
	}
	return nil
}

func (c *testLink) Close() error {
	c.mu.Lock()
	c.stoppedLink = true
	c.mu.Unlock()
	c.unblock()
	return nil
}

func (c *testLink) fail(error) {
	_ = c.Close()
}

func (c *testLink) stopped() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stoppedLink
}

func (c *testLink) unblock() {
	c.releaseOnce.Do(func() { close(c.releaseWrite) })
}

func (c *testLink) snapshot() []protocol.Frame {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]protocol.Frame(nil), c.writes...)
}

// A redial against a listener that is gone must surface the dial error and let
// the backoff loop retry. dialLink previously returned dialSocket's typed-nil
// *socketConn as a non-nil linkConn, so connectionLocked's `link != nil` guard
// called Close on a nil receiver and panicked the whole client process.
func TestRedialAgainstClosedListenerRetriesInsteadOfPanicking(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	url := "ws://" + listener.Addr().String() + "/mesh"
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	link, err := dialLink(context.Background(), url, websocket.DialOptions{}, KeepAlive{})
	if err == nil {
		t.Fatal("dialLink succeeded against a closed listener")
	}
	if link != nil {
		t.Fatalf("dialLink returned a non-nil linkConn (%T) alongside an error; a typed nil here panics connectionLocked", link)
	}
}

// Served HTTP services and the control socket share one origin on the tailnet
// listener, so the default same-origin check is satisfied by a file the user
// merely served. A page must never be able to open a Mesh control connection
// and issue session.create; a non-browser client, which sends no Origin at
// all, must be unaffected.
func TestServeRefusesBrowserOriginsOnTheControlSocket(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = ServeWithOptions(w, r, ServeOptions{}, func(context.Context, Conn) error { return nil })
	}))
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")

	// A browser dialling from the served origin is refused.
	refused, response, err := websocket.Dial(context.Background(), url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{server.URL}},
	})
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err == nil {
		_ = refused.CloseNow()
		t.Fatal("a browser origin opened a Mesh control connection")
	}

	// A Mesh client sends no Origin and still connects.
	conn, accepted, err := websocket.Dial(context.Background(), url, &websocket.DialOptions{})
	if accepted != nil && accepted.Body != nil {
		_ = accepted.Body.Close()
	}
	if err != nil {
		t.Fatalf("a non-browser client was refused: %v", err)
	}
	_ = conn.CloseNow()
}
