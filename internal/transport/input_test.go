package transport

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/shaul/mesh/internal/protocol"
)

func TestInputWaitsForHealthyContentionButStopsOnRetirement(t *testing.T) {
	for _, retire := range []bool{false, true} {
		t.Run(map[bool]string{false: "healthy", true: "retired"}[retire], func(t *testing.T) {
			testQueuedInput(t, retire)
		})
	}
}

func testQueuedInput(t *testing.T, retire bool) {
	t.Helper()
	old, next := newTestLink(false), newTestLink(false)
	conn := reconnectTestConnection(t, old, next)
	writeTestControl(t, conn, protocol.Control{Type: protocol.TypeAttach, SessionID: "BBBB"})
	conn.reconnectMu.Lock()
	sid, _ := protocol.NewSessionID("BBBB")
	written := make(chan error, 1)
	go func() {
		written <- conn.WriteFrame(protocol.Frame{Kind: protocol.KindInput, Session: sid, Payload: []byte("typed")})
	}()
	select {
	case err := <-written:
		conn.reconnectMu.Unlock()
		t.Fatalf("input dropped during healthy contention: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if retire {
		conn.retireLocked(conn.current)
		select {
		case err := <-written:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(time.Second):
			t.Error("retired input still waits for connection gate")
		}
		conn.reconnectMu.Unlock()
		if len(old.snapshot()) != 1 {
			t.Fatal("retired input was transmitted")
		}
		return
	}
	conn.reconnectMu.Unlock()
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if got := old.snapshot(); len(got) != 2 || string(got[1].Payload) != "typed" {
		t.Fatalf("healthy input lost: %#v", got)
	}
}

func TestResumeBarrierDropsInputWithoutDeadlock(t *testing.T) {
	old, next := newTestLink(false), newTestLink(false)
	conn := reconnectTestConnection(t, old, next)
	writeTestControl(t, conn, protocol.Control{Type: protocol.TypeAttach, SessionID: "BBBB"})
	barriers := 0
	conn.SetResumeInputBarrier(func() error {
		barriers++
		sid, _ := protocol.NewSessionID("BBBB")
		written := make(chan error, 1)
		go func() {
			written <- conn.WriteFrame(protocol.Frame{Kind: protocol.KindInput, Session: sid, Payload: []byte("pending")})
		}()
		select {
		case err := <-written:
			return err
		case <-time.After(time.Second):
			return errors.New("pre-acknowledgment input blocked the reset barrier")
		}
	})
	if _, err := conn.connection(conn.current); err != nil {
		t.Fatal(err)
	}
	acknowledgeTestResume(t, conn, protocol.Control{Type: protocol.TypeAttached, SessionID: "BBBB"})
	acknowledgeTestResume(t, conn, protocol.Control{Type: protocol.TypeAttached, SessionID: "BBBB"})
	if barriers != 1 || len(next.snapshot()) != 1 {
		t.Fatalf("barriers=%d frames=%#v", barriers, next.snapshot())
	}
	sid, _ := protocol.NewSessionID("BBBB")
	if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindInput, Session: sid, Payload: []byte("fresh")}); err != nil {
		t.Fatal(err)
	}
	if got := next.snapshot(); len(got) != 2 || string(got[1].Payload) != "fresh" {
		t.Fatalf("fresh input lost: %#v", got)
	}
}

