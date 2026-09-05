package transport

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/shaul/mesh/internal/protocol"
)

func TestReconnectDiscardsInputUntilTheSessionResumes(t *testing.T) {
	t.Parallel()
	old, next := newTestLink(false), newTestLink(false)
	conn := reconnectTestConnection(t, old, next)
	attach := protocol.Control{Type: protocol.TypeAttach, SessionID: "BBBB"}
	writeTestControl(t, conn, attach)
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	sid, _ := protocol.NewSessionID("BBBB")
	input := protocol.Frame{Kind: protocol.KindInput, Session: sid, Payload: []byte("old command\n")}
	if err := conn.WriteFrame(input); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.connection(conn.current); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteFrame(input); err != nil {
		t.Fatal(err)
	}
	if got := next.snapshot(); len(got) != 1 || got[0].Kind != protocol.KindControl {
		t.Fatalf("input crossed reconnect before acknowledgment: %#v", got)
	}
	acknowledgeTestResume(t, conn, protocol.Control{Type: protocol.TypeAttached, SessionID: "BBBB"})
	input.Payload = []byte("new command\n")
	if err := conn.WriteFrame(input); err != nil {
		t.Fatal(err)
	}
	writes := next.snapshot()
	if len(writes) != 2 {
		t.Fatalf("frames after resumed input = %d, want 2", len(writes))
	}
	assertFrameEqual(t, writes[1], input)
}

func TestInterruptedResumeDiscardsInputAndDoesNotAttachAgain(t *testing.T) {
	t.Parallel()
	old, next := newTestLink(false), newTestLink(false)
	conn := reconnectTestConnection(t, old, next)
	writeTestControl(t, conn, protocol.Control{Type: protocol.TypeAttach, SessionID: "BBBB"})
	if _, err := conn.connection(conn.current); err != nil {
		t.Fatal(err)
	}
	acknowledgeTestResume(t, conn, protocol.Control{
		Type: protocol.TypeError, SessionID: "BBBB", Message: "session BBBB is interrupted",
	})
	sid, _ := protocol.NewSessionID("BBBB")
	if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindInput, Session: sid, Payload: []byte("stale\n")}); err != nil {
		t.Fatal(err)
	}
	if len(next.snapshot()) != 1 {
		t.Fatal("input was written after the resumed session was refused")
	}
	frames, err := conn.resumeFrames()
	if err != nil || len(frames) != 0 {
		t.Fatalf("refused session resume frames = %#v, %v", frames, err)
	}
}

func TestReconnectRecoveryWaitsAfterAnUnavailableWitnessAndSkipsCatalogConnections(t *testing.T) {
	t.Parallel()
	for _, attached := range []bool{false, true} {
		t.Run(map[bool]string{false: "catalog", true: "session"}[attached], func(t *testing.T) {
			testReconnectRecovery(t, attached)
		})
	}
}

func TestReconnectRetriesUnavailableWitnessAtBoundedCadence(t *testing.T) {
	t.Parallel()
	old, next := newTestLink(false), newTestLink(false)
	conn := reconnectTestConnection(t, old, next)
	writeTestControl(t, conn, protocol.Control{Type: protocol.TypeAttach, SessionID: "BBBB"})
	base := time.Unix(1_700_000_000, 0)
	now := base
	conn.recoveryNow = func() time.Time { return now }
	attemptTimes := []time.Duration{0, 0, 0, 29 * time.Second, 30 * time.Second, 59 * time.Second, 60 * time.Second, 90 * time.Second, 90 * time.Second}
	attempts := 0
	conn.dial = func(context.Context, string, websocket.DialOptions, KeepAlive) (linkConn, error) {
		now = base.Add(attemptTimes[attempts])
		attempts++
		if attempts == len(attemptTimes) {
			return next, nil
		}
		return nil, errors.New("target remains offline")
	}
	var recoveries []int
	conn.recover = func(context.Context) error {
		recoveries = append(recoveries, attempts)
		if len(recoveries) < 3 {
			return errors.New("witness unavailable")
		}
		return nil
	}
	if _, err := conn.connection(conn.current); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(recoveries, []int{3, 5, 7}) {
		t.Fatalf("recovery attempts = %v, want unavailable retries at 30-second intervals and none after success", recoveries)
	}
}

