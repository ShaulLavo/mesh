package cli

import (
	"context"
	"errors"
	"fmt"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/update"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
)

func TestLargeNativeCatalogPreservesEverySession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	t.Setenv("MESH_STATE_DIR", compactSocketTempDir(t))
	host := nativeCatalogHostWithSessions(t, 0, 0, false, 168)
	conn, err := openVerifiedHost(t.Context(), host, dialControlHost)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	response, err := controlRequest(t.Context(), conn, protocol.Control{Type: protocol.TypeList, RequestID: "complete", Lean: true})
	if err != nil || response.Type != protocol.TypeListed {
		t.Fatal("native catalog incomplete", err)
	}
	rows := response.Sessions
	if len(rows) != 168 {
		t.Fatalf("rows=%d, want168", len(rows))
	}
	for _, row := range rows {
		if len(row.Command) != 3 || len(row.Command[2]) != 32<<10 {
			t.Fatal("command truncated")
		}
	}
	t.Logf("native pinned catalog returned %d sessions with %d command bytes", len(rows), len(rows)*(32<<10))
	legacy, err := update.Exchange(t.Context(), update.Host{ID: host.ID, Endpoint: host.Endpoint}, protocol.Control{Type: protocol.TypeList, RequestID: "updater", Lean: true})
	if err != nil || len(legacy.Sessions) != 168 {
		t.Fatal("updater observation lost catalog pages", err)
	}
	response, err = controlRequest(t.Context(), conn, protocol.Control{Type: protocol.TypeHostInfo, RequestID: "after-large"})
	if err != nil || response.Type != protocol.TypeHostInfoResult {
		t.Fatal("small control failed", err)
	}
}

func TestLargeNativeCatalogBackpressureAndStateSnapshot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	t.Setenv("MESH_STATE_DIR", compactSocketTempDir(t))
	host := nativeCatalogHostWithSessions(t, 0, 0, false, 420)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	conn, err := openVerifiedHost(ctx, host, dialControlHost)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	paused := &pausedCatalogConn{Conn: conn, seen: make(chan struct{}), resume: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		rows, err := controlRequest(ctx, paused, protocol.Control{Type: protocol.TypeList, RequestID: "paused", Lean: true})
		if err == nil && len(rows.Sessions) != 420 {
			err = errors.New("paused catalog lost sessions")
		}
		done <- err
	}()
	select {
	case <-paused.seen:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	defer paused.release()
	quick, err := openVerifiedHost(ctx, host, dialControlHost)
	if err != nil {
		t.Fatal("stalled reader blocked other commands", err)
	}
	_ = quick.Close()
	time.Sleep(75 * time.Millisecond)
	paused.release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	watch, err := controlRequest(ctx, conn, protocol.Control{Type: protocol.TypeStateWatch, RequestID: "large-watch", Watch: &protocol.StateWatch{Topics: []string{protocol.TopicSessions}}})
	if err != nil || watch.StateSnapshot == nil || len(watch.StateSnapshot.Sessions) != 420 {
		t.Fatal("large state snapshot incomplete", err)
	}
	t.Log("13.1MiB catalog survives paused consumer, independent commands remain available, watch snapshot is complete")
}

type pausedCatalogConn struct {
	transport.Conn
	seen     chan struct{}
	resume   chan struct{}
	once     sync.Once
	released sync.Once
}

func (c *pausedCatalogConn) ReadFrame() (protocol.Frame, error) {
	frame, err := c.Conn.ReadFrame()
	c.once.Do(func() { close(c.seen); <-c.resume })
	if err != nil {
		return protocol.Frame{}, fmt.Errorf("paused catalog: %w", err)
	}
	return frame, nil
}
func (c *pausedCatalogConn) release() { c.released.Do(func() { close(c.resume) }) }

func TestLargeNativeCatalogCancellationDropsPartialResult(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	t.Setenv("MESH_STATE_DIR", compactSocketTempDir(t))
	host := nativeCatalogHostWithSessions(t, 0, 0, false, 168)
	ctx, cancel := context.WithCancel(t.Context())
	conn, err := openVerifiedHost(ctx, host, dialControlHost)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	paused := &pausedCatalogConn{Conn: conn, seen: make(chan struct{}), resume: make(chan struct{})}
	defer paused.release()
	defer cancel()
	done := make(chan error, 1)
	go func() {
		result, err := controlRequest(ctx, paused, protocol.Control{Type: protocol.TypeList, RequestID: "cancel", Lean: true})
		if len(result.Sessions) != 0 {
			err = errors.New("cancelled catalog published partial rows")
		}
		done <- err
	}()
	select {
	case <-paused.seen:
	case <-ctx.Done():
		t.Fatal("first page unavailable", ctx.Err())
	}
	cancel()
	paused.release()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("catalog cancellation not returned", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("catalog cancellation stuck")
	}
	quick, err := openVerifiedHost(t.Context(), host, dialControlHost)
	if err != nil {
		t.Fatal("catalog cancellation left daemon unavailable", err)
	}
	_ = quick.Close()
}
