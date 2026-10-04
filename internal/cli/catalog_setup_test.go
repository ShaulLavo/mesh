package cli

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/daemon"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/worker"
)

func TestListCatalogBudgetStartsAfterAuthenticatedSetup(t *testing.T) {
	for _, test := range []struct {
		name  string
		delay time.Duration
		ended bool
	}{
		{name: "fast empty control"},
		{name: "slow healthy empty", delay: defaultCatalogTimeout + 150*time.Millisecond},
		{name: "slow healthy ended", delay: defaultCatalogTimeout + 150*time.Millisecond, ended: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("SHELL", "/bin/sh")
			t.Setenv("MESH_CONFIG_DIR", t.TempDir())
			clientState := compactSocketTempDir(t)
			t.Setenv("MESH_STATE_DIR", clientState)
			host := nativeCatalogHost(t, test.delay, test.ended)
			if err := SaveHost(host); err != nil {
				t.Fatal(err)
			}
			cache, err := OpenCatalogCache(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cache.Close() }()
			if err := cache.Save(t.Context(), host, []protocol.SessionInfo{{ID: "OLD1", HostID: host.ID, Command: []string{"cached-fixture"}, State: "detached", CreatedAt: time.Now()}}); err != nil {
				t.Fatal(err)
			}
			stdout, stderr, err := executeCommand(t, Dependencies{}, "ls", "--all")
			if err != nil || strings.Contains(stderr, "unavailable") || strings.Contains(stdout, "OLD1") {
				t.Fatalf("healthy authenticated catalog became stale: error=%v stderr=%q cached-row=%v", err, stderr, strings.Contains(stdout, "OLD1"))
			}
			rows, err := cache.Load(t.Context(), host)
			if err != nil {
				t.Fatal(err)
			}
			want := 1
			if test.ended {
				want++
			}
			if len(rows) != want {
				t.Fatalf("settled cached row count=%d, want=%d", len(rows), want)
			}
			for _, row := range rows {
				if row.HostID != host.ID || row.ID == "OLD1" && row.State != "interrupted" || row.ID == "7K3D" && row.State != "exited" {
					t.Fatal("cache did not settle authoritative host session states")
				}
			}
			t.Logf("setup delay=%s, native authoritative rows=%d, stale diagnostics=0", test.delay, len(rows)-1)
		})
	}
}

func nativeCatalogHost(t *testing.T, delay time.Duration, ended bool) HostRecord {
	t.Helper()
	state := compactSocketTempDir(t)
	host, key, err := identity.LoadOrCreate(state)
	if err != nil {
		t.Fatal(err)
	}
	client, _, err := identity.LoadOrCreate(os.Getenv("MESH_STATE_DIR"))
	if err != nil {
		t.Fatal(err)
	}
	if err := identity.ApproveDevice(state, client.ID); err != nil {
		t.Fatal(err)
	}
	if ended {
		dir := filepath.Join(state, "s", "7K3D")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		code := 0
		now := time.Now()
		if err := worker.WriteMeta(dir, worker.Meta{ID: "7K3D", Command: []string{"native-fixture"}, Cwd: state, PID: os.Getpid(), State: worker.StateExited, CreatedAt: now, ExitedAt: &now, ExitCode: &code}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- daemon.Run(ctx, daemon.Config{StateDir: state}) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("native catalog daemon did not stop")
		}
	})
	socket := daemon.SocketPath(state)
	deadline := time.Now().Add(3 * time.Second)
	for {
		conn, err := net.DialTimeout("unix", socket, 50*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("native catalog daemon socket unavailable")
		}
		time.Sleep(time.Millisecond)
	}
	auth := &transport.Authentication{Key: key, Authorize: func(id string) bool { return identity.GrantedIdentity(state, id) }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-r.Context().Done():
			return
		}
		_ = transport.ServeWithOptions(w, r, transport.ServeOptions{Auth: auth}, func(ctx context.Context, peer transport.Conn) error {
			stream, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
			if err != nil {
				return fmt.Errorf("connect native fixture daemon: %w", err)
			}
			local, err := transport.NewStreamConn(stream)
			if err != nil {
				_ = stream.Close()
				return fmt.Errorf("wrap native fixture stream: %w", err)
			}
			defer func() { _ = local.Close() }()
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				_ = relayCatalogFrames(local, peer)
				_ = local.Close()
				_ = peer.Close()
			}()
			err = relayCatalogFrames(peer, local)
			_ = local.Close()
			_ = peer.Close()
			<-finished
			return err
		})
	}))
	t.Cleanup(server.Close)
	return HostRecord{Alias: "fixture", ID: host.ID, MeshIdentity: host.ID, TailscaleName: "fixture.example.ts.net", Addresses: []string{"127.0.0.1"}, Endpoint: "ws" + strings.TrimPrefix(server.URL, "http") + "/mesh"}
}

func relayCatalogFrames(target, source transport.Conn) error {
	for {
		frame, err := source.ReadFrame()
		if err != nil {
			return fmt.Errorf("read native fixture frame: %w", err)
		}
		if err := target.WriteFrame(frame); err != nil {
			return fmt.Errorf("write native fixture frame: %w", err)
		}
	}
}
