package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/terminal"
	"github.com/shaul/mesh/internal/transport"
)

func TestAttachWithTerminalUsesExplicitStreamsAndResizes(t *testing.T) {
	t.Setenv("MESH_DEPTH", "8")
	client := startTerminalAttachment(t)
	if client.request.Cols != 127 || client.request.Rows != 39 || len(client.request.ContainingSessions) != 0 {
		t.Fatalf("attachment request = %#v", client.request)
	}
	client.control(protocol.Control{Type: protocol.TypeAttached, SessionID: "7K3D"})
	sid, err := protocol.NewSessionID("7K3D")
	if err != nil {
		t.Fatal(err)
	}
	payload := enterAltScreenSequence + "remote output"
	if err := client.writer.WriteData(sid, 0, []byte(payload)); err != nil {
		t.Fatal(err)
	}
	client.resizes <- terminal.Size{Cols: 92, Rows: 18}
	resize := client.nextControl()
	if resize.Type != protocol.TypeResize || resize.Cols != 92 || resize.Rows != 18 || resize.SessionID != "7K3D" {
		t.Fatalf("resize request = %#v", resize)
	}
	if _, err := client.input.Write([]byte{'x', DefaultDetachKey}); err != nil {
		t.Fatal(err)
	}
	frame, err := client.reader.ReadFrame()
	if err != nil || frame.Kind != protocol.KindInput || string(frame.Payload) != "x" {
		t.Fatalf("input frame = %#v, error = %v", frame, err)
	}
	if detached := client.nextControl(); detached.Type != protocol.TypeDetach || detached.Reason != protocol.ReasonClient {
		t.Fatalf("detach request = %#v", detached)
	}
	result := client.wait()
	if result.err != nil || !result.result.Detached || result.result.LastSeq != uint64(len(payload)) {
		t.Fatalf("attachment result = %#v, error = %v", result.result, result.err)
	}
	want := payload + leaveAltScreenSequence + restoreTerminalState
	if got := client.output.String(); got != want {
		t.Fatalf("terminal output = %q, want %q", got, want)
	}
	client.assertInputStopped()
}

func TestAttachWithTerminalCancellationJoinsBlockedInputAndResizes(t *testing.T) {
	client := startTerminalAttachment(t)
	client.control(protocol.Control{Type: protocol.TypeAttached, SessionID: "7K3D"})
	select {
	case <-client.trackedInput.reading:
	case <-time.After(time.Second):
		t.Fatal("attachment did not read input")
	}
	client.cancel()
	result := client.wait()
	if !errors.Is(result.err, context.Canceled) || result.result.Detached || result.result.Exited {
		t.Fatalf("canceled attachment = %#v, error = %v", result.result, result.err)
	}
	client.assertInputStopped()
	if _, err := client.reader.ReadFrame(); err == nil {
		t.Fatal("attachment transport stayed open")
	}
	select {
	case client.resizes <- terminal.Size{Cols: 80, Rows: 24}:
		t.Fatal("resize relay remained after attachment returned")
	default:
	}
}

func TestAttachWithTerminalCancellationInterruptsInitialWrite(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	conn, err := transport.NewStreamConn(client)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var cancellations atomic.Int32
	done := make(chan error, 1)
	go func() {
		_, err := AttachWithTerminal(ctx, AttachOptions{SessionID: "7K3D", Conn: conn}, AttachTerminal{
			Input: bytes.NewReader(nil), Output: io.Discard, Size: terminal.Size{Cols: 80, Rows: 24},
			CancelInput: func() { cancellations.Add(1) },
		})
		done <- err
	}()
	// Reading only the header leaves the initial request's payload blocked.
	header := make([]byte, 5)
	if _, err := io.ReadFull(server, header); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled initial write error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("context cancellation did not interrupt the initial write")
	}
	if got := cancellations.Load(); got != 1 {
		t.Fatalf("input canceled %d times, want 1", got)
	}
}

