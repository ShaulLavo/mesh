package daemon

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/worker"
)

// lateInputWorkerConn holds the relay's reader after the attach handshake, so a
// test can make the worker exit and close its socket while the relay still has
// the worker's final frames unread.
type lateInputWorkerConn struct {
	transport.Conn
	reads     atomic.Int32
	release   chan struct{}
	attempted chan struct{}
	once      sync.Once
}

func (c *lateInputWorkerConn) ReadFrame() (protocol.Frame, error) {
	if c.reads.Add(1) > 1 {
		<-c.release
	}
	return c.Conn.ReadFrame() //nolint:wrapcheck // fixture passes the worker's own error through
}

func (c *lateInputWorkerConn) WriteFrame(frame protocol.Frame) error {
	err := c.Conn.WriteFrame(frame)
	c.once.Do(func() { close(c.attempted) })
	return err //nolint:wrapcheck // fixture passes the worker's own error through
}

// A resize that reaches a worker which already exited fails to write. That
// failure must not discard the exit frame the worker queued before it closed.
func TestClientRelayDeliversExitAfterLateResizeToExitedWorker(t *testing.T) {
	root := compactSocketTempDir(t)
	dir := filepath.Join(root, "s")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stop := filepath.Join(root, "stop")
	type result struct {
		code int
		err  error
	}
	workerDone := make(chan result, 1)
	go func() {
		code, err := worker.Run(worker.Config{
			ID: "7K3D", Dir: dir, Cwd: root, Cols: 80, Rows: 24,
			Env:     []string{"PATH=/usr/bin:/bin", "TERM=xterm-256color", "LANG=C"},
			Command: []string{"/bin/sh", "-c", `while [ ! -e "$1" ]; do sleep 0.01; done; exit 23`, "late-resize", stop},
		})
		workerDone <- result{code, err}
	}()
	waitForPath(t, paths.Socket(dir))
	socket, err := net.Dial("unix", paths.Socket(dir))
	if err != nil {
		t.Fatal(err)
	}
	inner, err := transport.NewStreamConn(socket)
	if err != nil {
		_ = socket.Close()
		t.Fatal(err)
	}
	gate := &lateInputWorkerConn{Conn: inner, release: make(chan struct{}), attempted: make(chan struct{})}
	client := newRelayTestConn()
	relay := newClientRelay(client, newRelayTestConnector(connectResult{conn: gate}), defaultWorkerOperationTimeout)
	var releaseOnce sync.Once
	releaseReader := func() { releaseOnce.Do(func() { close(gate.release) }) }
	t.Cleanup(func() {
		releaseReader()
		_ = relay.Close()
		_ = os.WriteFile(stop, nil, 0o600)
	})
	if _, err := relay.HandleFrame(context.Background(), controlFrame(t, protocol.Control{Type: protocol.TypeAttach, SessionID: "7K3D"})); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(stop, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case done := <-workerDone:
		if done.err != nil || done.code != 23 {
			t.Fatalf("worker finished with code %d, error %v; want 23", done.code, done.err)
		}
	case <-time.After(sessionInspectorE2ETimeout):
		t.Fatal("worker did not exit")
	}

	resize := controlFrame(t, protocol.Control{Type: protocol.TypeResize, SessionID: "7K3D", Cols: 100, Rows: 30})
	if _, err := relay.HandleFrame(context.Background(), resize); err != nil {
		t.Fatalf("resize after worker exit: %v", err)
	}
	select {
	case <-gate.attempted:
	case <-time.After(sessionInspectorE2ETimeout):
		t.Fatal("relay never tried to forward the resize")
	}
	// The failed write has settled by now; a second late frame is dropped, not an error.
	if _, err := relay.HandleFrame(context.Background(), resize); err != nil {
		t.Fatalf("second resize after worker exit: %v", err)
	}

	releaseReader()
	for {
		select {
		case frame := <-client.writes:
			if frame.Kind != protocol.KindControl {
				continue
			}
			message, err := protocol.DecodeControl(frame.Payload)
			if err != nil {
				t.Fatal(err)
			}
			if message.Type == protocol.TypeExit {
				if message.ExitCode == nil || *message.ExitCode != 23 {
					t.Fatalf("exit frame = %+v, want status 23", message)
				}
				return
			}
			if message.Type == protocol.TypeError {
				t.Fatalf("client got %q instead of the exit frame", message.Message)
			}
		case <-time.After(sessionInspectorE2ETimeout):
			t.Fatal("exit frame never reached the client")
		}
	}
}

// When the worker's stream ends without a final frame after input stopped
// flowing, the client is told the session was interrupted instead of finding
// out from its next request.
func TestClientRelayReportsWorkerEndingWithoutFinalFrame(t *testing.T) {
	root := compactSocketTempDir(t)
	socketPath := filepath.Join(root, "w.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go serveAttachThenHangUp(listener)
	socket, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	inner, err := transport.NewStreamConn(socket)
	if err != nil {
		t.Fatal(err)
	}
	gate := &lateInputWorkerConn{Conn: inner, release: make(chan struct{}), attempted: make(chan struct{})}
	client := newRelayTestConn()
	relay := newClientRelay(client, newRelayTestConnector(connectResult{conn: gate}), defaultWorkerOperationTimeout)
	var releaseOnce sync.Once
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(gate.release) })
		_ = relay.Close()
	})
	if _, err := relay.HandleFrame(context.Background(), controlFrame(t, protocol.Control{Type: protocol.TypeAttach, SessionID: "7K3D"})); err != nil {
		t.Fatal(err)
	}

	// The worker hangs up right after its attach reply; resizes then fail to write.
	resize := controlFrame(t, protocol.Control{Type: protocol.TypeResize, SessionID: "7K3D", Cols: 100, Rows: 30})
	sid := mustRelaySessionID(t, "7K3D")
	deadline := time.Now().Add(sessionInspectorE2ETimeout)
	for {
		if _, err := relay.HandleFrame(context.Background(), resize); err != nil {
			t.Fatal(err)
		}
		lane, err := relay.currentLane(sid)
		if err != nil {
			t.Fatal(err)
		}
		if lane.inputWasStopped() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("lane never stopped input after the worker hung up")
		}
		time.Sleep(5 * time.Millisecond)
	}

	releaseOnce.Do(func() { close(gate.release) })
	for {
		select {
		case frame := <-client.writes:
			if frame.Kind != protocol.KindControl {
				continue
			}
			message, err := protocol.DecodeControl(frame.Payload)
			if err != nil {
				t.Fatal(err)
			}
			if message.Type == protocol.TypeError && strings.Contains(message.Message, "ended before the session reported its end") {
				return
			}
		case <-time.After(sessionInspectorE2ETimeout):
			t.Fatal("client was never told the worker connection ended")
		}
	}
}

func serveAttachThenHangUp(listener net.Listener) {
	stream, err := listener.Accept()
	if err != nil {
		return
	}
	conn, err := transport.NewStreamConn(stream)
	if err != nil {
		return
	}
	defer conn.Close() //nolint:errcheck // the fixture worker ends abruptly on purpose
	if _, err := conn.ReadFrame(); err != nil {
		return
	}
	payload, err := (protocol.Control{Type: protocol.TypeAttached, SessionID: "7K3D"}).Encode()
	if err != nil {
		return
	}
	_ = conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload})
}
