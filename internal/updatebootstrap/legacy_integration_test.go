package updatebootstrap

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/release"
)

func TestLegacyWorkerProcessProtocolAndVersion(t *testing.T) {
	binary := os.Getenv("MESH_LEGACY_BINARY")
	if binary == "" {
		t.Skip("set MESH_LEGACY_BINARY to the retained pre-updater Mesh executable")
	}
	state, err := os.MkdirTemp("/work/tmp/mesh-t26", "legacy-probe-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(state) }()
	createLegacyDatabase(t, state)
	dir := filepath.Join(state, "s", "7K3D")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, "session-worker", "--id", "7K3D", "--dir", dir, "--cols", "80", "--rows", "24", "--", "/bin/cat") //nolint:gosec // operator-selected retained legacy executable runs only in isolated test state
	command.Env = append(os.Environ(), "MESH_STATE_DIR="+state)
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = command.Process.Kill(); _ = command.Wait() }()
	awaitWorkerSocket(t, filepath.Join(dir, "sock"))
	listener, err := net.Listen("unix", filepath.Join(state, "daemon.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	build := release.Current()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = protocol.NewReader(conn).ReadFrame()
		_ = protocol.NewWriter(conn).WriteControlMsg(protocol.Control{Type: protocol.TypeHostInfoResult, Host: &protocol.HostInfo{ID: "fixture-host", Build: &build}})
	}()
	observed, err := Inspect(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	if len(observed.Health.Workers) != 1 {
		t.Fatalf("workers = %+v", observed.Health.Workers)
	}
	worker := observed.Health.Workers[0]
	if worker.PID != command.Process.Pid || worker.Protocol != 1 || worker.Build == nil || !strings.HasPrefix(worker.Build.Version, "v0.1.38") {
		t.Fatalf("legacy worker = %+v", worker)
	}
	t.Logf("verified actual legacy worker PID=%d version=%s digest=%s protocol=%d", worker.PID, worker.Build.Version, worker.Build.Digest, worker.Protocol)
}

func awaitWorkerSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("legacy worker did not open its socket")
}

func TestInstalledDaemonProbeReadOnly(t *testing.T) {
	state := os.Getenv("MESH_PROBE_STATE")
	if state == "" {
		t.Skip("set MESH_PROBE_STATE to inspect an existing daemon without modifying it")
	}
	observed, err := Inspect(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("daemon version=%s digest=%s schema=%d workers=%d", observed.Health.Build.Version, observed.Health.Build.Digest, observed.Health.Build.StateVersion, len(observed.Health.Workers))
	for _, worker := range observed.Health.Workers {
		t.Logf("session=%s workerPID=%d shellPID=%d version=%s protocol=%d", worker.ID, worker.PID, worker.ShellPID, worker.Build.Version, worker.Protocol)
	}
}
