package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/worker"
)

func TestClientRelayRealWorkerExpiryBeforeEvictionPreservesIncumbent(t *testing.T) {
	client, relay, candidate, workerDone := realWorkerHandoffFixture(t, "request")
	result, ctx := beginRealWorkerReplacement(t, relay)
	waitRelaySignal(t, candidate.entered, "replacement request before worker eviction")
	ctx.expire()
	if err := waitRelayError(t, result, "expired real worker handshake"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("replacement error = %v, want deadline", err)
	}
	assertRealHandoffEcho(t, relay, client, "incumbent-survived")
	assertHandoffCommandAlive(t, workerDone)
	assertHandoffCandidateOnlyAttached(t, candidate)
}

func TestClientRelayRealWorkerExpiryAfterEvictionAllowsFreshAttach(t *testing.T) {
	client, relay, candidate, workerDone := realWorkerHandoffFixture(t, "response")
	result, ctx := beginRealWorkerReplacement(t, relay)
	waitRelaySignal(t, candidate.entered, "worker acknowledgement after eviction")
	ctx.expire()
	err := waitRelayError(t, result, "expired post-eviction worker handshake")
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "still running") || !strings.Contains(err.Error(), "attach again") {
		t.Fatalf("post-eviction error = %v, want deadline and live-session reattach guidance", err)
	}
	for {
		frame := client.nextWrite(t)
		if frame.Kind != protocol.KindControl {
			continue
		}
		msg, err := protocol.DecodeControl(frame.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if msg.Type == protocol.TypeDetach {
			if msg.Reason != protocol.ReasonStolen {
				t.Fatalf("incumbent detach reason = %q, want stolen", msg.Reason)
			}
			break
		}
	}
	assertHandoffCommandAlive(t, workerDone)
	assertHandoffCandidateOnlyAttached(t, candidate)
	if _, err := relay.HandleFrame(context.Background(), controlFrame(t, protocol.Control{Type: protocol.TypeAttach, SessionID: "7K3D"})); err != nil {
		t.Fatalf("fresh attach after post-eviction expiry: %v", err)
	}
	assertRealHandoffEcho(t, relay, client, "fresh-attachment-survived")
	assertHandoffCommandAlive(t, workerDone)
}

func beginRealWorkerReplacement(t *testing.T, relay *clientRelay) (<-chan error, *handoffExpiryContext) {
	t.Helper()
	ctx := newHandoffExpiryContext()
	frame := controlFrame(t, protocol.Control{Type: protocol.TypeAttach, SessionID: "7K3D"})
	result := make(chan error, 1)
	go func() {
		_, err := relay.HandleFrame(ctx, frame)
		result <- err
	}()
	return result, ctx
}

func assertHandoffCommandAlive(t *testing.T, workerDone <-chan error) {
	t.Helper()
	select {
	case err := <-workerDone:
		t.Fatalf("handshake expiry stopped the command: %v", err)
	default:
	}
}

func assertHandoffCandidateOnlyAttached(t *testing.T, candidate *handoffGateConn) {
	t.Helper()
	select {
	case <-candidate.closed:
	default:
		t.Fatal("handshake expiry did not close candidate")
	}
	for _, msg := range candidate.controls {
		if msg.Type != protocol.TypeAttach {
			t.Fatalf("handshake expiry sent %s to real worker", msg.Type)
		}
	}
}

