package daemon

import (
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/transport"
)

const testWorkerOperationTimeout = 20 * time.Millisecond

func TestLifecycleBoundsWorkerAcknowledgementWithoutCallerDeadline(t *testing.T) {
	for _, requestType := range []string{protocol.TypeKill, protocol.TypeLogs, protocol.TypeInspect, protocol.TypeHibernate, protocol.TypeSignal} {
		t.Run(requestType, func(t *testing.T) {
			worker := newRelayTestConn()
			t.Cleanup(func() { _ = worker.Close() })
			lifecycle := mustLifecycle(t, lifecycleConfig{
				Catalog:   &lifecycleTestCatalog{sessions: []storage.Session{{ID: "7K3D", State: storage.StateRunning}}},
				Connector: newRelayTestConnector(connectResult{conn: worker}),
				Host:      storage.Host{ID: "host-a", MeshIdentity: "mesh-key"}, SessionsDir: "/state/s",
				OperationTimeout: testWorkerOperationTimeout,
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			request := protocol.Control{Type: requestType, RequestID: "bounded-control", SessionID: "7K3D"}
			switch requestType {
			case protocol.TypeLogs:
				request.Tail = 1
			case protocol.TypeInspect:
				request.PreviewCols, request.PreviewRows = 1, 1
			case protocol.TypeSignal:
				request.Signal = "term"
			}
			if requestType == protocol.TypeSignal {
				_, release := worker.blockWrites()
				defer release()
			}
			result := make(chan error, 1)
			go func() {
				_, _, err := lifecycle.HandleControl(ctx, request)
				result <- err
			}()
			if requestType != protocol.TypeSignal {
				assertRelayFrame(t, worker.nextWrite(t), controlFrame(t, request))
			}
			err := waitRelayError(t, result, "bounded worker acknowledgement")
			if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "7K3D") || !strings.Contains(err.Error(), requestType) {
				t.Fatalf("worker operation error = %v, want session, operation, and deadline", err)
			}
			worker.waitClosed(t)
			select {
			case frame := <-worker.writes:
				t.Fatalf("timeout sent an extra worker frame: %+v", frame)
			default:
			}
		})
	}
}

func TestLifecycleWorkerOperationCancellationClosesOnlyItsSocket(t *testing.T) {
	for _, source := range []string{"caller", "daemon"} {
		t.Run(source, func(t *testing.T) {
			callerCtx, cancelCaller := context.WithCancel(context.Background())
			defer cancelCaller()
			daemonCtx, cancelDaemon := context.WithCancel(context.Background())
			defer cancelDaemon()
			conn := newRelayTestConn()
			t.Cleanup(func() { _ = conn.Close() })
			lifecycle := mustLifecycle(t, lifecycleConfig{
				Context: daemonCtx, Catalog: &lifecycleTestCatalog{},
				Connector: newRelayTestConnector(connectResult{conn: conn}),
				Host:      storage.Host{ID: "host-a", MeshIdentity: "mesh-key"}, SessionsDir: "/state/s",
			})
			result := make(chan error, 1)
			go func() {
				_, _, err := lifecycle.HandleControl(callerCtx, protocol.Control{
					Type: protocol.TypeInspect, RequestID: "cancel-control", SessionID: "7K3D", PreviewCols: 1, PreviewRows: 1,
				})
				result <- err
			}()
			_ = conn.nextWrite(t)
			if source == "caller" {
				cancelCaller()
			} else {
				cancelDaemon()
			}
			if err := waitRelayError(t, result, "canceled worker operation"); !errors.Is(err, context.Canceled) {
				t.Fatalf("operation error = %v, want cancellation", err)
			}
			conn.waitClosed(t)
			select {
			case frame := <-conn.writes:
				t.Fatalf("cancellation sent an extra worker frame: %+v", frame)
			default:
			}
		})
	}
}

func TestLifecycleWorkerOperationTimeoutConfiguration(t *testing.T) {
	cfg := lifecycleConfig{
		Catalog: &lifecycleTestCatalog{}, Connector: failingLifecycleConnector(),
		Host: storage.Host{ID: "host-a", MeshIdentity: "mesh-key"}, SessionsDir: "/state/s",
	}
	lifecycle := mustLifecycle(t, cfg)
	if lifecycle.operationTimeout != defaultWorkerOperationTimeout || lifecycle.operationTimeout <= 5*time.Second {
		t.Fatalf("default operation timeout = %v, want more than worker shutdown grace", lifecycle.operationTimeout)
	}
	cfg.OperationTimeout = -time.Second
	if _, err := newLifecycle(cfg); err == nil {
		t.Fatal("negative worker operation timeout accepted")
	}
}

func TestClientRelayBoundsHandshakeWithoutCallerDeadline(t *testing.T) {
	for _, blocked := range []string{"read", "write"} {
		t.Run(blocked, func(t *testing.T) {
			client, incumbent, candidate := newRelayTestConn(), newRelayTestConn(), newRelayTestConn()
			relay := newClientRelay(client, newRelayTestConnector(connectResult{conn: incumbent}, connectResult{conn: candidate}), defaultWorkerOperationTimeout)
			t.Cleanup(func() { _ = relay.Close() })
			id := mustRelaySessionID(t, "KEEP")
			attachRelaySession(t, relay, client, incumbent, id)
			relay.operationTimeout = testWorkerOperationTimeout
			if blocked == "write" {
				_, release := candidate.blockWrites()
				defer release()
			}
			frame := controlFrame(t, protocol.Control{Type: protocol.TypeAttach, SessionID: id.String()})
			result := make(chan error, 1)
			go func() {
				_, err := relay.HandleFrame(context.Background(), frame)
				result <- err
			}()
			if blocked == "read" {
				assertRelayFrame(t, candidate.nextWrite(t), frame)
			}
			err := waitRelayError(t, result, "bounded worker handshake")
			if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), id.String()) || !strings.Contains(err.Error(), "attach") {
				t.Fatalf("handshake error = %v, want session, operation, and deadline", err)
			}
			candidate.waitClosed(t)
			assertRelayConnOpen(t, incumbent, "handshake timeout closed incumbent")
			input := protocol.Frame{Kind: protocol.KindInput, Session: id, Payload: []byte("still attached")}
			if handled, err := relay.HandleFrame(context.Background(), input); !handled || err != nil {
				t.Fatalf("incumbent input handled = %v, error = %v", handled, err)
			}
			assertRelayFrame(t, incumbent.nextWrite(t), input)
			select {
			case extra := <-candidate.writes:
				t.Fatalf("timeout sent an extra candidate frame: %+v", extra)
			default:
			}
		})
	}
}

