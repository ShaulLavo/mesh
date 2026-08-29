package transport

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
)

func TestBatchingConnCoalescesContiguousOutputAndFlushesBeforeControl(t *testing.T) {
	t.Parallel()

	dst := &recordingConn{}
	conn, err := NewBatchingConn(dst, BatchOptions{
		MaxBytes:      32,
		FlushInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	sid, err := protocol.NewSessionID("7K3D")
	if err != nil {
		t.Fatal(err)
	}

	first := []byte("ab")
	if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindData, Session: sid, Seq: 10, Payload: first}); err != nil {
		t.Fatal(err)
	}
	first[0] = 'x'
	if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindData, Session: sid, Seq: 12, Payload: []byte("cd")}); err != nil {
		t.Fatal(err)
	}
	if got := dst.snapshot(); len(got) != 0 {
		t.Fatalf("writes before flush = %d, want 0", len(got))
	}

	control := protocol.Frame{Kind: protocol.KindControl, Payload: []byte(`{"type":"barrier"}`)}
	if err := conn.WriteFrame(control); err != nil {
		t.Fatal(err)
	}
	writes := dst.snapshot()
	if len(writes) != 2 {
		t.Fatalf("writes = %d, want coalesced data plus control", len(writes))
	}
	wantData := protocol.Frame{Kind: protocol.KindData, Session: sid, Seq: 10, Payload: []byte("abcd")}
	assertFrameEqual(t, writes[0], wantData)
	assertFrameEqual(t, writes[1], control)
}

func TestBatchingConnFlushesAtSizeAndOnTimer(t *testing.T) {
	t.Parallel()

	dst := &recordingConn{wrote: make(chan struct{}, 4)}
	conn, err := NewBatchingConn(dst, BatchOptions{
		MaxBytes:      4,
		FlushInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close() //nolint:errcheck // test cleanup
	sid, err := protocol.NewSessionID("7K3D")
	if err != nil {
		t.Fatal(err)
	}

	if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindData, Session: sid, Seq: 0, Payload: []byte("abcd")}); err != nil {
		t.Fatal(err)
	}
	waitForWrite(t, dst.wrote)
	if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindData, Session: sid, Seq: 4, Payload: []byte("e")}); err != nil {
		t.Fatal(err)
	}
	waitForWrite(t, dst.wrote)

	writes := dst.snapshot()
	if len(writes) != 2 || string(writes[0].Payload) != "abcd" || string(writes[1].Payload) != "e" {
		t.Fatalf("writes = %+v", writes)
	}
}

func TestBatchingConnCloseInterruptsBlockedWrite(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name       string
		startWrite func(*testing.T, *BatchingConn) <-chan error
	}{
		{
			name: "caller",
			startWrite: func(t *testing.T, conn *BatchingConn) <-chan error {
				t.Helper()
				done := make(chan error, 1)
				go func() {
					done <- conn.WriteFrame(protocol.Frame{
						Kind:    protocol.KindControl,
						Payload: []byte(`{"type":"barrier"}`),
					})
				}()
				return done
			},
		},
		{
			name: "timer",
			startWrite: func(t *testing.T, conn *BatchingConn) <-chan error {
				t.Helper()
				sid, err := protocol.NewSessionID("7K3D")
				if err != nil {
					t.Fatal(err)
				}
				if err := conn.WriteFrame(protocol.Frame{
					Kind:    protocol.KindData,
					Session: sid,
					Payload: []byte("x"),
				}); err != nil {
					t.Fatal(err)
				}
				return nil
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			dst := newCloseUnblocksConn()
			conn, err := NewBatchingConn(dst, BatchOptions{
				MaxBytes:      32,
				FlushInterval: time.Millisecond,
			})
			if err != nil {
				t.Fatal(err)
			}
			writeDone := test.startWrite(t, conn)
			select {
			case <-dst.writeStarted:
			case <-time.After(250 * time.Millisecond):
				t.Fatal("destination write did not start")
			}

			closeDone := make(chan error, 1)
			go func() { closeDone <- conn.Close() }()
			var closeErr error
			select {
			case closeErr = <-closeDone:
			case <-time.After(250 * time.Millisecond):
				// Release the old implementation's lock cycle before failing so
				// this regression never leaves a stuck goroutine behind.
				_ = dst.Close()
				<-closeDone
				if writeDone != nil {
					<-writeDone
				}
				t.Fatal("Close blocked behind destination WriteFrame")
			}
			if !errors.Is(closeErr, errBlockedWriteClosed) {
				t.Fatalf("Close error = %v, want blocked write error", closeErr)
			}
			if writeDone != nil {
				if err := <-writeDone; !errors.Is(err, errBlockedWriteClosed) {
					t.Fatalf("WriteFrame error = %v, want blocked write error", err)
				}
			}
			if err := conn.Close(); !errors.Is(err, errBlockedWriteClosed) {
				t.Fatalf("second Close error = %v, want first Close result", err)
			}
			if calls := dst.closeCalls.Load(); calls != 1 {
				t.Fatalf("destination Close calls = %d, want 1", calls)
			}
		})
	}
}

func waitForWrite(t *testing.T, wrote <-chan struct{}) {
	t.Helper()
	select {
	case <-wrote:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("timed out waiting for batched write")
	}
}

type recordingConn struct {
	mu     sync.Mutex
	writes []protocol.Frame
	wrote  chan struct{}
}

func (c *recordingConn) ReadFrame() (protocol.Frame, error) {
	return protocol.Frame{}, io.EOF
}

func (c *recordingConn) WriteFrame(frame protocol.Frame) error {
	frame.Payload = bytes.Clone(frame.Payload)
	c.mu.Lock()
	c.writes = append(c.writes, frame)
	c.mu.Unlock()
	if c.wrote != nil {
		c.wrote <- struct{}{}
	}
	return nil
}

func (c *recordingConn) Close() error { return nil }

func (c *recordingConn) snapshot() []protocol.Frame {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]protocol.Frame(nil), c.writes...)
}

var errBlockedWriteClosed = errors.New("blocked write interrupted by close")

type closeUnblocksConn struct {
	writeStarted chan struct{}
	closed       chan struct{}
	startOnce    sync.Once
	closeOnce    sync.Once
	closeCalls   atomic.Int32
}

func newCloseUnblocksConn() *closeUnblocksConn {
	return &closeUnblocksConn{
		writeStarted: make(chan struct{}),
		closed:       make(chan struct{}),
	}
}

func (c *closeUnblocksConn) ReadFrame() (protocol.Frame, error) {
	return protocol.Frame{}, io.EOF
}

func (c *closeUnblocksConn) WriteFrame(protocol.Frame) error {
	c.startOnce.Do(func() { close(c.writeStarted) })
	<-c.closed
	return errBlockedWriteClosed
}

func (c *closeUnblocksConn) Close() error {
	c.closeCalls.Add(1)
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}
