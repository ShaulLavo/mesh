package cli

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/shaul/mesh/internal/protocol"
)

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
