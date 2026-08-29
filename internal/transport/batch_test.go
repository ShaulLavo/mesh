package transport

import (
	"bytes"
	"io"
	"sync"
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
