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

	for _, test := range []struct {
		name    string
		setup   time.Duration
		expired bool
	}{
		{name: "healthy retained connection", setup: time.Second},
		{name: "expired setup refuses catalog", setup: 50 * time.Millisecond, expired: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cache, err := OpenCatalogCache(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cache.Close() }()
			cached := []protocol.SessionInfo{{ID: "OLD1", HostID: host.ID, Command: []string{"cached-fixture"}, State: "detached", CreatedAt: time.Now()}}
			if err := cache.Save(t.Context(), host, cached); err != nil {
				t.Fatal(err)
			}
			before, err := cache.Load(t.Context(), host)
			if err != nil {
				t.Fatal(err)
			}
			var setupCtx context.Context
			var observed *catalogSetupObservedConn
			dial := func(ctx context.Context, host HostRecord) (transport.Conn, error) {
				setupCtx = ctx
				conn, err := dialControlHost(ctx, host)
				if err != nil {
					return nil, err
				}
				observed = &catalogSetupObservedConn{Conn: conn}
				return observed, nil
			}
			budget := HostQueryBudget{Setup: test.setup, Reply: time.Second}
			rows, err := CollectHostSessions(t.Context(), []HostRecord{host}, budget,
				func(ctx context.Context, host *HostRecord, budget HostQueryBudget) ([]protocol.SessionInfo, error) {
					return listRemoteDeclaredHost(ctx, host, dial, budget)
				}, cache)
			if err != nil || len(rows) != 1 || observed == nil || !observed.hostInfo.Load() {
				t.Fatal("native authenticated host.info control unavailable", err)
			}
			if !observed.closed.Load() {
				t.Fatal("catalog setup retained an open connection after return")
			}
			if !test.expired {
				if rows[0].Host.ID != host.ID || rows[0].Host.MachineName != "fixture" || rows[0].Host.NameRevision != 1 || !rows[0].Host.NameVerified || rows[0].Stale || rows[0].Err != nil || len(rows[0].Sessions) != 1 || rows[0].Sessions[0].ID != "7K3D" || observed.lists.Load() != 1 {
					t.Fatal("healthy setup lost native catalog or retained connection", rows[0].Err)
				}
				t.Log("healthy retained transport returns native catalog despite best-effort wake cache contention")
				return
			}
			if !errors.Is(setupCtx.Err(), context.DeadlineExceeded) {
				t.Fatal("fixture did not observe setup expiry after real host.info")
			}
			if !rows[0].Stale || !errors.Is(rows[0].Err, context.DeadlineExceeded) || observed.lists.Load() != 0 {
				t.Fatalf("expired setup admitted catalog: stale=%v deadline=%v list requests=%d", rows[0].Stale, errors.Is(rows[0].Err, context.DeadlineExceeded), observed.lists.Load())
			}
			if rows[0].Host.ID != host.ID || rows[0].Host.MachineName != host.MachineName || rows[0].Host.NameRevision != host.NameRevision || rows[0].Host.NameVerified != host.NameVerified {
				t.Fatal("expired setup published a fresh destination declaration")
			}
			stored, err := cache.Load(t.Context(), host)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, stored) || !reflect.DeepEqual(before, rows[0].Sessions) {
				t.Fatal("expired setup changed cached authority or stale fallback rows")
			}
			t.Log("expired setup closes transport, sends no session.list and preserves cached active state")
		})
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
