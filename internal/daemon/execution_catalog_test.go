package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/worker"
)

// blockingCatalog holds every reconcile until released.
type blockingCatalog struct {
	lifecycleTestCatalog
	release chan struct{}
	started chan struct{}
	once    sync.Once
}

func newBlockingCatalog() *blockingCatalog {
	c := &blockingCatalog{release: make(chan struct{}), started: make(chan struct{}, 16)}
	c.reconcileHook = func(ctx context.Context) error {
		c.started <- struct{}{}
		select {
		case <-c.release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return c
}

func (c *blockingCatalog) unblock() { c.once.Do(func() { close(c.release) }) }

func executionCatalogLifecycle(t *testing.T, catalog lifecycleCatalog) *lifecycle {
	t.Helper()
	l, err := newLifecycle(lifecycleConfig{Catalog: catalog, Connector: lifecycleConnectorFunc(func(context.Context, protocol.SessionID) (transport.Conn, error) { return nil, errors.New("unused") }), Host: storage.Host{ID: "host", MeshIdentity: "host"}, SessionsDir: filepath.Join(t.TempDir(), "s"), Env: []string{"PATH=/bin"}, Launch: func(cfg worker.LaunchConfig) (worker.Launched, error) {
		return worker.Launched{Meta: worker.Meta{Reservation: cfg.Reservation, ID: cfg.Reservation.ID}}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.closeExecution)
	return l
}

// controlWithin runs one control and fails the test if it is still waiting
// at the hang cap, as it would be if it waited for the blocked catalog.
func controlWithin(ctx context.Context, t *testing.T, l *lifecycle, request protocol.Control) (protocol.Control, error) {
	t.Helper()
	type reply struct {
		control protocol.Control
		err     error
	}
	done := make(chan reply, 1)
	go func() {
		control, _, err := l.HandleControl(ctx, request)
		done <- reply{control, err}
	}()
	select {
	case got := <-done:
		return got.control, got.err
	case <-time.After(30 * time.Second):
		t.Fatalf("%s waited for the catalog", request.Type)
		return protocol.Control{}, nil
	}
}

// The catalog scan leaves pipe reservations out, and pipe jobs are reached
// only through their exact binding, so creating one, replaying its key and
// its exact controls never wait for the catalog.
func TestPipeExecutionNeverWaitsForTheCatalog(t *testing.T) {
	catalog := newBlockingCatalog()
	t.Cleanup(catalog.unblock)
	l := executionCatalogLifecycle(t, catalog)
	ctx := context.WithValue(t.Context(), localClientKey{}, true)
	create := protocol.Control{Type: protocol.TypeExecutionCreate, RequestID: "r", RequestKey: "pipe-job", ExecutionKind: "pipe", Stdin: "closed", Command: []string{"/bin/true"}, Cwd: "/work"}
	response, err := controlWithin(ctx, t, l, create)
	if err != nil || response.ExecutionReceipt == nil {
		t.Fatalf("create: %+v %v", response, err)
	}
	job := response.ExecutionReceipt
	if job.Publication != "published" || job.Phase != "live" || job.LaunchOutcome != "started" {
		t.Fatalf("receipt: %+v", job)
	}
	// The fixture launched no real worker, so the replay finds nothing to
	// probe and reports an unknown outcome; it still must not wait.
	replay, _ := controlWithin(ctx, t, l, create)
	if replay.ExecutionReceipt == nil || replay.ExecutionReceipt.Instance != job.Instance {
		t.Fatalf("replay: %+v", replay)
	}
	wait := protocol.Control{Type: protocol.TypeExecutionWait, RequestID: "w", SessionID: job.SessionID, Instance: job.Instance, TimeoutMillis: 50}
	waited, err := controlWithin(ctx, t, l, wait)
	if err != nil || waited.ExecutionWait != executionWaitTimedOut {
		t.Fatalf("wait: %+v %v", waited, err)
	}
	if calls := catalog.reconciliationCount(); calls != 0 {
		t.Fatalf("pipe controls ran %d catalog reconciles", calls)
	}
}

// A terminal job is attached through the catalog, so its creation still
// waits for the reconcile.
func TestTerminalExecutionCreateWaitsForTheCatalog(t *testing.T) {
	catalog := newBlockingCatalog()
	t.Cleanup(catalog.unblock)
	l := executionCatalogLifecycle(t, catalog)
	ctx := context.WithValue(t.Context(), localClientKey{}, true)
	request := protocol.Control{Type: protocol.TypeExecutionCreate, RequestID: "r", RequestKey: "pty-job", ExecutionKind: "pty", Command: []string{"/bin/true"}, Cwd: "/work"}
	done := make(chan error, 1)
	go func() {
		_, _, err := l.HandleControl(ctx, request)
		done <- err
	}()
	<-catalog.started
	select {
	case err := <-done:
		t.Fatalf("terminal create replied before its catalog reconcile: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	catalog.unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
