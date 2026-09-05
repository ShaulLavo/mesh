package worker

import (
	"net"
	"os/exec"
	"testing"
	"testing/synctest"
	"time"

	"github.com/shaul/mesh/internal/protocol"
)

func TestFinishWaitsForKillAcknowledgment(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, finished := startBlockedKillAcknowledgment(t)
		select {
		case <-finished:
			t.Fatal("worker finished before its kill acknowledgment was written")
		default:
		}
		frame, err := protocol.NewReader(client).ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		message, err := protocol.DecodeControl(frame.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if frame.Kind != protocol.KindControl || message.Type != protocol.TypeOK || message.RequestID != "kill-ack" || message.SessionID != "KILL" {
			t.Fatalf("kill acknowledgment = kind %v, message %+v", frame.Kind, message)
		}
		synctest.Wait()
		select {
		case <-finished:
		default:
			t.Fatal("worker did not finish after its kill acknowledgment was written")
		}
	})
}

func TestFinishBoundsUnreadKillAcknowledgment(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, finished := startBlockedKillAcknowledgment(t)
		time.Sleep(attachmentWriteTimeout + time.Millisecond)
		synctest.Wait()
		select {
		case <-finished:
		default:
			t.Fatal("unread kill acknowledgment held worker shutdown beyond its write deadline")
		}
	})
}

func startBlockedKillAcknowledgment(t *testing.T) (net.Conn, <-chan struct{}) {
	t.Helper()
	w := &Worker{cfg: Config{ID: "KILL"}, cmd: &exec.Cmd{}, exited: make(chan struct{})}
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	go w.serve(server)
	if err := protocol.NewWriter(client).WriteControlMsg(protocol.Control{
		Type: protocol.TypeKill, RequestID: "kill-ack", SessionID: "KILL",
	}); err != nil {
		t.Fatal(err)
	}
	synctest.Wait()
	finished := make(chan struct{})
	go func() {
		w.finish(0)
		close(finished)
	}()
	synctest.Wait()
	return client, finished
}
