package updatebootstrap

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/testenv"
	"github.com/shaul/mesh/internal/worker"
)

func TestProbeMissingDaemonNamesDialPhase(t *testing.T) {
	_, err := Inspect(t.Context(), probeFixtureDir(t))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing daemon = %v, want ENOENT", err)
	}
	if !strings.Contains(err.Error(), "verify daemon phase=dial") {
		t.Fatalf("missing daemon has no probe target/phase: %v", err)
	}
}

func TestProbeWorkerResponseDeadlineNamesConnectedProcess(t *testing.T) {
	probeWorkerResponse(t, false)
}

func TestProbeWorkerResponseSucceedsWithoutChangingMetadata(t *testing.T) {
	probeWorkerResponse(t, true)
}

func probeFixtureDir(t *testing.T) string {
	t.Helper()
	return testenv.SocketTempDir(t)
}

func probeWorkerResponse(t *testing.T, respond bool) {
	t.Helper()
	dir := filepath.Join(probeFixtureDir(t), "7K3D")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	build := release.Current()
	meta := worker.Meta{ID: "7K3D", PID: 123, State: worker.StateRunning, BootID: worker.BootID(), Build: &build}
	if err := worker.WriteMeta(dir, meta); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "meta.json")) //nolint:gosec // fixed name beneath the isolated fixture
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(dir, "sock"))
	if err != nil {
		t.Fatal(err)
	}
	stop, done := make(chan struct{}), make(chan error, 1)
	t.Cleanup(func() {
		close(stop)
		_ = listener.Close()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer func() { _ = conn.Close() }()
		frame, err := protocol.NewReader(conn).ReadFrame()
		if err != nil {
			done <- err
			return
		}
		request, err := protocol.DecodeControl(frame.Payload)
		if err != nil || request.Type != protocol.TypeInspect || request.SessionID != "" {
			done <- fmt.Errorf("unexpected fixture request: %+v %w", request, err)
			return
		}
		if respond {
			done <- protocol.NewWriter(conn).WriteControlMsg(protocol.Control{
				Type: protocol.TypeInspected, RequestID: request.RequestID, SessionID: meta.ID,
				Inspection: &protocol.SessionInspection{ObservedAt: time.Now()},
			})
			return
		}
		<-stop
		done <- nil
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	observed, err := inspectWorker(ctx, dir, build.StateVersion)
	after, readErr := os.ReadFile(filepath.Join(dir, "meta.json")) //nolint:gosec // the same fixture is compared after the probe
	if readErr != nil || string(after) != string(before) {
		t.Fatalf("probe changed metadata: %v", readErr)
	}
	if respond {
		if err != nil || observed == nil || observed.PID != os.Getpid() || observed.ShellPID != meta.PID {
			t.Fatalf("healthy worker observation = %+v %v", observed, err)
		}
		return
	}
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() || observed != nil {
		t.Fatalf("stalled worker = %+v %v, want preserved socket timeout", observed, err)
	}
	for _, fact := range []string{"verify session 7K3D", fmt.Sprintf("workerPID=%d", os.Getpid()), "shellPID=123", "phase=read-response"} {
		if !strings.Contains(err.Error(), fact) {
			t.Errorf("stalled worker lacks %q: %v", fact, err)
		}
	}
}
