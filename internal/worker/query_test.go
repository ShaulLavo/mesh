package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session"
	terminalstate "github.com/shaul/mesh/internal/terminal"
)

// These completed-query observations cover the worker, not a real terminal's
// response to replay. Partial parser tails remain a separate capture contract.
func TestDetachedQueriesGetNoReplyAndSnapshotOmitsThem(t *testing.T) {
	w, pty := newQueryWorker(t)
	queryEmit(t, pty, []byte("before queries\r\n"))
	start, _ := queryCaptureAfter(t, w, "before queries", uint64(len("before queries\r\n")))

	client, reader, stop := queryAttach(t, w, nil)
	queryReadAttached(t, reader, w.sid, start, true)
	queryReadFrame(t, reader) // Drain the arming client's snapshot before input.
	knownInput := []byte("p013-observer-armed")
	if err := protocol.NewWriter(client).WriteInput(w.sid, knownInput); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return bytes.Equal(pty.inputs(), knownInput) })
	stop()
	pty.resetInputs()
	t.Logf("same PTY input relay recorded %q; arming client and writers joined before reset", knownInput)

	queries := []string{
		"\x1b[6n", "\x1b[c", "\x1b[>c", "\x1b[?2004$p",
		"\x1b[>q", "\x1b]10;?\x07", "\x1b]11;?\x1b\\",
	}
	marker := "P013_QUERIES_DONE_7f24"
	output := []byte(strings.Join(queries, "") + "\r\n" + marker + "\r\n")
	queryEmit(t, pty, output)
	head, capture := queryCaptureAfter(t, w, marker, start+uint64(len(output)))
	if err := pty.noInput(); err != nil {
		t.Fatal(err)
	}
	t.Logf("detached interval [%d,%d): screen marker consumed, Restorable=%t, PTY input bytes=0", start, head, capture.Restorable)

	_, fresh, stopFresh := queryAttach(t, w, nil)
	queryReadAttached(t, fresh, w.sid, head, true)
	frame := queryReadFrame(t, fresh)
	if err := querySnapshotError(frame, w.sid, capture, marker); err != nil {
		t.Fatal(err)
	}
	for _, query := range queries {
		if bytes.Contains(frame.Payload, []byte(query)) {
			t.Fatalf("fresh snapshot contains completed query %q", query)
		}
	}
	stopFresh()

	_, resumed, stopResumed := queryAttach(t, w, &start)
	queryReadAttached(t, resumed, w.sid, start, false)
	if err := queryReplayError(queryReadFrame(t, resumed), w.sid, start, output); err != nil {
		t.Fatal(err)
	}
	stopResumed()
	if err := pty.noInput(); err != nil {
		t.Fatal(err)
	}
	t.Logf("fresh capture accepted with marker and no completed queries; replay preserved all %d bytes at offset %d", len(output), start)

	t.Run("injected-input-is-detected", func(t *testing.T) {
		if _, err := w.pty.Write([]byte("test-only-injected-reply")); err != nil {
			t.Fatal(err)
		}
		if err := pty.noInput(); err == nil {
			t.Fatal("zero-input check accepted an injected PTY write")
		} else {
			t.Logf("zero-input control rejected actual injected write: %v", err)
		}
	})
}

type queryPTY struct {
	*pipePTY
	inputMu sync.Mutex
	input   []byte
}

func (p *queryPTY) Write(data []byte) (int, error) {
	p.inputMu.Lock()
	defer p.inputMu.Unlock()
	p.input = append(p.input, data...)
	return len(data), nil
}

func (p *queryPTY) inputs() []byte {
	p.inputMu.Lock()
	defer p.inputMu.Unlock()
	return bytes.Clone(p.input)
}

func (p *queryPTY) resetInputs() {
	p.inputMu.Lock()
	defer p.inputMu.Unlock()
	p.input = nil
}

func (p *queryPTY) noInput() error {
	if input := p.inputs(); len(input) != 0 {
		return fmt.Errorf("PTY received %d input bytes: %q", len(input), input)
	}
	return nil
}