func TestReconnectRetryWaitStartsAfterWitnessReturns(t *testing.T) {
	t.Parallel()
	old, next := newTestLink(false), newTestLink(false)
	conn := reconnectTestConnection(t, old, next)
	writeTestControl(t, conn, protocol.Control{Type: protocol.TypeAttach, SessionID: "BBBB"})
	base := time.Unix(1_700_000_000, 0)
	now := base
	conn.recoveryNow = func() time.Time { return now }
	attemptTimes := []time.Duration{0, 0, 0, 119 * time.Second, 120 * time.Second, 120 * time.Second}
	attempts := 0
	conn.dial = func(context.Context, string, websocket.DialOptions, KeepAlive) (linkConn, error) {
		now = base.Add(attemptTimes[attempts])
		attempts++
		if attempts == len(attemptTimes) {
			return next, nil
		}
		return nil, errors.New("target remains offline")
	}
	var recoveries []int
	conn.recover = func(context.Context) error {
		recoveries = append(recoveries, attempts)
		if len(recoveries) == 1 {
			now = now.Add(90 * time.Second)
			return context.DeadlineExceeded
		}
		return nil
	}
	if _, err := conn.connection(conn.current); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(recoveries, []int{3, 5}) {
		t.Fatalf("recovery attempts = %v, want 30 seconds after the slow witness returned", recoveries)
	}
}

func TestReconnectCloseCancelsWitnessRecovery(t *testing.T) {
	t.Parallel()
	old, next := newTestLink(false), newTestLink(false)
	conn := reconnectTestConnection(t, old, next)
	writeTestControl(t, conn, protocol.Control{Type: protocol.TypeAttach, SessionID: "BBBB"})
	conn.dial = func(context.Context, string, websocket.DialOptions, KeepAlive) (linkConn, error) {
		return nil, errors.New("target offline")
	}
	started := make(chan struct{})
	stopped := make(chan struct{})
	conn.recover = func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		close(stopped)
		return ctx.Err()
	}
	done := make(chan error, 1)
	go func() {
		_, err := conn.connection(conn.current)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("witness recovery did not start")
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("Close returned before witness recovery stopped")
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("reconnect = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("reconnect did not finish after Close")
	}
}

func testReconnectRecovery(t *testing.T, attached bool) {
	t.Helper()
	old, next := newTestLink(false), newTestLink(false)
	conn := reconnectTestConnection(t, old, next)
	if attached {
		writeTestControl(t, conn, protocol.Control{Type: protocol.TypeAttach, SessionID: "BBBB"})
	}
	attempts, recoveries := 0, 0
	conn.dial = func(context.Context, string, websocket.DialOptions, KeepAlive) (linkConn, error) {
		attempts++
		if attempts <= 5 {
			return nil, errors.New("offline")
		}
		return next, nil
	}
	conn.recover = func(context.Context) error {
		recoveries++
		if attempts != 3 {
			t.Errorf("recovery started after %d attempts, want 3", attempts)
		}
		return errors.New("witness unavailable")
	}
	if _, err := conn.connection(conn.current); err != nil {
		t.Fatal(err)
	}
	want := 0
	if attached {
		want = 1
	}
	if recoveries != want || attempts != 6 {
		t.Fatalf("recoveries=%d attempts=%d, want %d and 6", recoveries, attempts, want)
	}
}

func reconnectTestConnection(t *testing.T, old, next *testLink) *reconnectingConn {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	conn := &reconnectingConn{
		ctx: ctx, cancel: cancel, current: connectionRef{link: old, generation: 1}, generation: 1,
		backoff: Backoff{Initial: time.Millisecond, Max: time.Millisecond},
		resumes: make(map[string]resumeState),
		dial:    func(context.Context, string, websocket.DialOptions, KeepAlive) (linkConn, error) { return next, nil },
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func writeTestControl(t *testing.T, conn *reconnectingConn, control protocol.Control) {
	t.Helper()
	payload, err := control.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload}); err != nil {
		t.Fatal(err)
	}
}

func acknowledgeTestResume(t *testing.T, conn *reconnectingConn, control protocol.Control) {
	t.Helper()
	payload, err := control.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, drop, err := conn.observeFrame(conn.current, protocol.Frame{Kind: protocol.KindControl, Payload: payload}); err != nil || drop {
		t.Fatalf("resume response: drop=%v err=%v", drop, err)
	}
}
