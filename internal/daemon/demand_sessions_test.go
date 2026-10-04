package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	meshserve "github.com/shaul/mesh/internal/serve"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/worker"
)

func labelledTestLifecycle(t *testing.T, catalog *lifecycleTestCatalog, launch launchWorker) *lifecycle {
	t.Helper()
	return mustLifecycle(t, lifecycleConfig{
		Catalog:     catalog,
		Connector:   failingLifecycleConnector(),
		Host:        storage.Host{ID: "host-a", MeshIdentity: "mesh-key", LastSeenAt: time.Now()},
		SessionsDir: compactSocketTempDir(t),
		Launch:      launch,
	})
}

func TestLabelledPublicationFailureKeepsWorkerIdentity(t *testing.T) {
	publishErr := errors.New("catalog is read-only")
	lifecycle := labelledTestLifecycle(t, &lifecycleTestCatalog{reconcileErr: publishErr}, func(worker.LaunchConfig) (worker.Launched, error) {
		return worker.Launched{Meta: worker.Meta{ID: "7K3D"}}, nil
	})
	id, err := lifecycle.startLabelled(context.Background(), "serve /dev", []string{"sh", "-lc", "vite"}, "/work", nil)
	if !errors.Is(err, publishErr) {
		t.Fatalf("startLabelled error = %v, want the publication failure", err)
	}
	if id != "7K3D" {
		t.Fatalf("launched worker 7K3D was discarded on publication failure (got %q)", id)
	}
}

func TestLabelledLaunchFailureOwnsNoWorker(t *testing.T) {
	launchErr := errors.New("fork: resource temporarily unavailable")
	lifecycle := labelledTestLifecycle(t, &lifecycleTestCatalog{}, func(worker.LaunchConfig) (worker.Launched, error) {
		return worker.Launched{}, launchErr
	})
	id, err := lifecycle.startLabelled(context.Background(), "serve /dev", []string{"sh", "-lc", "vite"}, "/work", nil)
	if !errors.Is(err, launchErr) || id != "" {
		t.Fatalf("startLabelled = %q, %v; want no session and the launch failure", id, err)
	}
}

func TestSessionExitDoesNotTreatUnreadableMetadataAsEnded(t *testing.T) {
	lifecycle := labelledTestLifecycle(t, &lifecycleTestCatalog{}, nil)
	dir := filepath.Join(lifecycle.sessionsDir, "7K3D")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Meta(dir), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ended := lifecycle.sessionExit("7K3D"); ended {
		t.Fatal("unreadable metadata certified that session 7K3D ended")
	}
	if _, ended := lifecycle.sessionExit("8M4F"); ended {
		t.Fatal("missing metadata certified that session 8M4F ended")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := lifecycle.stopSession(ctx, "7K3D"); err == nil {
		t.Fatal("stopSession reported session 7K3D stopped without reaching it")
	}
}

// fakeOwnedWorker answers a worker's socket in dir: it acknowledges a kill
// after recording the exit, and reports each request type it saw.
func fakeOwnedWorker(t *testing.T, dir, id string) <-chan string {
	t.Helper()
	if err := worker.WriteMeta(dir, worker.Meta{ID: id, State: worker.StateRunning, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", paths.Socket(dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	requests := make(chan string, 16)
	go func() {
		for {
			stream, err := listener.Accept()
			if err != nil {
				return
			}
			answerOwnedWorker(dir, id, stream, requests)
		}
	}()
	return requests
}

func answerOwnedWorker(dir, id string, stream net.Conn, requests chan<- string) {
	conn, err := transport.NewStreamConn(stream)
	if err != nil {
		_ = stream.Close()
		return
	}
	defer conn.Close() //nolint:errcheck // a test worker's close result is irrelevant
	frame, err := conn.ReadFrame()
	if err != nil {
		return
	}
	request, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return
	}
	requests <- request.Type
	if request.Type != protocol.TypeKill {
		return
	}
	code := 137
	now := time.Now()
	_ = worker.WriteMeta(dir, worker.Meta{ID: id, State: worker.StateExited, CreatedAt: now, ExitedAt: &now, ExitCode: &code})
	payload, err := protocol.Control{Type: protocol.TypeOK, RequestID: request.RequestID, SessionID: id}.Encode()
	if err == nil {
		_ = conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload})
	}
}