func newQueryWorker(t *testing.T) (*Worker, *queryPTY) {
	t.Helper()
	return newQueryWorkerScreen(t, terminalstate.NewScreen(80, 6))
}

func newQueryWorkerScreen(t *testing.T, screen terminalstate.Screen) (*Worker, *queryPTY) {
	t.Helper()
	sid, err := protocol.NewSessionID("QUERY")
	if err != nil {
		t.Fatal(err)
	}
	pty := &queryPTY{pipePTY: newPipePTY()}
	w := &Worker{
		cfg: Config{ID: sid.String(), Dir: t.TempDir()}, sid: sid, pty: pty,
		ring: session.NewRing(ringSize), screen: screen,
		exited: make(chan struct{}), attached: make(chan struct{}), pumpDone: make(chan struct{}),
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if _, err := queryStopWorker(ctx, w, pty); err != nil {
			t.Error(err)
		}
	})
	go w.pump()
	return w, pty
}

func queryEmit(t *testing.T, pty *queryPTY, output []byte) {
	t.Helper()
	done := make(chan struct{})
	var writeErr error
	t.Cleanup(func() {
		_ = pty.Close()
		queryJoin(t, done, "PTY output writer")
	})
	go func() {
		defer close(done)
		n, err := pty.emit(output)
		writeErr = err
		if err == nil && n != len(output) {
			writeErr = io.ErrShortWrite
		}
	}()
	queryJoin(t, done, "PTY output writer")
	if writeErr != nil {
		t.Fatal(writeErr)
	}
}

func queryCaptureAfter(t *testing.T, w *Worker, marker string, head uint64) (uint64, terminalstate.Capture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	capture, err := queryWaitCapture(ctx, w, marker, head)
	if err != nil {
		t.Fatal(err)
	}
	if !capture.Restorable {
		t.Fatal("completed queries did not leave an accepted Restorable capture")
	}
	return head, capture
}

func queryWaitCapture(ctx context.Context, w *Worker, marker string, head uint64) (terminalstate.Capture, error) {
	for {
		if err := ctx.Err(); err != nil {
			return terminalstate.Capture{}, fmt.Errorf("query capture: %w", err)
		}
		if capture, ready := queryTryCapture(w, marker, head); ready {
			return capture, nil
		}
		select {
		case <-ctx.Done():
			return terminalstate.Capture{}, fmt.Errorf("query capture: %w", ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
}

func queryTryCapture(w *Worker, marker string, head uint64) (terminalstate.Capture, bool) {
	if !w.mu.TryLock() {
		return terminalstate.Capture{}, false
	}
	defer w.mu.Unlock()
	if w.client != nil || w.ring.Head() != head {
		return terminalstate.Capture{}, false
	}
	if !strings.Contains(strings.Join(w.screen.Preview(80, 6).Lines, "\n"), marker) {
		return terminalstate.Capture{}, false
	}
	return w.screen.Snapshot(), true
}

func queryStopWorker(ctx context.Context, w *Worker, pty *queryPTY) (<-chan struct{}, error) {
	_ = pty.Close()
	if err := queryAwait(ctx, w.pumpDone, "pump"); err != nil {
		return nil, err
	}
	inputDone := make(chan struct{})
	go func() {
		w.closeInput()
		close(inputDone)
	}()
	return inputDone, queryAwait(ctx, inputDone, "input relay")
}

func queryAwait(ctx context.Context, done <-chan struct{}, name string) error {
	select {
	case <-done:
		return nil
	default:
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%s did not join: %w", name, ctx.Err())
	}
}

func queryAttach(t *testing.T, w *Worker, seq *uint64) (net.Conn, *protocol.Reader, func()) {
	t.Helper()
	client, server := net.Pipe()
	done := make(chan struct{})
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = client.Close()
			_ = server.Close()
			queryJoin(t, done, "serve and attachment writer")
		})
	}
	t.Cleanup(stop)
	go func() {
		w.serve(server)
		w.writers.Wait()
		close(done)
	}()
	if err := client.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := protocol.NewWriter(client).WriteControlMsg(protocol.Control{
		Type: protocol.TypeAttach, SessionID: w.sid.String(), LastSeq: seq,
	}); err != nil {
		t.Fatal(err)
	}
	return client, protocol.NewReader(client), stop
}

