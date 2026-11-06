package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/wake"
)

func TestListCatalogRejectsSetupExpiredDuringWakeCacheSettlement(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	state := compactSocketTempDir(t)
	t.Setenv("MESH_STATE_DIR", state)
	host := nativeCatalogHost(t, 0, 0, true)
	if _, err := wake.NewCache(state); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(state)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	lock, err := root.OpenFile("wake/cache.lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Flock(int(lock.Fd()), unix.LOCK_UN) }()

	t.Run("healthy retained connection", func(t *testing.T) {
		run := collectCatalogSetupBoundary(t, host, time.Second, false)
		rows, observed := run.rows, run.observed
		if rows[0].Host.ID != host.ID || rows[0].Host.MachineName != "fixture" || rows[0].Host.NameRevision != 1 || !rows[0].Host.NameVerified || rows[0].Stale || rows[0].Err != nil || len(rows[0].Sessions) != 1 || rows[0].Sessions[0].ID != "7K3D" || observed.lists.Load() != 1 {
			t.Fatal("healthy setup lost native catalog or retained connection", rows[0].Err)
		}
		t.Log("healthy retained transport returns native catalog despite best-effort wake cache contention")
	})
	t.Run("expired setup refuses catalog", func(t *testing.T) {
		// Setup expires when verification reaches wake-cache settlement, so
		// this budget only bounds a hang and never races dial or host.info.
		run := collectCatalogSetupBoundary(t, host, time.Minute, true)
		rows, observed := run.rows, run.observed
		if !run.settledGrant {
			t.Fatal("verified setup did not reach wake-cache settlement with a native grant")
		}
		if !errors.Is(run.setupCtx.Err(), context.DeadlineExceeded) {
			t.Fatal("fixture did not observe setup expiry after real host.info")
		}
		if !rows[0].Stale || !errors.Is(rows[0].Err, context.DeadlineExceeded) || observed.lists.Load() != 0 {
			t.Fatalf("expired setup admitted catalog: stale=%v deadline=%v list requests=%d", rows[0].Stale, errors.Is(rows[0].Err, context.DeadlineExceeded), observed.lists.Load())
		}
		if rows[0].Host.ID != host.ID || rows[0].Host.MachineName != host.MachineName || rows[0].Host.NameRevision != host.NameRevision || rows[0].Host.NameVerified != host.NameVerified {
			t.Fatal("expired setup published a fresh destination declaration")
		}
		stored, err := run.cache.Load(t.Context(), host)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(run.before, stored) || !reflect.DeepEqual(run.before, rows[0].Sessions) {
			t.Fatal("expired setup changed cached authority or stale fallback rows")
		}
		t.Log("expired setup closes transport, sends no session.list and preserves cached active state")
	})
}

type catalogSetupBoundaryRun struct {
	cache        *SQLiteCatalogCache
	before       []protocol.SessionInfo
	rows         []HostSessions
	observed     *catalogSetupObservedConn
	setupCtx     context.Context
	settledGrant bool
}

// collectCatalogSetupBoundary lists host over a real authenticated native
// connection. With expire, setup expires at the wake-cache settlement step.
func collectCatalogSetupBoundary(t *testing.T, host HostRecord, setup time.Duration, expire bool) catalogSetupBoundaryRun {
	t.Helper()
	cache, err := OpenCatalogCache(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	run := catalogSetupBoundaryRun{cache: cache}
	cached := []protocol.SessionInfo{{ID: "OLD1", HostID: host.ID, Command: []string{"cached-fixture"}, State: "detached", CreatedAt: time.Now()}}
	if err := cache.Save(t.Context(), host, cached); err != nil {
		t.Fatal(err)
	}
	if run.before, err = cache.Load(t.Context(), host); err != nil {
		t.Fatal(err)
	}
	dial := func(ctx context.Context, host HostRecord) (transport.Conn, error) {
		run.setupCtx = ctx
		conn, err := dialControlHost(ctx, host)
		if err != nil {
			return nil, err
		}
		run.observed = &catalogSetupObservedConn{Conn: conn}
		return run.observed, nil
	}
	deadline := make(chan struct{})
	if expire {
		rememberWakeGrant = func(ctx context.Context, grant *wake.Grant) {
			run.settledGrant = grant != nil
			close(deadline)
			<-ctx.Done()
			cacheWakeGrant(ctx, grant)
		}
		t.Cleanup(func() { rememberWakeGrant = cacheWakeGrant })
	}
	query := func(ctx context.Context, host *HostRecord, budget HostQueryBudget) ([]protocol.SessionInfo, error) {
		if expire {
			ctx = withCatalogSetupExpiry(ctx, deadline)
		}
		return listRemoteDeclaredHost(ctx, host, dial, budget)
	}
	run.rows, err = CollectHostSessions(t.Context(), []HostRecord{host}, HostQueryBudget{Setup: setup, Reply: time.Second}, query, cache)
	if failure := run.controlFailure(err); failure != nil {
		t.Fatal("native authenticated host.info control unavailable: ", failure)
	}
	if !run.observed.closed.Load() {
		t.Fatal("catalog setup retained an open connection after return")
	}
	return run
}

func (r catalogSetupBoundaryRun) controlFailure(err error) error {
	if err != nil {
		return fmt.Errorf("collect host sessions: %w", err)
	}
	if len(r.rows) != 1 {
		return fmt.Errorf("collected %d host rows, want 1", len(r.rows))
	}
	if r.observed == nil {
		return fmt.Errorf("dial failed: %w", r.rows[0].Err)
	}
	if !r.observed.hostInfo.Load() {
		return fmt.Errorf("no host.info result: %w", r.rows[0].Err)
	}
	return nil
}

// catalogSetupExpiry is a setup parent whose deadline passes when the test
// closes expired, so expiry lands at a chosen step after real verification.
type catalogSetupExpiry struct {
	context.Context
	expired chan struct{}
}

func withCatalogSetupExpiry(parent context.Context, expired chan struct{}) context.Context {
	return &catalogSetupExpiry{Context: parent, expired: expired}
}

func (c *catalogSetupExpiry) Done() <-chan struct{} { return c.expired }

func (c *catalogSetupExpiry) Err() error {
	select {
	case <-c.expired:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

type catalogSetupObservedConn struct {
	transport.Conn
	hostInfo atomic.Bool
	lists    atomic.Int64
	closed   atomic.Bool
}

func (c *catalogSetupObservedConn) ReadFrame() (protocol.Frame, error) {
	frame, err := c.Conn.ReadFrame()
	if err != nil {
		return protocol.Frame{}, fmt.Errorf("observe native catalog response: %w", err)
	}
	response, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return protocol.Frame{}, fmt.Errorf("decode native catalog response: %w", err)
	}
	if response.Type == protocol.TypeHostInfoResult {
		c.hostInfo.Store(true)
	}
	return frame, nil
}

func (c *catalogSetupObservedConn) WriteFrame(frame protocol.Frame) error {
	request, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return fmt.Errorf("decode native catalog request: %w", err)
	}
	if request.Type == protocol.TypeList {
		c.lists.Add(1)
	}
	if err := c.Conn.WriteFrame(frame); err != nil {
		return fmt.Errorf("forward native catalog request: %w", err)
	}
	return nil
}

func (c *catalogSetupObservedConn) Close() error {
	c.closed.Store(true)
	if err := c.Conn.Close(); err != nil {
		return fmt.Errorf("close native catalog observation: %w", err)
	}
	return nil
}
