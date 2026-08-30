package cli

import (
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/worker"
)

func TestKillWaitsForWorkerAcknowledgement(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close() //nolint:errcheck // test resource cleanup

	received := make(chan protocol.Control, 1)
	release := make(chan struct{})
	serverErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close() //nolint:errcheck // test resource cleanup
		frame, err := protocol.NewReader(conn).ReadFrame()
		if err != nil {
			serverErr <- err
			return
		}
		request, err := protocol.DecodeControl(frame.Payload)
		if err != nil {
			serverErr <- err
			return
		}
		received <- request
		<-release
		serverErr <- protocol.NewWriter(conn).WriteControlMsg(protocol.Control{
			Type:      protocol.TypeOK,
			RequestID: request.RequestID,
			SessionID: request.SessionID,
		})
	}()

	result := make(chan error, 1)
	go func() {
		result <- Kill(Session{
			Meta: worker.Meta{ID: "7K3D"},
			Dir:  dir,
		})
	}()
	request := <-received
	if request.Type != protocol.TypeKill || request.SessionID != "7K3D" {
		t.Fatalf("kill request = %+v", request)
	}
	select {
	case err := <-result:
		t.Fatalf("Kill returned before acknowledgement: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Kill did not return after acknowledgement")
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}