func TestClientRelayPublishedStreamOutlivesHandshakeBudget(t *testing.T) {
	client, worker := newRelayTestConn(), newRelayTestConn()
	relay := newClientRelay(client, newRelayTestConnector(connectResult{conn: worker}), testWorkerOperationTimeout)
	t.Cleanup(func() { _ = relay.Close() })
	id := mustRelaySessionID(t, "LIVE")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker.pushRead(controlFrame(t, protocol.Control{Type: protocol.TypeAttached, SessionID: id.String()}))
	if _, err := relay.HandleFrame(ctx, controlFrame(t, protocol.Control{Type: protocol.TypeAttach, SessionID: id.String()})); err != nil {
		t.Fatal(err)
	}
	_ = worker.nextWrite(t)
	_ = client.nextWrite(t)
	cancel()
	timer := time.NewTimer(2 * testWorkerOperationTimeout)
	defer timer.Stop()
	select {
	case <-worker.closed:
		t.Fatal("published stream closed by handshake cancellation")
	case <-timer.C:
	}
	data := protocol.Frame{Kind: protocol.KindData, Session: id, Payload: []byte("after the handshake budget")}
	worker.pushRead(data)
	assertRelayFrame(t, client.nextWrite(t), data)
	input := protocol.Frame{Kind: protocol.KindInput, Session: id, Payload: []byte("still usable")}
	if _, err := relay.HandleFrame(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	assertRelayFrame(t, worker.nextWrite(t), input)
}

func TestClientServerBoundsSilentWorkerOnUnixConnection(t *testing.T) {
	for _, requestType := range []string{protocol.TypeAttach, protocol.TypeInspect} {
		for _, abandon := range []bool{false, true} {
			name := requestType + "/continued"
			if abandon {
				name = requestType + "/abandoned"
			}
			t.Run(name, func(t *testing.T) {
				client, serverConn := workerOperationUnixPair(t)
				workerSocket, silentWorker := net.Pipe()
				t.Cleanup(func() { _ = workerSocket.Close(); _ = silentWorker.Close() })
				workerConn, err := transport.NewStreamConn(workerSocket)
				if err != nil {
					t.Fatal(err)
				}
				connector := newRelayTestConnector(connectResult{conn: workerConn})
				lifecycle := mustLifecycle(t, lifecycleConfig{
					Catalog: &lifecycleTestCatalog{}, Connector: connector,
					Host: storage.Host{ID: "host-a", MeshIdentity: "mesh-key"}, SessionsDir: "/state/s",
					OperationTimeout: testWorkerOperationTimeout,
				})
				server, err := newClientServer(lifecycle, connector, noServiceControl{}, disabledCertificateController{})
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan error, 1)
				go func() { done <- server.Handle(ctx, serverConn) }()
				if err := client.SetDeadline(time.Now().Add(time.Second)); err != nil {
					t.Fatal(err)
				}
				if err := silentWorker.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
					t.Fatal(err)
				}
				request := protocol.Control{Type: requestType, RequestID: "silent-worker", SessionID: "7K3D", PreviewCols: 1, PreviewRows: 1}
				if err := protocol.NewWriter(client).WriteControlMsg(request); err != nil {
					t.Fatal(err)
				}
				reader := protocol.NewReader(silentWorker)
				if _, err := reader.ReadFrame(); err != nil {
					t.Fatal(err)
				}
				if !abandon {
					assertWorkerOperationClientRecovers(t, client, request)
				}
				_ = client.Close()
				if err := waitServerResult(t, done, "silent worker client handler"); err != nil {
					t.Fatal(err)
				}
				if frame, err := reader.ReadFrame(); !errors.Is(err, io.EOF) {
					t.Fatalf("timed-out worker socket got frame %+v, error = %v; want EOF without kill or detach", frame, err)
				}
			})
		}
	}
}