func queryJoin(t *testing.T, done <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("%s did not join", name)
	}
}

func queryReadFrame(t *testing.T, reader *protocol.Reader) protocol.Frame {
	t.Helper()
	frame, err := reader.ReadFrame()
	if err != nil {
		t.Fatalf("expected complete query-test frame: %v", err)
	}
	return frame
}

func queryReadAttached(t *testing.T, reader *protocol.Reader, sid protocol.SessionID, seq uint64, snapshot bool) {
	t.Helper()
	if err := queryAttachedError(queryReadFrame(t, reader), sid, seq, snapshot); err != nil {
		t.Fatal(err)
	}
}

func queryAttachedError(frame protocol.Frame, sid protocol.SessionID, seq uint64, snapshot bool) error {
	msg, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return fmt.Errorf("decode attached readiness: %w", err)
	}
	if frame.Kind != protocol.KindControl || msg.Type != protocol.TypeAttached || msg.SessionID != sid.String() || msg.Seq != seq || msg.Snapshot != snapshot {
		return fmt.Errorf("missing expected attached readiness: frame=%+v, control=%+v", frame, msg)
	}
	return nil
}

func querySnapshotError(frame protocol.Frame, sid protocol.SessionID, capture terminalstate.Capture, marker string) error {
	if !capture.Restorable || frame.Kind != protocol.KindSnapshot || frame.Session != sid || !bytes.Equal(frame.Payload, capture.Bytes) || !bytes.Contains(frame.Payload, []byte(marker)) {
		return fmt.Errorf("missing accepted fresh snapshot with marker %q: frame=%+v, Restorable=%t", marker, frame, capture.Restorable)
	}
	return nil
}

func queryReplayError(frame protocol.Frame, sid protocol.SessionID, seq uint64, output []byte) error {
	if frame.Kind != protocol.KindData || frame.Session != sid || frame.Seq != seq || !bytes.Equal(frame.Payload, output) {
		return fmt.Errorf("replay is not exact at offset %d: frame=%+v, want=%q", seq, frame, output)
	}
	return nil
}

func TestQueryCapturePositiveControls(t *testing.T) {
	sid := protocol.SessionID{'Q'}
	marker := "query-control-marker"
	capture := terminalstate.Capture{Bytes: []byte(marker), Restorable: true}
	good := protocol.Frame{Kind: protocol.KindSnapshot, Session: sid, Payload: capture.Bytes}
	if err := querySnapshotError(good, sid, capture, marker); err != nil {
		t.Fatal(err)
	}
	controls := map[string]protocol.Frame{
		"missing snapshot": {},
		"empty snapshot":   {Kind: protocol.KindSnapshot, Session: sid},
		"wrong framing":    {Kind: protocol.KindData, Session: sid, Payload: capture.Bytes},
		"wrong session":    {Kind: protocol.KindSnapshot, Payload: capture.Bytes},
	}
	for name, frame := range controls {
		t.Run(name, func(t *testing.T) {
			queryRequireRejected(t, querySnapshotError(frame, sid, capture, marker))
		})
	}
	queryRequireRejected(t, querySnapshotError(good, sid, capture, "absent-completion-marker"))
	if err := querySnapshotError(good, sid, terminalstate.Capture{Bytes: capture.Bytes}, marker); err == nil {
		t.Fatal("capture check accepted nonrestorable state")
	}
	if err := queryAttachedError(protocol.Frame{}, sid, 9, false); err == nil {
		t.Fatal("readiness check accepted absent attached frame")
	}
	queryReplayControls(t, sid, []byte(marker))
}

func queryReplayControls(t *testing.T, sid protocol.SessionID, output []byte) {
	t.Helper()
	good := protocol.Frame{Kind: protocol.KindData, Session: sid, Seq: 9, Payload: output}
	if err := queryReplayError(good, sid, 9, output); err != nil {
		t.Fatal(err)
	}
	controls := map[string]protocol.Frame{
		"missing replay":       {},
		"dropped replay bytes": {Kind: protocol.KindData, Session: sid, Seq: 9},
		"wrong replay offset":  {Kind: protocol.KindData, Session: sid, Seq: 10, Payload: output},
		"wrong replay framing": {Kind: protocol.KindSnapshot, Session: sid, Payload: output},
	}
	for name, frame := range controls {
		t.Run(name, func(t *testing.T) {
			queryRequireRejected(t, queryReplayError(frame, sid, 9, output))
		})
	}
}

