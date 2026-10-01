//go:build linux

package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/creack/pty"

	"github.com/shaul/mesh/internal/protocol"
)

func TestAttachCancellationUnblocksFullOutputAndRestoresPTY(t *testing.T) {
	master, input, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = master.Close(); _ = input.Close() })
	before, err := term.GetState(input.Fd())
	if err != nil {
		t.Fatal(err)
	}
	reader, output, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close(); _ = output.Close() })
	conn := &outputCancelConn{frames: make(chan protocol.Frame, 2), closed: make(chan struct{})}
	conn.frames <- mustCommandControlFrame(protocol.Control{Type: protocol.TypeAttached, SessionID: "7K3D"})
	conn.frames <- protocol.Frame{Kind: protocol.KindData, Payload: bytes.Repeat([]byte("x"), 1<<20)}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := Attach(ctx, AttachOptions{SessionID: "7K3D", Conn: conn, In: input, Out: output})
		done <- err
	}()
	if err := reader.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(reader, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("full-output cancellation = %v", err)
		}
	case <-time.After(time.Second):
		_ = reader.SetReadDeadline(time.Time{})
		drained := make(chan struct{})
		go func() { _, _ = io.Copy(io.Discard, reader); close(drained) }()
		<-done
		_ = output.Close()
		<-drained
		t.Fatal("cancellation left output blocked until the pipe was drained")
	}
	after, err := term.GetState(input.Fd())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("output cancellation did not restore termios")
	}
	if _, err := output.Stat(); err != nil {
		t.Fatalf("attachment closed caller-owned output: %v", err)
	}
	if _, err := input.Stat(); err != nil {
		t.Fatalf("attachment closed caller-owned input: %v", err)
	}
	if got := goroutinesWithStack("internal/cli.relayInput"); got != 0 {
		t.Fatalf("input relay leaked: %d", got)
	}
	conn.mu.Lock()
	defer conn.mu.Unlock()
	for _, message := range conn.controls {
		if message.Type == protocol.TypeDetach || message.Type == protocol.TypeSignal || message.Type == protocol.TypeKill {
			t.Fatalf("cancellation sent worker control: %s", message.Type)
		}
	}
}

func TestAttachRestorationToFullOutputIsBounded(t *testing.T) {
	master, input, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = master.Close(); _ = input.Close() })
	before, err := term.GetState(input.Fd())
	if err != nil {
		t.Fatal(err)
	}
	reader, output, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close(); _ = output.Close() })
	if err := output.SetWriteDeadline(time.Now().Add(10 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := output.Write(bytes.Repeat([]byte("x"), 1<<20)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("fill output pipe: %v", err)
	}
	if err := output.SetWriteDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	conn := &outputCancelConn{frames: make(chan protocol.Frame, 2), closed: make(chan struct{})}
	conn.frames <- mustCommandControlFrame(protocol.Control{Type: protocol.TypeAttached, SessionID: "7K3D"})
	conn.frames <- mustCommandControlFrame(protocol.Control{Type: protocol.TypeExit, SessionID: "7K3D"})
	done := make(chan error, 1)
	go func() {
		_, err := Attach(t.Context(), AttachOptions{SessionID: "7K3D", Conn: conn, In: input, Out: output})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		drained := make(chan struct{})
		go func() { _, _ = io.Copy(io.Discard, reader); close(drained) }()
		<-done
		_ = output.Close()
		<-drained
		t.Fatal("restoration escape output blocked attachment return")
	}
	after, err := term.GetState(input.Fd())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("restoration output prevented termios restoration")
	}
	if _, err := output.Stat(); err != nil {
		t.Fatalf("restoration closed caller-owned output: %v", err)
	}
}
