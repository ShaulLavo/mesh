package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/worker"
)

type appOwnershipSocketWorker struct {
	dir      string
	id       string
	listener *net.UnixListener
	requests chan string
	done     chan struct{}
	err      error
}

func newAppOwnershipSocketWorker(t *testing.T, sessionsDir, label string) *appOwnershipSocketWorker {
	t.Helper()
	w := &appOwnershipSocketWorker{dir: filepath.Join(sessionsDir, "7K3D"), id: "7K3D", requests: make(chan string, 1), done: make(chan struct{})}
	if err := os.MkdirAll(w.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := worker.WriteMeta(w.dir, worker.Meta{ID: w.id, Label: label, State: worker.StateRunning, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: paths.Socket(w.dir), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	w.listener = listener
	t.Cleanup(func() { w.closeAndJoin(t) })
	if err := listener.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		_ = listener.Close()
		close(w.done)
		t.Fatal(err)
	}
	go func() {
		w.err = w.serve()
		close(w.done)
	}()
	return w
}

func (w *appOwnershipSocketWorker) closeAndJoin(t *testing.T) {
	t.Helper()
	_ = w.listener.Close()
	select {
	case <-w.done:
		if w.err != nil && !errors.Is(w.err, net.ErrClosed) {
			t.Errorf("fixture worker: %v", w.err)
		}
		t.Log("cleanup closed the fixture listener and joined its accept/handler goroutine")
	case <-time.After(5 * time.Second):
		t.Error("fixture worker did not join within five seconds")
	}
}

func (w *appOwnershipSocketWorker) serve() error {
	stream, err := w.listener.AcceptUnix()
	if err != nil {
		return fmt.Errorf("accept fixture socket: %w", err)
	}
	defer func() { _ = stream.Close() }()
	if err := stream.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		return fmt.Errorf("set fixture socket deadline: %w", err)
	}
	conn, err := transport.NewStreamConn(stream)
	if err != nil {
		return fmt.Errorf("adapt fixture stream: %w", err)
	}
	frame, err := conn.ReadFrame()
	if err != nil {
		return fmt.Errorf("read fixture request: %w", err)
	}
	request, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return fmt.Errorf("decode fixture control: %w", err)
	}
	w.requests <- request.Type
	if request.Type != protocol.TypeKill {
		return errors.New("fixture expected a kill request")
	}
	code, now := 137, time.Now()
	if err := worker.WriteMeta(w.dir, worker.Meta{ID: w.id, State: worker.StateExited, CreatedAt: now, ExitedAt: &now, ExitCode: &code}); err != nil {
		return fmt.Errorf("write fixture exit metadata: %w", err)
	}
	payload, err := (protocol.Control{Type: protocol.TypeOK, RequestID: request.RequestID, SessionID: w.id}).Encode()
	if err != nil {
		return fmt.Errorf("encode fixture acknowledgement: %w", err)
	}
	if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload}); err != nil {
		return fmt.Errorf("send fixture acknowledgement: %w", err)
	}
	return nil
}

func TestAppWorkersPreservePostLaunchIdentity(t *testing.T) {
	sessionsDir := compactSocketTempDir(t)
	publicationErr := errors.New("synthetic catalog publication failure")
	catalog := &lifecycleTestCatalog{reconcileErr: publicationErr}
	connector, err := newWorkerConnector(sessionsDir, catalog)
	if err != nil {
		t.Fatal(err)
	}
	var fixture *appOwnershipSocketWorker
	lifecycle := mustLifecycle(t, lifecycleConfig{
		Catalog: catalog, Connector: connector,
		Host: storage.Host{ID: "host-a", MeshIdentity: "mesh-key", LastSeenAt: time.Now()}, SessionsDir: sessionsDir,
		Launch: func(cfg worker.LaunchConfig) (worker.Launched, error) {
			fixture = newAppOwnershipSocketWorker(t, sessionsDir, cfg.Label)
			meta, err := worker.ReadMeta(fixture.dir)
			if err != nil {
				return worker.Launched{}, fmt.Errorf("read fixture launch metadata: %w", err)
			}
			return worker.Launched{Meta: meta, Dir: fixture.dir}, nil
		},
	})
	adapter := appWorkers{lifecycle: lifecycle}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := adapter.Start(ctx, "app-setup synthetic", "synthetic setup", sessionsDir, nil)
	var publication publicationError
	if id != "7K3D" || !errors.Is(err, publicationErr) || !errors.As(err, &publication) {
		t.Fatalf("appWorkers.Start = %q, %v; want exact ID and publication error", id, err)
	}
	found, alive, err := adapter.Find(ctx, "app-setup synthetic")
	if found != id || !alive || err != nil {
		t.Fatalf("appWorkers.Find = %q, %t, %v; want the exact unpublished worker", found, alive, err)
	}
	if _, ended := lifecycle.sessionExit(id); ended {
		t.Fatal("unpublished fixture worker unexpectedly has an exit record")
	}
	if err := adapter.Stop(ctx, id); err != nil {
		t.Fatalf("direct stop of unpublished worker %s: %v", id, err)
	}
	select {
	case request := <-fixture.requests:
		if request != protocol.TypeKill {
			t.Fatalf("worker received %q, want kill", request)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not receive direct stop within five seconds")
	}
	if _, ended := lifecycle.sessionExit(id); !ended {
		t.Fatal("direct stop did not leave verified exit metadata")
	}
	t.Log("actual appWorkers preserved ID 7K3D plus publication error; Find returned its raw unpublished identity while running; direct Stop delivered TypeKill and recorded exit")
}