func queryRequireRejected(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("positive check accepted broken behavior")
	}
	t.Logf("positive check rejected broken behavior: %v", err)
}

// The held operations are released even when a timeout assertion fails, so
// testing the failure paths does not leave a pump or relay behind.
func TestQueryCaptureCancellationWhileRendererBlocked(t *testing.T) {
	w, pty, release, started := queryBlockedWorker(t)
	queryEmit(t, pty, []byte("blocked-renderer"))
	queryJoin(t, started, "blocked renderer entry")
	queryRequireCancelled(t, release, func(ctx context.Context) (<-chan struct{}, error) {
		_, err := queryWaitCapture(ctx, w, "blocked-renderer", uint64(len("blocked-renderer")))
		return nil, err
	})
}

func TestQueryCleanupCancellationWhileRendererBlocked(t *testing.T) {
	w, pty, release, started := queryBlockedWorker(t)
	queryEmit(t, pty, []byte("blocked-renderer"))
	queryJoin(t, started, "blocked renderer entry")
	queryRequireCancelled(t, release, func(ctx context.Context) (<-chan struct{}, error) {
		return queryStopWorker(ctx, w, pty)
	})
}

func TestQueryCleanupCancellationWhileRelayBlocked(t *testing.T) {
	w, pty := newQueryWorker(t)
	relay := &inputRelay{done: make(chan struct{})}
	relay.wg.Add(1)
	w.mu.Lock()
	w.input = relay
	w.mu.Unlock()
	var once sync.Once
	release := func() { once.Do(relay.wg.Done) }
	t.Cleanup(release)
	_ = pty.Close()
	queryJoin(t, w.pumpDone, "pump before relay control")
	queryRequireCancelled(t, release, func(ctx context.Context) (<-chan struct{}, error) {
		return queryStopWorker(ctx, w, pty)
	})
}

type queryHeldScreen struct {
	terminalstate.Screen
	started chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (s *queryHeldScreen) Write(data []byte) (int, error) {
	s.once.Do(func() { close(s.started) })
	<-s.release
	n, err := s.Screen.Write(data)
	if err != nil {
		return n, fmt.Errorf("write released query screen: %w", err)
	}
	return n, nil
}

func queryBlockedWorker(t *testing.T) (*Worker, *queryPTY, func(), <-chan struct{}) {
	t.Helper()
	gate := make(chan struct{})
	screen := &queryHeldScreen{Screen: terminalstate.NewScreen(80, 6), started: make(chan struct{}), release: gate}
	w, pty := newQueryWorkerScreen(t, screen)
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	return w, pty, release, screen.started
}

type queryOperationResult struct {
	joined <-chan struct{}
	err    error
}

func queryRequireCancelled(t *testing.T, release func(), operation func(context.Context) (<-chan struct{}, error)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := make(chan queryOperationResult, 1)
	go func() {
		joined, err := operation(ctx)
		result <- queryOperationResult{joined: joined, err: err}
	}()
	outcome := queryCancelledResult(t, result, release)
	release()
	if outcome.joined != nil {
		queryJoin(t, outcome.joined, "released input relay closer")
	}
	if !errors.Is(outcome.err, context.Canceled) {
		t.Errorf("blocked operation returned %v, want cancellation", outcome.err)
	}
}

func queryCancelledResult(t *testing.T, result <-chan queryOperationResult, release func()) queryOperationResult {
	t.Helper()
	select {
	case outcome := <-result:
		return outcome
	case <-time.After(2 * time.Second):
		t.Error("blocked operation bypassed its canceled context")
	}
	release()
	select {
	case outcome := <-result:
		return outcome
	case <-time.After(2 * time.Second):
		t.Fatal("released control operation did not join")
		return queryOperationResult{}
	}
}
