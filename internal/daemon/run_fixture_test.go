package daemon

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/transport"
)

type daemonRunFixture struct {
	root     string
	stateDir string
	cancel   context.CancelFunc
	done     chan struct{}
	err      error
	joined   bool
	env      map[string]daemonFixtureEnv
}

type daemonFixtureEnv struct {
	value   string
	exists  bool
	private string
}

func newDaemonRunFixture(t *testing.T) *daemonRunFixture {
	t.Helper()
	base := os.Getenv("MESH_SHORT_TMP")
	if base == "" {
		base = os.TempDir()
	}
	root, err := os.MkdirTemp(base, "d-")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &daemonRunFixture{root: root, stateDir: filepath.Join(root, "state"), env: make(map[string]daemonFixtureEnv)}
	t.Cleanup(func() { fixture.remove(t) })
	if len(SocketPath(fixture.stateDir)) > 103 {
		t.Fatal("daemon fixture socket path exceeds 103 bytes; set MESH_SHORT_TMP to a shorter private directory")
	}
	for key, directory := range map[string]string{
		"HOME": "home", "XDG_CONFIG_HOME": "config", "XDG_STATE_HOME": "xdg-state",
		"XDG_CACHE_HOME": "cache", "MESH_STATE_DIR": "state",
		"MESH_UPDATE_CACHE_DIR": "updates", "TMPDIR": "tmp", "TMP": "tmp", "TEMP": "tmp",
	} {
		path := filepath.Join(root, directory)
		if err := os.MkdirAll(path, 0o700); err != nil { //nolint:gosec // fixed child names beneath the fresh private MkdirTemp root
			t.Fatal(err)
		}
		// t.Setenv would restore live paths even when the run has not joined.
		value, exists := os.LookupEnv(key)
		fixture.env[key] = daemonFixtureEnv{value: value, exists: exists, private: path}
		if err := os.Setenv(key, path); err != nil {
			t.Fatal(err)
		}
	}
	return fixture
}

func (f *daemonRunFixture) start(t *testing.T, runner func(context.Context) error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	f.done = make(chan struct{})
	t.Cleanup(func() { f.stop(t) })
	t.Log("daemon fixture: starting")
	go func() {
		f.err = runner(ctx)
		close(f.done)
	}()
}

func (f *daemonRunFixture) stop(t *testing.T) {
	t.Helper()
	t.Log("daemon fixture: canceling")
	f.cancel()
	timer := time.NewTimer(runtimeTestTimeout)
	defer timer.Stop()
	select {
	case <-f.done:
		f.joined = true
		t.Logf("daemon fixture: joined, run returned %v", f.err)
		if f.err != nil {
			t.Errorf("daemon fixture run: %v", f.err)
		}
	case <-timer.C:
		t.Errorf("daemon fixture: shutdown did not complete within %s; retaining %s", runtimeTestTimeout, f.root)
	}
}

func (f *daemonRunFixture) remove(t *testing.T) {
	t.Helper()
	if f.done != nil && !f.joined {
		t.Logf("daemon fixture: private root and environment retained at %s because run completion is unknown", f.root)
		return
	}
	for key, previous := range f.env {
		var err error
		if previous.exists {
			err = os.Setenv(key, previous.value)
		} else {
			err = os.Unsetenv(key)
		}
		if err != nil {
			t.Error(err)
		}
	}
	if err := os.RemoveAll(f.root); err != nil { //nolint:gosec // owns the fresh MkdirTemp root
		t.Error(err)
	}
	t.Log("daemon fixture: private root removed")
}

func (f *daemonRunFixture) dial(t *testing.T) transport.Conn {
	t.Helper()
	timer := time.NewTimer(runtimeTestTimeout)
	defer timer.Stop()
	retry := time.NewTicker(10 * time.Millisecond)
	defer retry.Stop()
	var lastErr error
	for {
		select {
		case <-f.done:
			t.Fatalf("daemon fixture: run returned before readiness: %v", f.err)
		case <-timer.C:
			t.Fatalf("daemon fixture: readiness did not complete within %s: %v", runtimeTestTimeout, lastErr)
		case <-retry.C:
		}
		stream, err := net.DialTimeout("unix", SocketPath(f.stateDir), 100*time.Millisecond)
		if err != nil {
			lastErr = err
			continue
		}
		conn, err := transport.NewStreamConn(stream)
		if err != nil {
			_ = stream.Close()
			t.Fatal(fmt.Errorf("wrap daemon fixture connection: %w", err))
		}
		t.Log("daemon fixture: socket ready")
		return conn
	}
}
