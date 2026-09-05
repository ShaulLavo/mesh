package worker

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
)

func recoveryListenerFixture(t *testing.T, reply func(net.Conn)) RecoveryRuntime {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "7K3D")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteMeta(dir, Meta{ID: "7K3D", State: StateRunning, BootID: BootID(), Cwd: root, Command: []string{"/bin/sh"}}); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", paths.Socket(dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer connection.Close() //nolint:errcheck // one bounded test response
		reply(connection)
	}()
	return RecoveryRuntime{SessionsDir: root, HostID: "test-host"}
}

func TestRecoveryLivenessRequiresAWorkerResponse(t *testing.T) {
	accepted := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	runtime := recoveryListenerFixture(t, func(conn net.Conn) {
		close(accepted)
		_, _ = protocol.NewReader(conn).ReadFrame()
		<-release
	})
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	_, err := recovery.Recover(ctx, recovery.Config{SessionsDir: runtime.SessionsDir, HostID: runtime.HostID,
		Runtime: runtime, BootID: BootID()}, recovery.Request{SessionID: "7K3D"})
	if err == nil {
		t.Fatal("a socket that never answered was treated as a living worker")
	}
	<-accepted
	entries, err := os.ReadDir(runtime.SessionsDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("inconclusive listener created a replacement: %v %v", entries, err)
	}
}

func TestRecoveryLivenessClosedConnectionMeansInterrupted(t *testing.T) {
	runtime := recoveryListenerFixture(t, func(conn net.Conn) {
		_, _ = protocol.NewReader(conn).ReadFrame()
	})
	source, err := runtime.Inspect(context.Background(), "7K3D")
	if err != nil || source.State != StateInterrupted {
		t.Fatalf("closed worker connection = %+v %v", source, err)
	}
}

func TestRecoveryLivenessAcceptsExactAndLegacyReplies(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "exact", true: "legacy"}[legacy], func(t *testing.T) { testAnsweredRecoveryWorker(t, legacy) })
	}
}

func testAnsweredRecoveryWorker(t *testing.T, legacy bool) {
	t.Helper()
	runtime := recoveryListenerFixture(t, func(conn net.Conn) {
		_, _ = protocol.NewReader(conn).ReadFrame()
		response := protocol.Control{Type: protocol.TypeContained, SessionID: "7K3D",
			ContainingSessions: []protocol.SessionIdentity{{HostID: "test-host", SessionID: "7K3D"}}}
		if legacy {
			response = protocol.Control{Type: protocol.TypeError, SessionID: "7K3D", Message: "expected session.attach"}
		}
		_ = protocol.NewWriter(conn).WriteControlMsg(response)
	})
	source, err := runtime.Inspect(context.Background(), "7K3D")
	if err != nil || source.State != StateRunning {
		t.Fatalf("answering worker = %+v %v", source, err)
	}
}

func TestRecoveryLivenessWrongHostAndMalformedRepliesStayInconclusive(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		t.Run(map[bool]string{false: "wrong-host", true: "malformed"}[malformed], func(t *testing.T) { testInvalidRecoveryReply(t, malformed) })
	}
}

func testInvalidRecoveryReply(t *testing.T, malformed bool) {
	t.Helper()
	runtime := recoveryListenerFixture(t, func(conn net.Conn) {
		_, _ = protocol.NewReader(conn).ReadFrame()
		if malformed {
			_ = protocol.NewWriter(conn).WriteControl([]byte("not json"))
			return
		}
		_ = protocol.NewWriter(conn).WriteControlMsg(protocol.Control{Type: protocol.TypeContained, SessionID: "7K3D",
			ContainingSessions: []protocol.SessionIdentity{{HostID: "another-host", SessionID: "7K3D"}}})
	})
	if source, err := runtime.Inspect(context.Background(), "7K3D"); err == nil {
		t.Fatalf("invalid worker response declared liveness or interruption: %+v", source)
	}
}
