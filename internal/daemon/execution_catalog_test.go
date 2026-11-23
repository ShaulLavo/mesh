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

// A pipe job is reached only through its exact binding, so its creation does
// not wait for a catalog scan; a background reconcile still lists it.
func TestPipeExecutionCreateDoesNotWaitForTheCatalog(t *testing.T) {
	catalog := newBlockingCatalog()
	t.Cleanup(catalog.unblock)
	l := executionCatalogLifecycle(t, catalog)
	ctx := context.WithValue(t.Context(), localClientKey{}, true)
	request := protocol.Control{Type: protocol.TypeExecutionCreate, RequestID: "r", RequestKey: "pipe-job", ExecutionKind: "pipe", Stdin: "closed", Command: []string{"/bin/true"}, Cwd: "/work"}
	response, handled, err := l.HandleControl(ctx, request)
	if !handled || err != nil || response.ExecutionReceipt == nil {
		t.Fatalf("create: %+v %v", response, err)
	}
	if r := response.ExecutionReceipt; r.Publication != "published" || r.Phase != "live" || r.LaunchOutcome != "started" {
		t.Fatalf("receipt: %+v", r)
	}
	select {
	case <-catalog.started:
	case <-time.After(10 * time.Second):
		t.Fatal("no background catalog reconcile was requested")
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

// A burst of requests while a reconcile runs schedules one more, not one each.
func TestReconcileSoonCoalescesABurst(t *testing.T) {
	catalog := newBlockingCatalog()
	t.Cleanup(catalog.unblock)
	l := executionCatalogLifecycle(t, catalog)
	l.reconcileSoon()
	<-catalog.started
	for range 20 {
		l.reconcileSoon()
	}
	catalog.unblock()
	<-catalog.started
	deadline := time.Now().Add(10 * time.Second)
	for l.reconcilePending.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	l.reconcileSoonMu.Lock()
	calls := catalog.reconciliationCount()
	l.reconcileSoonMu.Unlock()
	if calls != 2 {
		t.Fatalf("reconciles = %d, want the running one and one more", calls)
	}
}