func assertWorkerOperationClientRecovers(t *testing.T, client net.Conn, request protocol.Control) {
	t.Helper()
	reader := protocol.NewReader(client)
	frame, err := reader.ReadFrame()
	if err != nil {
		t.Fatalf("bounded request failed to respond: %v", err)
	}
	response, err := protocol.DecodeControl(frame.Payload)
	if err != nil || response.Type != protocol.TypeError || response.RequestID != request.RequestID || response.SessionID != request.SessionID || !strings.Contains(response.Message, context.DeadlineExceeded.Error()) {
		t.Fatalf("bounded request response = %+v, error = %v", response, err)
	}
	if err := protocol.NewWriter(client).WriteControlMsg(protocol.Control{Type: protocol.TypeHostInfo, RequestID: "after-timeout"}); err != nil {
		t.Fatal(err)
	}
	frame, err = reader.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	response, err = protocol.DecodeControl(frame.Payload)
	if err != nil || response.Type != protocol.TypeHostInfoResult || response.RequestID != "after-timeout" {
		t.Fatalf("later request = %+v, error = %v", response, err)
	}
}

func workerOperationUnixPair(t *testing.T) (net.Conn, transport.Conn) {
	t.Helper()
	path := filepath.Join(compactSocketTempDir(t), "c.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	client, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	accepted, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = accepted.Close() })
	serverConn, err := transport.NewStreamConn(accepted)
	if err != nil {
		t.Fatal(err)
	}
	return client, serverConn
}
