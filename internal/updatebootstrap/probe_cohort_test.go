package updatebootstrap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/worker"
)

// Twenty-four independent 250 ms observations exceed Inspect's five seconds
// when serialized. These sockets still use the production native image probe.
func TestInspectDelayedWorkerCohort(t *testing.T) {
	for _, count := range []int{24, 40} {
		t.Run(strconv.Itoa(count), func(t *testing.T) { inspectDelayedWorkerCohort(t, count) })
	}
}

func inspectDelayedWorkerCohort(t *testing.T, count int) {
	original := workerImages
	var lookups atomic.Int32
	workerImages = func(ctx context.Context, pids []int) []processImageResult {
		lookups.Add(1)
		return original(ctx, pids)
	}
	t.Cleanup(func() { workerImages = original })
	state := probeFixtureDir(t)
	createLegacyDatabase(t, state)
	build := release.Current()
	serveProbeResponse(t, filepath.Join(state, "daemon.sock"), protocol.Control{
		Type: protocol.TypeHostInfoResult, Host: &protocol.HostInfo{ID: "cohort-host", Build: &build},
	}, 0, nil, nil)
	var active, peak atomic.Int32
	for index := range count {
		id := fmt.Sprintf("W%03d", index)
		dir := cohortWorkerDir(t, state, id, build)
		serveProbeResponse(t, filepath.Join(dir, "sock"), cohortResponse(id), 250*time.Millisecond, &active, &peak)
	}
	started := time.Now()
	observed, err := Inspect(t.Context(), state)
	if err != nil {
		t.Fatalf("delayed cohort after %s: %v", time.Since(started), err)
	}
	if len(observed.Health.Workers) != count {
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
	if got := lookups.Load(); got != int32((count+31)/32) { //nolint:gosec // fixture count is bounded to 40
		t.Fatalf("worker image lookups = %d, want %d", got, (count+31)/32)
	}
	t.Logf("native worker observations in %s; peak socket concurrency %d", time.Since(started), peak.Load())
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
	awaitCohortConnections(t, entered, 8)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled cohort = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled cohort did not join within one second")
	}
	awaitCohortConnections(t, closed, 8)
	if len(entered) != 0 {
		t.Fatal("canceled cohort started more observations")
	}
}

func awaitCohortConnections(t *testing.T, events <-chan struct{}, count int) {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for range count {
		select {
		case <-events:
		case <-timer.C:
			t.Fatalf("missing connection event, wanted %d", count)
		}
	}
}

func TestWorkerCohortRejectsEvidenceInDirectoryOrder(t *testing.T) {
	state := probeFixtureDir(t)
	build := release.Current()
	for index := range 12 {
		id := fmt.Sprintf("W%03d", index)
		current := build
		if index == 0 || index == 9 {
			current.Digest = strings.Repeat("0", 64)
		}
		dir := cohortWorkerDir(t, state, id, current)
		// The later invalid worker answers first; the error must name W000.
		delay := time.Duration(12-index) * 10 * time.Millisecond
		serveProbeResponse(t, filepath.Join(dir, "sock"), cohortResponse(id), delay, nil, nil)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	workers, err := inspectWorkers(ctx, state, 7)
	if len(workers) != 0 || err == nil || !strings.Contains(err.Error(), "verify session W000") || !strings.Contains(err.Error(), "reported build differs") {
		t.Fatalf("invalid cohort = %+v, %v", workers, err)
	}
}

func TestWorkerCohortPreservesStaleAndUnknownDistinction(t *testing.T) {
	state := probeFixtureDir(t)
	build := release.Current()
	stale := cohortWorkerDir(t, state, "stale", build)
	if err := worker.WriteMeta(stale, worker.Meta{ID: "stale", State: worker.StateRunning, BootID: "different-boot"}); err != nil {
		t.Fatal(err)
	}
	_ = cohortWorkerDir(t, state, "dead", build)
	live := cohortWorkerDir(t, state, "live", build)
	serveProbeResponse(t, filepath.Join(live, "sock"), cohortResponse("live"), 0, nil, nil)
	unknown := cohortWorkerDir(t, state, "unknown", build)
	serveProbeResponse(t, filepath.Join(unknown, "sock"), protocol.Control{
		Type: protocol.TypeError, SessionID: "unknown", Message: "expected " + protocol.TypeAttach,
	}, 0, nil, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	workers, err := inspectWorkers(ctx, state, 7)
	if len(workers) != 0 || err == nil || !strings.Contains(err.Error(), "cannot answer a read-only session inspection") {
		t.Fatalf("unrecognized modern worker = %+v, %v", workers, err)
	}
}

func TestLegacyVersionProbeHonorsParentCancellation(t *testing.T) {
	dir := probeFixtureDir(t)
	path, pidPath := filepath.Join(dir, "mesh"), filepath.Join(dir, "pid")
	// exec keeps the fixture to one process, which CommandContext must reap.
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s' \"$$\" > \"$MESH_PROBE_VERSION_PID\"\nexec sleep 10\n"), 0700); err != nil { //nolint:gosec // isolated executable version fixture
		t.Fatal(err)
	}
	t.Setenv("MESH_PROBE_VERSION_PID", pidPath)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	if version := legacyVersionFlag(ctx, path); version != "" || time.Since(started) > time.Second {
		t.Fatalf("canceled version probe = %q after %s", version, time.Since(started))
	}
	data, err := os.ReadFile(pidPath) //nolint:gosec // fixed fixture output under this test's directory
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil || pid <= 0 {
		t.Fatalf("fixture process ID = %q: %v", data, err)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("version probe did not reap its process: %v", err)
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
	closed := make(chan struct{})
	finish := context.AfterFunc(ctx, func() { _ = conn.Close(); close(closed) })
	defer func() {
		if !finish() {
			<-closed
		}
	}()
	if _, err := protocol.NewReader(conn).ReadFrame(); err != nil {
		return
	}
	respond(conn, stop)
}
