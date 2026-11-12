package updatebootstrap

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/worker"
)

// Twenty-four independent 250 ms observations exceed Inspect's five seconds
// when serialized. These sockets still use the production native image probe.
func TestInspectDelayedWorkerCohort(t *testing.T) {
	state := probeFixtureDir(t)
	createLegacyDatabase(t, state)
	build := release.Current()
	serveProbeResponse(t, filepath.Join(state, "daemon.sock"), protocol.Control{
		Type: protocol.TypeHostInfoResult, Host: &protocol.HostInfo{ID: "cohort-host", Build: &build},
	}, 0, nil, nil)
	var active, peak atomic.Int32
	for index := range 24 {
		id := fmt.Sprintf("W%03d", index)
		dir := cohortWorkerDir(t, state, id, build)
		serveProbeResponse(t, filepath.Join(dir, "sock"), cohortResponse(id), 250*time.Millisecond, &active, &peak)
	}
	started := time.Now()
	observed, err := Inspect(t.Context(), state)
	if err != nil {
		t.Fatalf("delayed cohort after %s: %v", time.Since(started), err)
	}
	if len(observed.Health.Workers) != 24 {
		t.Fatalf("workers = %d, want every worker", len(observed.Health.Workers))
	}
	for index, current := range observed.Health.Workers {
		want := fmt.Sprintf("W%03d", index)
		if current.ID != want || current.PID != os.Getpid() || current.ShellPID != 123 || current.Protocol != build.WorkerProtocol || current.Build == nil || current.Build.Digest != build.Digest {
			t.Fatalf("worker %d = %+v", index, current)
		}
	}
	if got := peak.Load(); got < 2 || got > 8 {
		t.Fatalf("concurrent socket observations = %d, want 2 through 8", got)
	}
	if active.Load() != 0 {
		t.Fatal("socket response handlers still active after inspection")
	}
	t.Logf("24 native worker observations in %s; peak socket concurrency %d", time.Since(started), peak.Load())
}

func TestWorkerCohortCancellationJoinsConnections(t *testing.T) {
	state := probeFixtureDir(t)
	build := release.Current()
	entered := make(chan struct{}, 16)
	closed := make(chan struct{}, 16)
	for index := range 16 {
		dir := cohortWorkerDir(t, state, fmt.Sprintf("W%03d", index), build)
		serveStalledProbe(t, filepath.Join(dir, "sock"), entered, closed)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := inspectWorkers(ctx, state, 7)
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("no worker inspection connected")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled cohort accepted partial observations")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled cohort did not join within one second")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("canceled inspection retained its connected socket")
	}
}

func cohortWorkerDir(t *testing.T, state, id string, build release.Build) string {
	t.Helper()
	dir := filepath.Join(state, "s", id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	meta := worker.Meta{ID: id, PID: 123, State: worker.StateRunning, BootID: worker.BootID(), Build: &build}
	if err := worker.WriteMeta(dir, meta); err != nil {
		t.Fatal(err)
	}
	return dir
}

func cohortResponse(id string) protocol.Control {
	return protocol.Control{Type: protocol.TypeInspected, RequestID: "bootstrap-worker", SessionID: id,
		Inspection: &protocol.SessionInspection{ObservedAt: time.Now()}}
}

func serveProbeResponse(t *testing.T, socket string, response protocol.Control, delay time.Duration, active, peak *atomic.Int32) {
	t.Helper()
	serveCohortProbe(t, socket, func(conn net.Conn, stop <-chan struct{}) {
		if active != nil {
			count := active.Add(1)
			defer active.Add(-1)
			updateProbePeak(peak, count)
		}
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
			_ = protocol.NewWriter(conn).WriteControlMsg(response)
		case <-stop:
		}
	})
}

func updateProbePeak(peak *atomic.Int32, count int32) {
	for previous := peak.Load(); count > previous; previous = peak.Load() {
		if peak.CompareAndSwap(previous, count) {
			return
		}
	}
}

func serveStalledProbe(t *testing.T, socket string, entered, closed chan<- struct{}) {
	t.Helper()
	serveCohortProbe(t, socket, func(conn net.Conn, _ <-chan struct{}) {
		entered <- struct{}{}
		_, _ = io.Copy(io.Discard, conn)
		closed <- struct{}{}
	})
}

func serveCohortProbe(t *testing.T, socket string, respond func(net.Conn, <-chan struct{})) {
	t.Helper()
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	stop, done := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(func() {
		cancel()
		close(stop)
		_ = listener.Close()
		<-done
	})
	go func() {
		defer close(done)
		serveCohortConnection(ctx, listener, stop, respond)
	}()
}

func serveCohortConnection(ctx context.Context, listener net.Listener, stop <-chan struct{}, respond func(net.Conn, <-chan struct{})) {
	conn, err := listener.Accept()
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	finish := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer finish()
	if _, err := protocol.NewReader(conn).ReadFrame(); err != nil {
		return
	}
	respond(conn, stop)
}
