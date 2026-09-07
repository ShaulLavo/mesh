package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/tailnet"
	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/updategate"
	"github.com/shaul/mesh/internal/worker"
)

func TestUpdateHealthAndAuthorizationUseRunningDaemon(t *testing.T) {
	stateDir := t.TempDir()
	host, key, err := identity.LoadOrCreate(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, Config{StateDir: stateDir}, runOptions{now: time.Now, bootID: worker.BootID, discoverSelf: func(context.Context) (tailnet.Peer, error) { return tailnet.Peer{}, errors.New("offline") }, reconcileInterval: time.Hour})
	}()
	t.Cleanup(func() {
		cancel()
		if err := waitRuntime(t, done); err != nil {
			t.Error(err)
		}
	})
	conn := dialUnixRuntime(t, SocketPath(stateDir))
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	client := update.Client{ID: host.ID, Key: key}
	target := update.LocalHost(stateDir, host.ID)
	var info update.Info
	if err = client.Call(ctx, target, "info", nil, &info); err != nil {
		t.Fatal(err)
	}
	if info.Health.HostID != host.ID || info.Health.Build.Digest != release.Current().Digest || info.Health.Build.StateVersion != release.CurrentStateVersion {
		t.Fatalf("wrong executing health: %#v", info)
	}
	other, otherKey, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	untrusted := update.Client{ID: other.ID, Key: otherKey}
	if err = untrusted.Call(ctx, target, "info", nil, &info); err == nil {
		t.Fatal("unenrolled administrator got update access")
	}
	if err = update.Trust(stateDir, other.ID, true); err != nil {
		t.Fatal(err)
	}
	if err = untrusted.Call(ctx, target, "info", nil, &info); err != nil {
		t.Fatal(err)
	}
	wrongTarget := target
	wrongTarget.ID = other.ID
	if err = client.Call(ctx, wrongTarget, "info", nil, &info); err == nil {
		t.Fatal("accepted endpoint for a different pinned host")
	}
	if err = updategate.Set(stateDir, "test-activation"); err != nil {
		t.Fatal(err)
	}
	if err = client.Call(ctx, target, "info", nil, &info); err != nil {
		t.Fatalf("gate blocked update health: %v", err)
	}
	response, err := update.Exchange(ctx, target, protocol.Control{Type: protocol.TypeCreate, RequestID: "gated-create", Command: []string{"/bin/true"}, Cols: 80, Rows: 24})
	if err == nil || response.ErrorCode != "update.in_progress" {
		t.Fatalf("creation bypassed gate: %#v %v", response, err)
	}
	_, err = worker.Run(worker.Config{ID: "7K3D", Dir: filepath.Join(stateDir, "s", "7K3D"), Command: []string{"/bin/true"}})
	if !errors.Is(err, updategate.ErrUpdating) {
		t.Fatalf("direct worker bypassed gate: %v", err)
	}
	if _, err = os.Stat(filepath.Join(stateDir, "s", "7K3D")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("gated launch wrote session state")
	}
}
