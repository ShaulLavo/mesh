package updatebootstrap

import (
	"bytes"
	"context"
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
	t.Cleanup(func() { _ = os.RemoveAll(state) })
	command := exec.Command(binary, "daemon", "--tailnet-port=0", "--ssh-port=0") //nolint:gosec // operator-selected retained executable runs only in isolated test state
	command.Env = append(os.Environ(), "MESH_STATE_DIR="+state, "MESH_CONFIG_DIR="+filepath.Join(state, "config"))
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
		if t.Failed() {
			t.Logf("retained daemon: %s", output.String())
		}
	})
	awaitLegacyDaemonSocket(t, filepath.Join(state, "daemon.sock"))
	// The retained daemon owns its worker's private launch arguments and reservation.
	dir, workerPID := createLegacySession(t, state)
	metaBefore, err := os.ReadFile(filepath.Join(dir, "meta.json")) //nolint:gosec // fixed metadata name under the isolated test worker directory
	if err != nil {
		t.Fatal(err)
	}
	meta, err := workerstate.ReadMeta(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Kill(meta.PID, syscall.SIGKILL) }()
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
	if observedWorker.ID != filepath.Base(dir) || observedWorker.PID != workerPID || observedWorker.ShellPID != meta.PID || observedWorker.Protocol != 1 || observedWorker.Build == nil || observedWorker.Build.Digest != wantDigest {
		t.Fatalf("legacy worker = %+v", observedWorker)
	}
	if _, err = release.CompareVersions(observedWorker.Build.Version, "v0.0.0"); err != nil {
		t.Fatalf("legacy worker version = %q: %v", observedWorker.Build.Version, err)
	}
	if err = syscall.Kill(observedWorker.PID, 0); err != nil {
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

func createLegacySession(t *testing.T, state string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	conn, err := dial(ctx, filepath.Join(state, "daemon.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	response, err := exchange(conn, protocol.Control{Type: protocol.TypeCreate, RequestID: "legacy-probe-create", Command: []string{"/bin/cat"}, Cols: 80, Rows: 24})
	if err != nil || response.Type != protocol.TypeCreated || response.SessionID == "" {
		t.Fatalf("create retained session: %+v %v", response, err)
	}
	dir := filepath.Join(state, "s", response.SessionID)
	worker, err := dial(ctx, filepath.Join(dir, "sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = worker.Close() }()
	image, err := peerImage(ctx, worker)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(image.PID, syscall.SIGKILL) })
	return dir, image.PID
}

func awaitLegacyDaemonSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("retained daemon did not open its socket")
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