// unpublishedLifecycle launches fake workers through the production connector
// over a catalog that can neither publish nor find them.
func unpublishedLifecycle(t *testing.T) (*lifecycle, map[string]<-chan string) {
	t.Helper()
	sessionsDir := compactSocketTempDir(t)
	catalog := &lifecycleTestCatalog{reconcileErr: errors.New("catalog is read-only")}
	connector, err := newWorkerConnector(sessionsDir, catalog)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{"7K3D", "8M4F", "9P5G"}
	workers := map[string]<-chan string{}
	lifecycle := mustLifecycle(t, lifecycleConfig{
		Catalog:     catalog,
		Connector:   connector,
		Host:        storage.Host{ID: "host-a", MeshIdentity: "mesh-key", LastSeenAt: time.Now()},
		SessionsDir: sessionsDir,
		Launch: func(worker.LaunchConfig) (worker.Launched, error) {
			id := ids[len(workers)]
			dir := filepath.Join(sessionsDir, id)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return worker.Launched{}, fmt.Errorf("fake worker %s: %w", id, err)
			}
			workers[id] = fakeOwnedWorker(t, dir, id)
			meta, err := worker.ReadMeta(dir)
			if err != nil {
				return worker.Launched{}, fmt.Errorf("fake worker %s: %w", id, err)
			}
			return worker.Launched{Meta: meta, Dir: dir}, nil
		},
	})
	return lifecycle, workers
}

func TestOwnedStopReachesAnUnpublishedWorker(t *testing.T) {
	lifecycle, workers := unpublishedLifecycle(t)
	id, err := lifecycle.startLabelled(context.Background(), "serve /dev", []string{"sh", "-lc", "vite"}, "/work", nil)
	if err == nil || id != "7K3D" {
		t.Fatalf("startLabelled = %q, %v; want 7K3D with a publication failure", id, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := lifecycle.stopSession(ctx, id); err != nil {
		t.Fatalf("stopping unpublished worker %s: %v", id, err)
	}
	if got := <-workers[id]; got != protocol.TypeKill {
		t.Fatalf("worker received %q, want a kill", got)
	}
}

func TestDemandStopsAnUnpublishedWorkerThroughItsSocket(t *testing.T) {
	lifecycle, workers := unpublishedLifecycle(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := newDemandManager(ctx, lifecycle, func(error) {})
	manager.poll = 5 * time.Millisecond
	manager.bind = func(uint16) (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") }
	var upstream atomic.Bool
	manager.dial = func(context.Context, string) error {
		if upstream.Load() {
			return nil
		}
		return syscall.ECONNREFUSED
	}
	t.Cleanup(manager.Close)
	service := demandService(time.Minute)
	service.Demand.ReadyTimeout = 50 * time.Millisecond
	manager.Sync([]meshserve.Service{service})

	if err := manager.Start(context.Background(), "dev"); err == nil || !strings.Contains(err.Error(), "stopped it") {
		t.Fatalf("Start = %v, want a ready timeout whose cleanup reached the unpublished worker", err)
	}
	upstream.Store(true)
	if err := manager.Start(context.Background(), "dev"); err != nil {
		t.Fatalf("restart after a clean stop: %v", err)
	}
	if err := manager.Stop(context.Background(), "dev"); err != nil {
		t.Fatalf("stopping the unpublished running session: %v", err)
	}
	for _, id := range []string{"7K3D", "8M4F"} {
		if meta, err := worker.ReadMeta(filepath.Join(lifecycle.sessionsDir, id)); err != nil || meta.State != worker.StateExited {
			t.Fatalf("session %s was not stopped: %+v, %v", id, meta, err)
		}
	}
	if len(workers) != 2 {
		t.Fatalf("launched %d workers, want 2", len(workers))
	}
}

func TestLabelledLaunchFailureBeforeStartOwnsNoWorker(t *testing.T) {
	sessionsDir := t.TempDir()
	lifecycle := mustLifecycle(t, lifecycleConfig{
		Catalog:     &lifecycleTestCatalog{},
		Connector:   failingLifecycleConnector(),
		Host:        storage.Host{ID: "host-a", MeshIdentity: "mesh-key", LastSeenAt: time.Now()},
		SessionsDir: sessionsDir,
		Executable:  filepath.Join(t.TempDir(), "missing-mesh"),
	})
	id, err := lifecycle.startLabelled(context.Background(), "serve /dev", []string{"sh", "-lc", "vite"}, t.TempDir(), nil)
	if err == nil || id != "" {
		t.Fatalf("startLabelled = %q, %v; want no session when the process never started", id, err)
	}
}
