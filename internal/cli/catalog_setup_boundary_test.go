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
		run := collectCatalogSetupBoundary(t, host, 50*time.Millisecond, true)
		rows, observed := run.rows, run.observed
		if run.configuredSetup != 50*time.Millisecond {
			t.Fatalf("setup context received budget %s, want the configured 50ms", run.configuredSetup)
		}
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
	cache           *SQLiteCatalogCache
	before          []protocol.SessionInfo
	rows            []HostSessions
	observed        *catalogSetupObservedConn
	setupCtx        context.Context
	configuredSetup time.Duration
	settledGrant    bool
}

// collectCatalogSetupBoundary lists host over a real authenticated native
// connection. With expire, the configured setup budget expires at the
// wake-cache settlement step instead of on the wall clock, so a slow dial or
// host.info cannot consume it first.
func collectCatalogSetupBoundary(t *testing.T, host HostRecord, setup time.Duration, expire bool) catalogSetupBoundaryRun {
	t.Helper()
	if reflect.ValueOf(newHostSetupContext).Pointer() != reflect.ValueOf(context.WithTimeout).Pointer() {
		t.Fatal("host setup budget no longer defaults to context.WithTimeout")
	}
	cache, err := OpenCatalogCache(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	run := catalogSetupBoundaryRun{cache: cache}
	cached := []protocol.SessionInfo{{ID: "OLD1", HostID: host.ID, Command: []string{"cached-fixture"}, State: "detached", CreatedAt: time.Now()}}
	if _, err := cache.Save(t.Context(), host, cached); err != nil {
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
	if expire {
		scheduleSetupExpiryAtWakeSettlement(t, &run)
	}
	query := func(ctx context.Context, host *HostRecord, budget HostQueryBudget) ([]protocol.SessionInfo, error) {
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

// scheduleSetupExpiryAtWakeSettlement derives setup through
// withScheduledSetup and expires it when verified setup reaches wake-cache
// settlement, before the real settlement code runs.
func scheduleSetupExpiryAtWakeSettlement(t *testing.T, run *catalogSetupBoundaryRun) {
	var expire func()
	newHostSetupContext = func(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
		run.configuredSetup = timeout
		setup, cancel, expireSetup := withScheduledSetup(parent)
		expire = expireSetup
		return setup, cancel
	}
	rememberWakeGrant = func(ctx context.Context, grant *wake.Grant) {
		run.settledGrant = grant != nil
		expire()
		<-ctx.Done()
		cacheWakeGrant(ctx, grant)
	}
	t.Cleanup(func() {
		newHostSetupContext = context.WithTimeout
		rememberWakeGrant = cacheWakeGrant
	})
}

// errScheduledSetupExpiry is the cause of a scheduled setup expiry. It wraps
// DeadlineExceeded, as the cause of a timer-driven expiry is DeadlineExceeded.
var errScheduledSetupExpiry = fmt.Errorf("scheduled catalog setup expiry: %w", context.DeadlineExceeded)

// scheduledSetupContext is derived from its parent like context.WithTimeout,
// except its deadline passes when expire is called rather than on the wall
// clock, so it reports only the parent's Deadline. Parent cancellation, cause
// and values flow through a standard WithCancelCause child.
type scheduledSetupContext struct {
	context.Context
	cancel context.CancelCauseFunc
	done   chan struct{}
	err    error
}

func withScheduledSetup(parent context.Context) (context.Context, context.CancelFunc, func()) {
	inner, cancel := context.WithCancelCause(parent)
	setup := &scheduledSetupContext{Context: inner, cancel: cancel, done: make(chan struct{})}
	context.AfterFunc(inner, setup.settle)
	return setup, func() { cancel(nil) }, setup.expire
}

func (c *scheduledSetupContext) settle() {
	c.err = c.Context.Err()
	if errors.Is(context.Cause(c.Context), errScheduledSetupExpiry) {
		c.err = context.DeadlineExceeded
	}
	close(c.done)
}

func (c *scheduledSetupContext) expire() { c.cancel(errScheduledSetupExpiry) }

func (c *scheduledSetupContext) Done() <-chan struct{} { return c.done }

func (c *scheduledSetupContext) Err() error {
	select {
	case <-c.done:
		return c.err
	default:
		return nil
	}
}

func TestScheduledSetupContextKeepsDerivedSemantics(t *testing.T) {
	type key struct{}
	deadline := time.Now().Add(time.Hour)
	parent, cancelDeadline := context.WithDeadline(context.WithValue(t.Context(), key{}, "value"), deadline)
	defer cancelDeadline()

	t.Run("parent cancellation wins", func(t *testing.T) {
		cause := errors.New("parent cause")
		parent, cancelParent := context.WithCancelCause(parent)
		setup, cancel, expire := withScheduledSetup(parent)
		defer cancel()
		child, cancelChild := context.WithTimeout(setup, time.Hour)
		defer cancelChild()
		cancelParent(cause)
		<-setup.Done()
		<-child.Done()
		expire()
		if !errors.Is(setup.Err(), context.Canceled) || !errors.Is(context.Cause(setup), cause) || !errors.Is(child.Err(), context.Canceled) || !errors.Is(context.Cause(child), cause) {
			t.Fatalf("parent cancellation lost: err=%v cause=%v child=%v child cause=%v", setup.Err(), context.Cause(setup), child.Err(), context.Cause(child))
		}
	})
	t.Run("scheduled expiry is a deadline", func(t *testing.T) {
		setup, cancel, expire := withScheduledSetup(parent)
		defer cancel()
		child, cancelChild := context.WithTimeout(setup, time.Hour)
		defer cancelChild()
		if got, ok := setup.Deadline(); !ok || !got.Equal(deadline) || setup.Value(key{}) != "value" || setup.Err() != nil {
			t.Fatal("scheduled setup did not inherit the parent's deadline and values")
		}
		expire()
		<-child.Done()
		if !errors.Is(setup.Err(), context.DeadlineExceeded) || !errors.Is(context.Cause(setup), context.DeadlineExceeded) || !errors.Is(child.Err(), context.DeadlineExceeded) || parent.Err() != nil {
			t.Fatalf("scheduled expiry: err=%v cause=%v child=%v parent=%v", setup.Err(), context.Cause(setup), child.Err(), parent.Err())
		}
	})
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
