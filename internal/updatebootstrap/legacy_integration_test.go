package updatebootstrap

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/release"
	workerstate "github.com/shaul/mesh/internal/worker"
)

func TestLegacyWorkerProcessProtocolAndVersion(t *testing.T) {
	binary := os.Getenv("MESH_LEGACY_BINARY")
	if binary == "" {
		t.Skip("set MESH_LEGACY_BINARY to the retained pre-updater Mesh executable")
	}
	state, err := os.MkdirTemp("/tmp", "mesh-legacy-probe-")
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
	metaBefore, err := os.ReadFile(filepath.Join(dir, "meta.json")) //nolint:gosec // fixed metadata name under the isolated test worker directory
	if err != nil {
		t.Fatal(err)
	}
	meta, err := workerstate.ReadMeta(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Kill(meta.PID, syscall.SIGKILL) }()
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
	observedWorker := observed.Health.Workers[0]
	wantDigest, err := imageDigest(binary)
	if err != nil {
		t.Fatal(err)
	}
	if observedWorker.PID != command.Process.Pid || observedWorker.ShellPID != meta.PID || observedWorker.Protocol != 1 || observedWorker.Build == nil || observedWorker.Build.Digest != wantDigest {
		t.Fatalf("legacy worker = %+v", observedWorker)
	}
	if _, err = release.CompareVersions(observedWorker.Build.Version, "v0.0.0"); err != nil {
		t.Fatalf("legacy worker version = %q: %v", observedWorker.Build.Version, err)
	}
	if err = syscall.Kill(command.Process.Pid, 0); err != nil {
		t.Fatalf("legacy worker did not survive read-only probe: %v", err)
	}
	if err = syscall.Kill(meta.PID, 0); err != nil {
		t.Fatalf("legacy shell did not survive read-only probe: %v", err)
	}
	metaAfter, err := os.ReadFile(filepath.Join(dir, "meta.json")) //nolint:gosec // the same isolated metadata file is compared for read-only behavior
	if err != nil || !bytes.Equal(metaAfter, metaBefore) {
		t.Fatalf("read-only probe changed worker metadata: %v", err)
	}
	t.Logf("verified actual legacy worker PID=%d shellPID=%d version=%s digest=%s protocol=%d", observedWorker.PID, observedWorker.ShellPID, observedWorker.Build.Version, observedWorker.Build.Digest, observedWorker.Protocol)
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
