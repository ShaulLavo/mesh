package worker

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/testenv"
)

// Input that reaches the worker after its command exited must not close the
// attachment before the exit frame is written. A slow client keeps output
// queued ahead of the exit frame, which is when a reader-side close would
// discard it.
func TestWorkerDeliversExitFrameDespiteInputAfterExit(t *testing.T) {
	root := testenv.SocketTempDir(t)
	dir := filepath.Join(root, "s")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stop := filepath.Join(root, "stop")
	done := make(chan error, 1)
	go func() {
		_, err := Run(Config{
			ID: "7K3D", Dir: dir, Cwd: root, Cols: 80, Rows: 24,
			Env: []string{"PATH=/usr/bin:/bin", "TERM=xterm-256color", "LANG=C"},
			Command: []string{"/bin/sh", "-c",
				`head -c 1048576 /dev/zero | tr '\0' x; while [ ! -e "$1" ]; do sleep 0.01; done; exit 7`, "late-input", stop},
		})
		done <- err
	}()
	t.Cleanup(func() { _ = os.WriteFile(stop, nil, 0o600) })
	socketPath := paths.Socket(dir)
	conn := dialWorkerWhenReady(t, socketPath)
	defer conn.Close() //nolint:errcheck // test connection cleanup
	_ = conn.SetDeadline(time.Now().Add(time.Minute))
	if err := protocol.NewWriter(conn).WriteControlMsg(protocol.Control{Type: protocol.TypeAttach, SessionID: "7K3D"}); err != nil {
		t.Fatal(err)
	}

	// The client reads nothing, so the writer stays blocked on queued output.
	if err := os.WriteFile(stop, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	input := protocol.NewWriter(conn)
	sid, err := protocol.NewSessionID("7K3D")
	if err != nil {
		t.Fatal(err)
	}
	// Keep sending past the exit. A failed write only means the worker already
	// closed the connection, which the read below judges.
	for range 120 {
		time.Sleep(25 * time.Millisecond)
		if err := input.WriteInput(sid, []byte("late\n")); err != nil {
			break
		}
	}

	reader := protocol.NewReader(conn)
	for {
		frame, err := reader.ReadFrame()
		if err != nil {
			t.Fatalf("connection ended without the exit frame: %v", err)
		}
		if frame.Kind != protocol.KindControl {
			continue
		}
		message, err := protocol.DecodeControl(frame.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if message.Type != protocol.TypeExit {
			continue
		}
		if message.ExitCode == nil || *message.ExitCode != 7 {
			t.Fatalf("exit frame = %+v, want status 7", message)
		}
		break
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("worker: %v", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("worker did not stop")
	}
}

func dialWorkerWhenReady(t *testing.T, socketPath string) net.Conn {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		conn, err := net.Dial("unix", socketPath)
		if err == nil {
			return conn
		}
		if time.Now().After(deadline) {
			t.Fatalf("dial worker socket: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