func realWorkerHandoffFixture(t *testing.T, boundary string) (*relayTestConn, *clientRelay, *handoffGateConn, <-chan error) {
	t.Helper()
	root := compactSocketTempDir(t)
	dir := filepath.Join(root, "s")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stop := filepath.Join(root, "stop")
	workerDone := make(chan error, 1)
	go func() {
		_, err := worker.Run(worker.Config{
			ID: "7K3D", Dir: dir, Cwd: root, Cols: 80, Rows: 24,
			Env:     []string{"PATH=/usr/bin:/bin", "TERM=xterm-256color", "LANG=C"},
			Command: []string{"/bin/sh", "-c", `while [ ! -e "$1" ]; do sleep 0.01; done`, "handoff-worker", stop},
		})
		workerDone <- err
	}()
	t.Cleanup(func() {
		if err := os.WriteFile(stop, nil, 0o600); err != nil {
			t.Error(err)
			return
		}
		select {
		case err := <-workerDone:
			if err != nil {
				t.Errorf("stop fixture worker: %v", err)
			}
		case <-time.After(sessionInspectorE2ETimeout):
			t.Error("fixture worker did not finish")
		}
	})
	waitForPath(t, paths.Socket(dir))
	connect := func() transport.Conn {
		socket, err := net.Dial("unix", paths.Socket(dir))
		if err != nil {
			t.Fatal(err)
		}
		conn, err := transport.NewStreamConn(socket)
		if err != nil {
			_ = socket.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}
	incumbent := connect()
	candidate := &handoffGateConn{
		Conn: connect(), boundary: boundary,
		entered: make(chan struct{}), released: make(chan struct{}), closed: make(chan struct{}),
	}
	client := newRelayTestConn()
	relay := newClientRelay(client, newRelayTestConnector(connectResult{conn: incumbent}, connectResult{conn: candidate}, connectResult{conn: connect()}), defaultWorkerOperationTimeout)
	t.Cleanup(func() { candidate.release(); _ = relay.Close() })
	if _, err := relay.HandleFrame(context.Background(), controlFrame(t, protocol.Control{Type: protocol.TypeAttach, SessionID: "7K3D"})); err != nil {
		t.Fatal(err)
	}
	assertRealHandoffEcho(t, relay, client, "incumbent-ready")
	return client, relay, candidate, workerDone
}

func assertRealHandoffEcho(t *testing.T, relay *clientRelay, client *relayTestConn, marker string) {
	t.Helper()
	if _, err := relay.HandleFrame(context.Background(), protocol.Frame{
		Kind: protocol.KindInput, Session: mustRelaySessionID(t, "7K3D"), Payload: []byte(marker + "\n"),
	}); err != nil {
		t.Fatalf("input to surviving attachment: %v", err)
	}
	var output []byte
	for !bytes.Contains(output, []byte(marker)) {
		frame := client.nextWrite(t)
		if frame.Kind == protocol.KindControl {
			msg, err := protocol.DecodeControl(frame.Payload)
			if err != nil {
				t.Fatal(err)
			}
			if msg.Type == protocol.TypeDetach || msg.Type == protocol.TypeExit {
				t.Fatalf("surviving attachment received %s/%s", msg.Type, msg.Reason)
			}
		} else {
			output = append(output, frame.Payload...)
		}
	}
}

type handoffGateConn struct {
	transport.Conn
	controls    []protocol.Control
	boundary    string
	entered     chan struct{}
	released    chan struct{}
	closed      chan struct{}
	gateOnce    sync.Once
	closeOnce   sync.Once
	releaseOnce sync.Once
}

func (c *handoffGateConn) hold() error {
	c.gateOnce.Do(func() { close(c.entered) })
	select {
	case <-c.released:
		return nil
	case <-c.closed:
		return transport.ErrClosed
	}
}

func (c *handoffGateConn) ReadFrame() (protocol.Frame, error) {
	frame, err := c.Conn.ReadFrame()
	if err != nil {
		return frame, fmt.Errorf("handoff gate: read worker: %w", err)
	}
	if c.boundary == "response" {
		msg, err := protocol.DecodeControl(frame.Payload)
		if err != nil {
			return protocol.Frame{}, fmt.Errorf("handoff gate: decode attached response: %w", err)
		}
		if frame.Kind != protocol.KindControl || msg.Type != protocol.TypeAttached {
			return protocol.Frame{}, fmt.Errorf("handoff gate: expected genuine attached response, got %+v", frame)
		}
		if err := c.hold(); err != nil {
			return protocol.Frame{}, err
		}
	}

	return frame, nil
}

func (c *handoffGateConn) WriteFrame(frame protocol.Frame) error {
	if frame.Kind == protocol.KindControl {
		msg, err := protocol.DecodeControl(frame.Payload)
		if err != nil {
			return fmt.Errorf("handoff gate: decode control: %w", err)
		}
		c.controls = append(c.controls, msg)
	}
	if c.boundary == "request" {
		if err := c.hold(); err != nil {
			return err
		}
	}
	if err := c.Conn.WriteFrame(frame); err != nil {
		return fmt.Errorf("handoff gate: write worker: %w", err)
	}
	return nil
}

func (c *handoffGateConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	if err := c.Conn.Close(); err != nil {
		return fmt.Errorf("handoff gate: close worker: %w", err)
	}
	return nil
}

func (c *handoffGateConn) release() { c.releaseOnce.Do(func() { close(c.released) }) }

// This caller expires only at a witnessed socket boundary, not during setup.
type handoffExpiryContext struct {
	context.Context
	done chan struct{}
}

func newHandoffExpiryContext() *handoffExpiryContext {
	return &handoffExpiryContext{Context: context.Background(), done: make(chan struct{})}
}

func (c *handoffExpiryContext) Done() <-chan struct{} { return c.done }
func (c *handoffExpiryContext) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}
func (c *handoffExpiryContext) expire() { close(c.done) }
