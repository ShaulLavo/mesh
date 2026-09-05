package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/terminal"
	"github.com/shaul/mesh/internal/transport"
)

func TestReconnectDiscardsReadAndKernelBufferedInputBeforeAcceptingFreshInput(t *testing.T) {
	for _, test := range []struct {
		name          string
		buffered, tty bool
	}{
		{name: "idle pipe reader"},
		{name: "buffered pipe input", buffered: true},
		{name: "idle terminal reader", tty: true},
		{name: "buffered terminal input", buffered: true, tty: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			testReconnectInput(t, test.buffered, test.tty)
		})
	}
}

func testReconnectInput(t *testing.T, buffered, tty bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server := newInputReconnectServer(t, ctx)
	conn, err := transport.Dial(ctx, server.URL, transport.DialOptions{})
	if err != nil {
		t.Fatal(err)
	}
	observed := &observedInputConn{Conn: conn, initial: make(chan struct{}), resumed: make(chan struct{})}
	input, writer, restore := reconnectInputPair(t, tty)
	defer input.Close()  //nolint:errcheck // test cleanup
	defer writer.Close() //nolint:errcheck // test cleanup
	defer restore()
	local, err := localAttachInput(input, tty)
	if err != nil {
		t.Fatal(err)
	}
	defer local.close() //nolint:errcheck // test cleanup
	held := &heldAttachmentInput{ctx: ctx, source: local.reader, read: make(chan struct{}), release: make(chan struct{})}
	terminalInput := local.reader
	if buffered {
		terminalInput = held
	}
	resetStarted := make(chan struct{})
	var resetOnce sync.Once
	done := make(chan error, 1)
	go func() {
		_, err := AttachWithTerminal(ctx, AttachOptions{SessionID: "BBBB", Conn: observed, Raw: true}, AttachTerminal{
			Input: terminalInput, Output: io.Discard, Size: terminal.Size{Cols: 80, Rows: 24},
			CancelInput: func() { resetOnce.Do(func() { close(resetStarted) }); local.cancel() },
			ResetInput:  local.reset,
		})
		done <- err
	}()
	waitInputEvent(t, ctx, observed.initial)
	close(server.cut)
	waitInputEvent(t, ctx, server.reconnecting)
	if buffered {
		writeAttachmentInput(t, writer, "already read during outage")
		waitInputEvent(t, ctx, held.read)
		writeAttachmentInput(t, writer, "still buffered during outage\n")
	}
	close(server.restore)
	waitInputEvent(t, ctx, resetStarted)
	close(held.release)
	waitInputEvent(t, ctx, observed.resumed)
	writeAttachmentInput(t, writer, "fresh\n")
	select {
	case frame := <-server.input:
		if frame.Kind != protocol.KindInput || string(frame.Payload) != "fresh\n" {
			t.Fatalf("input after recovery = kind %v, payload %q", frame.Kind, frame.Payload)
		}
	case <-ctx.Done():
		t.Fatal("first fresh input was lost after recovery")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("attachment did not join its input relay")
	}
}

func reconnectInputPair(t *testing.T, tty bool) (*os.File, *os.File, func()) {
	t.Helper()
	if !tty {
		input, writer, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		return input, writer, func() {}
	}
	writer, input, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	restore, err := makeRaw(input)
	if err != nil {
		_ = input.Close()
		_ = writer.Close()
		t.Fatal(err)
	}
	return input, writer, restore
}

type heldAttachmentInput struct {
	ctx     context.Context
	source  io.Reader
	read    chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *heldAttachmentInput) Read(buf []byte) (int, error) {
	n, err := r.source.Read(buf)
	if n != 0 {
		r.once.Do(r.waitRelease)
	}
	return n, err
}

func (r *heldAttachmentInput) waitRelease() {
	close(r.read)
	select {
	case <-r.release:
	case <-r.ctx.Done():
	}
}

type observedInputConn struct {
	transport.Conn
	initial chan struct{}
	resumed chan struct{}
	acks    int
}

func (c *observedInputConn) SetResumeInputBarrier(barrier func() error) {
	if conn, ok := c.Conn.(interface{ SetResumeInputBarrier(func() error) }); ok {
		conn.SetResumeInputBarrier(barrier)
	}
}

func (c *observedInputConn) ReadFrame() (protocol.Frame, error) {
	frame, err := c.Conn.ReadFrame()
	if err != nil || frame.Kind != protocol.KindControl {
		return frame, err
	}
	msg, _ := protocol.DecodeControl(frame.Payload)
	if msg.Type != protocol.TypeAttached {
		return frame, err
	}
	c.acks++
	if c.acks == 1 {
		close(c.initial)
		return frame, err
	}
	close(c.resumed)
	return frame, err
}

type inputReconnectServer struct {
	*httptest.Server
	ctx          context.Context
	cut          chan struct{}
	reconnecting chan struct{}
	restore      chan struct{}
	input        chan protocol.Frame
	connections  atomic.Int32
}

func newInputReconnectServer(t *testing.T, ctx context.Context) *inputReconnectServer {
	t.Helper()
	server := &inputReconnectServer{ctx: ctx, cut: make(chan struct{}), reconnecting: make(chan struct{}), restore: make(chan struct{}), input: make(chan protocol.Frame, 1)}
	server.Server = httptest.NewServer(http.HandlerFunc(server.serveHTTP))
	t.Cleanup(server.Close)
	return server
}

func (s *inputReconnectServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	generation := s.connections.Add(1)
	if generation == 2 {
		close(s.reconnecting)
		select {
		case <-s.restore:
		case <-s.ctx.Done():
			return
		}
	}
	_ = transport.Serve(w, r, func(_ context.Context, conn transport.Conn) error { return s.serve(conn, generation) })
}

func (s *inputReconnectServer) serve(conn transport.Conn, generation int32) error {
	if _, err := conn.ReadFrame(); err != nil {
		return err
	}
	if err := conn.WriteFrame(controlFrame(protocol.Control{Type: protocol.TypeAttached, SessionID: "BBBB"})); err != nil {
		return err
	}
	if generation == 1 {
		select {
		case <-s.cut:
		case <-s.ctx.Done():
		}
		return nil
	}
	frame, err := conn.ReadFrame()
	if err != nil {
		return err
	}
	s.input <- frame
	return conn.WriteFrame(controlFrame(protocol.Control{Type: protocol.TypeDetach, SessionID: "BBBB"}))
}

func waitInputEvent(t *testing.T, ctx context.Context, event <-chan struct{}) {
	t.Helper()
	select {
	case <-event:
	case <-ctx.Done():
		t.Fatal("timed out waiting for attachment input event")
	}
}

func writeAttachmentInput(t *testing.T, writer io.Writer, input string) {
	t.Helper()
	if _, err := io.WriteString(writer, input); err != nil {
		t.Fatal(err)
	}
}