func TestAttachWithTerminalInvalidRequestReleasesOwnedResources(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	conn, err := transport.NewStreamConn(client)
	if err != nil {
		t.Fatal(err)
	}
	var cancellations int
	_, err = AttachWithTerminal(context.Background(), AttachOptions{SessionID: "invalid id", Conn: conn}, AttachTerminal{
		Input: bytes.NewReader(nil), Output: io.Discard, Size: terminal.Size{Cols: 80, Rows: 24},
		CancelInput: func() { cancellations++ },
	})
	if err == nil || cancellations != 1 {
		t.Fatalf("invalid request error = %v, input cancellation count = %d", err, cancellations)
	}
	if _, err := server.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("invalid request retained its connection: %v", err)
	}
}

type terminalAttachResult struct {
	result AttachResult
	err    error
}

type terminalAttachment struct {
	t            *testing.T
	reader       *protocol.Reader
	writer       *protocol.Writer
	request      protocol.Control
	input        *io.PipeWriter
	trackedInput *trackedAttachInput
	output       *bytes.Buffer
	resizes      chan terminal.Size
	cancel       context.CancelFunc
	done         chan terminalAttachResult
}

type trackedAttachInput struct {
	reader        *io.PipeReader
	reading       chan struct{}
	firstRead     sync.Once
	active        atomic.Int32
	cancellations atomic.Int32
}

func (r *trackedAttachInput) Read(buffer []byte) (int, error) {
	r.active.Add(1)
	defer r.active.Add(-1)
	r.firstRead.Do(func() { close(r.reading) })
	return r.reader.Read(buffer)
}

func (r *trackedAttachInput) cancel() {
	r.cancellations.Add(1)
	_ = r.reader.Close()
}

func startTerminalAttachment(t *testing.T) *terminalAttachment {
	t.Helper()
	client, server := net.Pipe()
	if err := server.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	conn, err := transport.NewStreamConn(client)
	if err != nil {
		t.Fatal(err)
	}
	input, writer := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = client.Close()
		_ = server.Close()
		_ = input.Close()
		_ = writer.Close()
	})
	attachment := &terminalAttachment{
		t: t, reader: protocol.NewReader(server), writer: protocol.NewWriter(server),
		input: writer, trackedInput: &trackedAttachInput{reader: input, reading: make(chan struct{})},
		output: &bytes.Buffer{}, resizes: make(chan terminal.Size), cancel: cancel,
		done: make(chan terminalAttachResult, 1),
	}
	go func() {
		result, err := AttachWithTerminal(ctx, AttachOptions{SessionID: "7K3D", Conn: conn}, AttachTerminal{
			Input: attachment.trackedInput, Output: attachment.output, Size: terminal.Size{Cols: 127, Rows: 39},
			Resizes: attachment.resizes, CancelInput: attachment.trackedInput.cancel,
		})
		attachment.done <- terminalAttachResult{result: result, err: err}
	}()
	attachment.request = attachment.nextControl()
	return attachment
}

func (c *terminalAttachment) control(message protocol.Control) {
	c.t.Helper()
	if err := c.writer.WriteControlMsg(message); err != nil {
		c.t.Fatal(err)
	}
}

func (c *terminalAttachment) nextControl() protocol.Control {
	c.t.Helper()
	frame, err := c.reader.ReadFrame()
	if err != nil {
		c.t.Fatal(err)
	}
	message, err := protocol.DecodeControl(frame.Payload)
	if err != nil || frame.Kind != protocol.KindControl {
		c.t.Fatalf("control frame = %#v, error = %v", frame, err)
	}
	return message
}

func (c *terminalAttachment) wait() terminalAttachResult {
	c.t.Helper()
	select {
	case result := <-c.done:
		return result
	case <-time.After(time.Second):
		c.t.Fatal("attachment did not return")
		return terminalAttachResult{}
	}
}

func (c *terminalAttachment) assertInputStopped() {
	c.t.Helper()
	if active := c.trackedInput.active.Load(); active != 0 {
		c.t.Fatalf("active input readers = %d, want 0", active)
	}
	if count := c.trackedInput.cancellations.Load(); count != 1 {
		c.t.Fatalf("input canceled %d times, want 1", count)
	}
}
