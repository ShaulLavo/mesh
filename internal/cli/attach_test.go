package cli

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
)

func TestAttachStopsReadingInputBeforeReturning(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "worker.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	detach := make(chan struct{})
	serverErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()
		if _, err := protocol.NewReader(conn).ReadFrame(); err != nil {
			serverErr <- err
			return
		}
		<-detach
		serverErr <- protocol.NewWriter(conn).WriteControlMsg(protocol.Control{
			Type:      protocol.TypeDetach,
			SessionID: "STOP",
		})
	}()

	in, input, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	defer input.Close()
	out, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()

	baseline := goroutinesWithStack("internal/cli.relayInput")
	attached := make(chan error, 1)
	go func() {
		_, err := Attach(AttachOptions{
			SocketPath: socketPath,
			SessionID:  "STOP",
			In:         in,
			Out:        out,
		})
		attached <- err
	}()
	deadline := time.Now().Add(time.Second)
	for goroutinesWithStack("internal/cli.relayInput") <= baseline {
		if time.Now().After(deadline) {
			t.Fatal("input relay did not block on input")
		}
		time.Sleep(time.Millisecond)
	}
	close(detach)
	if err := <-attached; err != nil {
		t.Fatal(err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
	if got := goroutinesWithStack("internal/cli.relayInput"); got != baseline {
		t.Fatalf("blocked input relays after Attach = %d, want baseline %d", got, baseline)
	}
}

func TestAttachTransportErrorsAreBoundedAndPreserveCause(t *testing.T) {
	cause := errors.New("ATTACKER\r\n\u202e" + strings.Repeat("x", 10_000))
	for _, test := range []struct {
		name string
		conn *failingCLIConn
	}{
		{name: "write", conn: &failingCLIConn{writeErr: fmt.Errorf("peer write: %w", cause)}},
		{name: "read", conn: &failingCLIConn{readErr: fmt.Errorf("peer close: %w", cause)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			input, err := os.CreateTemp(t.TempDir(), "input")
			if err != nil {
				t.Fatal(err)
			}
			defer input.Close()
			output, err := os.CreateTemp(t.TempDir(), "output")
			if err != nil {
				t.Fatal(err)
			}
			defer output.Close()
			_, err = Attach(AttachOptions{SessionID: "7K3D", Conn: test.conn, In: input, Out: output})
			if err == nil || !errors.Is(err, cause) || strings.ContainsAny(err.Error(), "\r\n\x1b") || strings.ContainsRune(err.Error(), '\u202e') || len(err.Error()) > maximumRemoteErrorBytes+100 {
				t.Fatalf("bounded attach error = %q (%d bytes), errors.Is = %v", err, len(err.Error()), errors.Is(err, cause))
			}
		})
	}
}

func TestResizeRelayStopsWithoutClosingSignalChannel(t *testing.T) {
	winch := make(chan os.Signal)
	done := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		relayResizes(done, winch, func() {})
		close(exited)
	}()
	close(done)
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("resize relay did not stop")
	}
}

func goroutinesWithStack(markers ...string) int {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	count := 0
	for _, stack := range strings.Split(string(buf[:n]), "\n\n") {
		matches := true
		for _, marker := range markers {
			if !strings.Contains(stack, marker) {
				matches = false
				break
			}
		}
		if matches {
			count++
		}
	}
	return count
}

func TestAttachRendersSnapshotWithoutAdvancingResumeSequence(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "worker.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	sid, err := protocol.NewSessionID("SNAP")
	if err != nil {
		t.Fatal(err)
	}
	serverErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()
		if _, err := protocol.NewReader(conn).ReadFrame(); err != nil {
			serverErr <- err
			return
		}

		writer := protocol.NewWriter(conn)
		if err := writer.WriteControlMsg(protocol.Control{
			Type:      protocol.TypeAttached,
			SessionID: sid.String(),
			Seq:       42,
			Snapshot:  true,
		}); err != nil {
			serverErr <- err
			return
		}
		if err := writer.WriteSnapshot(sid, []byte("snapshot")); err != nil {
			serverErr <- err
			return
		}
		if err := writer.WriteData(sid, 42, []byte("live")); err != nil {
			serverErr <- err
			return
		}
		serverErr <- writer.WriteControlMsg(protocol.Control{
			Type:      protocol.TypeDetach,
			SessionID: sid.String(),
		})
	}()

	in, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()

	initialSeq := uint64(0)
	result, err := Attach(AttachOptions{
		SocketPath: socketPath,
		SessionID:  sid.String(),
		LastSeq:    &initialSeq,
		In:         in,
		Out:        out,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
	if !result.Detached {
		t.Fatal("server detach was not reported")
	}
	if result.LastSeq != 46 {
		t.Fatalf("last sequence = %d, want 46", result.LastSeq)
	}
	if _, err := out.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte("snapshotlive")) {
		t.Fatalf("output = %q, want snapshot and live data", got)
	}
}

func TestAttachDoesNotCommitAnIncompleteSnapshot(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "worker.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	serverErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		if _, err := protocol.NewReader(conn).ReadFrame(); err != nil {
			_ = conn.Close()
			serverErr <- err
			return
		}
		err = protocol.NewWriter(conn).WriteControlMsg(protocol.Control{
			Type:      protocol.TypeAttached,
			SessionID: "SNAP",
			Seq:       42,
			Snapshot:  true,
		})
		if err == nil {
			// Announce a 12-byte snapshot body (8-byte session plus 4 bytes),
			// then sever the stream after only half of its rendered bytes.
			_, err = conn.Write([]byte{
				byte(protocol.KindSnapshot), 0, 0, 0, 12,
				'S', 'N', 'A', 'P', 0, 0, 0, 0,
				'p', 'a',
			})
		}
		_ = conn.Close()
		serverErr <- err
	}()

	in, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()

	lastSeq := uint64(7)
	result, err := Attach(AttachOptions{
		SocketPath: socketPath,
		SessionID:  "SNAP",
		LastSeq:    &lastSeq,
		In:         in,
		Out:        out,
	})
	if err == nil {
		t.Fatal("truncated snapshot was accepted")
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
	if result.LastSeq != lastSeq {
		t.Fatalf("last sequence = %d, want previous valid sequence %d", result.LastSeq, lastSeq)
	}
}
