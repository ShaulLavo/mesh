package updatebootstrap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/worker"
)

func TestWorkerGroupCancellationBeforeRequest(t *testing.T) {
	for _, phase := range []string{"identify", "ask"} {
		t.Run(phase, func(t *testing.T) { cancelWorkerGroup(t, phase) })
	}
}

func cancelWorkerGroup(t *testing.T, phase string) {
	state := probeFixtureDir(t)
	accepted, requested, closed := make(chan struct{}, 40), make(chan struct{}, 40), make(chan struct{}, 40)
	for i := range 40 {
		id := fmt.Sprintf("W%03d", i)
		dir := cohortWorkerDir(t, state, id, release.Current())
		serveOwnedProbe(t, filepath.Join(dir, "sock"), id, accepted, requested, closed, true)
	}
	original := workerImages
	t.Cleanup(func() { workerImages = original })
	identifying := make(chan struct{}, 1)
	var nativeActive atomic.Int32
	workerImages = func(ctx context.Context, pids []int) []processImageResult {
		nativeActive.Add(1)
		defer nativeActive.Add(-1)
		if phase != "identify" {
			return original(ctx, pids)
		}
		identifying <- struct{}{}
		<-ctx.Done()
		results := make([]processImageResult, len(pids))
		for i := range results {
			results[i].err = ctx.Err()
		}
		return results
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := inspectWorkers(ctx, state, 7); done <- err }()
	awaitCohortConnections(t, accepted, 32)
	if phase == "identify" {
		awaitCohortConnections(t, identifying, 1)
	}
	if phase == "ask" {
		awaitCohortConnections(t, requested, 8)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stages did not join after cancellation")
	}
	awaitCohortConnections(t, closed, 32)
	if nativeActive.Load() != 0 || len(accepted) != 0 || len(requested) != 0 {
		t.Fatalf("unfinished or later work: native=%d accepted=%d requested=%d", nativeActive.Load(), len(accepted), len(requested))
	}
}

func serveOwnedProbe(t *testing.T, socket, id string, accepted, requested, closed chan<- struct{}, stall bool) {
	t.Helper()
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); _ = listener.Close(); <-done })
	go func() {
		defer close(done)
		serveOwnedConnection(ctx, listener, id, accepted, requested, closed, stall)
	}()
}

func serveOwnedConnection(ctx context.Context, listener net.Listener, id string, accepted, requested, closed chan<- struct{}, stall bool) {
	conn, err := listener.Accept()
	if err != nil {
		return
	}
	defer func() { _ = conn.Close(); closed <- struct{}{} }()
	finish := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer finish()
	accepted <- struct{}{}
	if _, err = protocol.NewReader(conn).ReadFrame(); err != nil {
		return
	}
	requested <- struct{}{}
	if !stall {
		_ = protocol.NewWriter(conn).WriteControlMsg(cohortResponse(id))
	}
	_, _ = io.Copy(io.Discard, conn)
}

func TestIdentifyWorkersMapsDuplicatePIDsAndFailedImage(t *testing.T) {
	original := workerImages
	t.Cleanup(func() { workerImages = original })
	failure := errors.New("unproved image")
	calls := 0
	workerImages = func(_ context.Context, pids []int) []processImageResult {
		calls++
		if !reflect.DeepEqual(pids, []int{22, 11}) {
			t.Fatalf("pids = %v", pids)
		}
		return []processImageResult{{image: executableImage{PID: 22, Path: "second"}}, {err: failure}}
	}
	observations := []workerObservation{{pid: 22, meta: worker.Meta{ID: "a"}}, {pid: 11, meta: worker.Meta{ID: "b"}}, {pid: 22, meta: worker.Meta{ID: "c"}}}
	identifyWorkers(t.Context(), observations)
	if calls != 1 || observations[0].image.PID != 22 || observations[2].image.Path != "second" || !errors.Is(observations[1].err, failure) {
		t.Fatalf("identified = %+v, calls=%d", observations, calls)
	}
	serveIdentifiedWorker(t, &observations[0])
	serveIdentifiedWorker(t, &observations[2])
	runWorkerStage(t.Context(), observations, func(current *workerObservation) { current.ask() })
	if observations[0].err != nil || observations[2].err != nil || !errors.Is(observations[1].err, failure) {
		t.Fatalf("ask results = %+v", observations)
	}
	identifyWorkers(t.Context(), nil)
	if calls != 1 {
		t.Fatal("empty group launched a lookup")
	}
}

func serveIdentifiedWorker(t *testing.T, current *workerObservation) {
	t.Helper()
	client, server := net.Pipe()
	current.conn = client
	done := make(chan struct{})
	t.Cleanup(func() { _ = client.Close(); _ = server.Close(); <-done })
	go func() {
		defer close(done)
		defer func() { _ = server.Close() }()
		if _, err := protocol.NewReader(server).ReadFrame(); err != nil {
			return
		}
		_ = protocol.NewWriter(server).WriteControlMsg(cohortResponse(current.meta.ID))
	}()
}
