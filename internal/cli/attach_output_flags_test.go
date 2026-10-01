//go:build !windows

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/shaul/mesh/internal/protocol"
)

func TestAttachNeverMakesSharedOutputNonblocking(t *testing.T) {
	for _, cancelAttachment := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%t", cancelAttachment), func(t *testing.T) {
			input, err := os.CreateTemp(t.TempDir(), "input")
			if err != nil {
				t.Fatal(err)
			}
			reader, output, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = input.Close(); _ = reader.Close(); _ = output.Close() })
			// Model an inherited, blocking stdout, not os.Pipe's pollable file.
			_ = output.Fd()
			assertOutputBlocking(t, output, "before attachment")
			conn := &flagCheckConn{outputCancelConn: &outputCancelConn{
				frames: make(chan protocol.Frame, 2), closed: make(chan struct{}),
			}, ready: make(chan struct{})}
			conn.frames <- mustCommandControlFrame(protocol.Control{Type: protocol.TypeAttached, SessionID: "7K3D"})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := Attach(ctx, AttachOptions{SessionID: "7K3D", Conn: conn, In: input, Out: output})
				done <- err
			}()
			select {
			case <-conn.ready:
			case <-time.After(time.Second):
				t.Fatal("attachment did not acknowledge")
			}
			assertOutputBlocking(t, output, "during attachment")
			if cancelAttachment {
				cancel()
			} else {
				conn.frames <- mustCommandControlFrame(protocol.Control{Type: protocol.TypeExit, SessionID: "7K3D"})
			}
			select {
			case err := <-done:
				if cancelAttachment && !errors.Is(err, context.Canceled) {
					t.Errorf("cancelled attachment = %v", err)
				}
				if !cancelAttachment && err != nil {
					t.Errorf("completed attachment = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("attachment did not return")
			}
			assertOutputBlocking(t, output, "after attachment")
		})
	}
}

func TestAttachDoesNotMakeConcurrentPipeWriterFailWithEAGAIN(t *testing.T) {
	input, err := os.CreateTemp(t.TempDir(), "input")
	if err != nil {
		t.Fatal(err)
	}
	reader, output, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = input.Close(); _ = reader.Close(); _ = output.Close() })
	if err := output.SetWriteDeadline(time.Now().Add(10 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := output.Write(bytes.Repeat([]byte("x"), 1<<20)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("fill output pipe: %v", err)
	}
	if err := output.SetWriteDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Dup(int(output.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	peer := os.NewFile(uintptr(fd), "concurrent-output")
	t.Cleanup(func() { _ = peer.Close() })
	conn := &flagCheckConn{outputCancelConn: &outputCancelConn{frames: make(chan protocol.Frame, 2), closed: make(chan struct{})}, ready: make(chan struct{})}
	conn.frames <- mustCommandControlFrame(protocol.Control{Type: protocol.TypeAttached, SessionID: "7K3D"})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := Attach(ctx, AttachOptions{SessionID: "7K3D", Conn: conn, In: input, Out: output})
		done <- err
	}()
	select {
	case <-conn.ready:
	case <-time.After(time.Second):
		t.Fatal("attachment did not acknowledge")
	}
	assertOutputBlocking(t, output, "during a blocked output write")
	written := make(chan error, 1)
	go func() {
		_, err := unix.Write(fd, bytes.Repeat([]byte("p"), 512))
		written <- err
	}()
	peerReturned := false
	select {
	case err := <-written:
		peerReturned = true
		t.Errorf("concurrent writer returned before the full pipe was drained: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("cancelled pipe attachment = %v", err)
		}
	case <-time.After(time.Second):
		_ = reader.Close()
		<-done
		t.Error("cancelled output did not return before draining")
	}
	drained := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, reader); close(drained) }()
	if !peerReturned {
		if err := <-written; err != nil {
			t.Errorf("concurrent writer failed after draining: %v", err)
		}
	}
	_ = peer.Close()
	_ = output.Close()
	<-drained
}

func assertOutputBlocking(t *testing.T, output *os.File, stage string) {
	t.Helper()
	raw, err := output.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var flags int
	var queryErr error
	if err := raw.Control(func(fd uintptr) { flags, queryErr = unix.FcntlInt(fd, unix.F_GETFL, 0) }); err != nil {
		t.Fatal(err)
	}
	if queryErr != nil {
		t.Fatal(queryErr)
	}
	if flags&unix.O_NONBLOCK != 0 {
		t.Errorf("original stdout has O_NONBLOCK %s", stage)
	}
}

type flagCheckConn struct {
	*outputCancelConn
	ready     chan struct{}
	ack       bool
	onceReady sync.Once
}

func (c *flagCheckConn) ReadFrame() (protocol.Frame, error) {
	if c.ack {
		c.onceReady.Do(func() { close(c.ready) })
	}
	frame, err := c.outputCancelConn.ReadFrame()
	if err == nil && frame.Kind == protocol.KindControl {
		control, _ := protocol.DecodeControl(frame.Payload)
		if control.Type == protocol.TypeAttached {
			c.ack = true
		}
	}
	return frame, err
}