func TestResumeBarrierRunsOnceAndEachSessionWaitsForItsAcknowledgment(t *testing.T) {
	old, middle, next := newTestLink(false), newTestLink(false), newTestLink(false)
	conn := reconnectTestConnection(t, old, middle)
	writeTestControl(t, conn, protocol.Control{Type: protocol.TypeAttach, SessionID: "BBBB"})
	writeTestControl(t, conn, protocol.Control{Type: protocol.TypeAttach, SessionID: "CCCC"})
	stale, err := conn.connection(conn.current)
	if err != nil {
		t.Fatal(err)
	}
	conn.dial = func(context.Context, string, websocket.DialOptions, KeepAlive) (linkConn, error) {
		return next, nil
	}
	barriers := 0
	conn.SetResumeInputBarrier(func() error { barriers++; return nil })
	if _, err := conn.connection(stale); err != nil {
		t.Fatal(err)
	}
	ack := protocol.Control{Type: protocol.TypeAttached, SessionID: "BBBB"}
	payload, err := ack.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, drop, err := conn.observeFrame(stale, protocol.Frame{Kind: protocol.KindControl, Payload: payload}); err != nil || !drop {
		t.Fatalf("stale acknowledgment: drop=%v err=%v", drop, err)
	}
	if barriers != 0 {
		t.Fatal("stale acknowledgment reset current input")
	}
	acknowledgeTestResume(t, conn, ack)
	firstID, _ := protocol.NewSessionID("BBBB")
	secondID, _ := protocol.NewSessionID("CCCC")
	first := protocol.Frame{Kind: protocol.KindInput, Session: firstID, Payload: []byte("first session")}
	second := protocol.Frame{Kind: protocol.KindInput, Session: secondID, Payload: []byte("second session")}
	if err := conn.WriteFrame(first); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteFrame(second); err != nil {
		t.Fatal(err)
	}
	writes := next.snapshot()
	if barriers != 1 || len(writes) != 3 {
		t.Fatalf("first session resumed: barriers=%d frames=%#v", barriers, writes)
	}
	assertFrameEqual(t, writes[2], first)
	acknowledgeTestResume(t, conn, protocol.Control{Type: protocol.TypeAttached, SessionID: "CCCC"})
	if err := conn.WriteFrame(second); err != nil {
		t.Fatal(err)
	}
	writes = next.snapshot()
	if barriers != 1 || len(writes) != 4 {
		t.Fatalf("both sessions resumed: barriers=%d frames=%#v", barriers, writes)
	}
	assertFrameEqual(t, writes[3], second)
}

func TestFailedResumeBarrierNeverEnablesInput(t *testing.T) {
	old, next := newTestLink(false), newTestLink(false)
	conn := reconnectTestConnection(t, old, next)
	writeTestControl(t, conn, protocol.Control{Type: protocol.TypeAttach, SessionID: "BBBB"})
	failure := errors.New("cannot reset input")
	conn.SetResumeInputBarrier(func() error { return failure })
	if _, err := conn.connection(conn.current); err != nil {
		t.Fatal(err)
	}
	payload, err := (protocol.Control{Type: protocol.TypeAttached, SessionID: "BBBB"}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = conn.observeFrame(conn.current, protocol.Frame{Kind: protocol.KindControl, Payload: payload})
	if !errors.Is(err, failure) {
		t.Fatalf("resume error=%v", err)
	}
	sid, _ := protocol.NewSessionID("BBBB")
	if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindInput, Session: sid, Payload: []byte("pending")}); err != nil {
		t.Fatal(err)
	}
	if len(next.snapshot()) != 1 || !next.stopped() {
		t.Fatal("failed input reset left the replacement usable")
	}
}

func TestInputDuringReconnectDoesNotWaitForRecovery(t *testing.T) {
	old, next := newTestLink(false), newTestLink(false)
	conn := reconnectTestConnection(t, old, next)
	writeTestControl(t, conn, protocol.Control{Type: protocol.TypeAttach, SessionID: "BBBB"})
	dialing, restore := make(chan struct{}), make(chan struct{})
	conn.dial = func(ctx context.Context, _ string, _ websocket.DialOptions, _ KeepAlive) (linkConn, error) {
		close(dialing)
		select {
		case <-restore:
			return next, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	reconnected := make(chan error, 1)
	go func() { _, err := conn.connection(conn.current); reconnected <- err }()
	<-dialing
	written := make(chan error, 1)
	sid, _ := protocol.NewSessionID("BBBB")
	go func() {
		written <- conn.WriteFrame(protocol.Frame{Kind: protocol.KindInput, Session: sid, Payload: []byte("pending")})
	}()
	select {
	case err := <-written:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("input blocked behind reconnect instead of releasing the terminal reader")
	}
	close(restore)
	if err := <-reconnected; err != nil {
		t.Fatal(err)
	}
	if got := next.snapshot(); len(got) != 1 || got[0].Kind != protocol.KindControl {
		t.Fatalf("input reached replacement connection: %#v", got)
	}
}
