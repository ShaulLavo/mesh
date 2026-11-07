package updatebootstrap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
	state, _ := startLegacyFixture(t, binary)
	dir, workerPID, err := createLegacySession(t.Context(), state, peerImage)
	if err != nil {
		t.Fatal(err)
	}
	metaBefore, err := os.ReadFile(filepath.Join(dir, "meta.json")) //nolint:gosec // fixed metadata name under the isolated test worker directory
	if err != nil {
		t.Fatal(err)
	}
	meta, err := workerstate.ReadMeta(dir)
	if err != nil {
		t.Fatal(err)
	}
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
	t.Run("failed image probe cleanup", func(t *testing.T) { testLegacyWorkerFixtureCleansFailedImageProbe(t, binary) })
}

func startLegacyFixture(t *testing.T, binary string) (string, func()) {
	t.Helper()
	state, err := os.MkdirTemp("", "mesh-legacy-probe-")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, "daemon", "--tailnet-port=0", "--ssh-port=0") //nolint:gosec // operator-selected retained executable runs only in isolated test state
	command.Env = append(os.Environ(), "MESH_STATE_DIR="+state, "MESH_CONFIG_DIR="+filepath.Join(state, "config"))
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	var once sync.Once
	cleanup := func() { once.Do(func() { stopLegacyFixture(t, state, command, &output) }) }
	t.Cleanup(cleanup)
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	awaitLegacyDaemonSocket(t, filepath.Join(state, "daemon.sock"))
	return state, cleanup
}

func stopLegacyFixture(t *testing.T, state string, command *exec.Cmd, output *bytes.Buffer) {
	t.Helper()
	if command.Process != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
	}
	if err := cleanupLegacyWorkers(state); err != nil {
		t.Errorf("retained fixture cleanup: %v; state kept at %s", err, state)
		return
	}
	if err := os.RemoveAll(state); err != nil {
		t.Error(err)
	}
	if t.Failed() {
		t.Logf("retained daemon: %s", output.String())
	}
}

func createLegacySession(parent context.Context, state string, inspect func(context.Context, net.Conn) (executableImage, error)) (string, int, error) {
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	conn, err := dial(ctx, filepath.Join(state, "daemon.sock"))
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = conn.Close() }()
	response, err := exchange(conn, protocol.Control{Type: protocol.TypeCreate, RequestID: "legacy-probe-create", Command: []string{"/bin/cat"}, Cols: 80, Rows: 24})
	if err != nil || response.Type != protocol.TypeCreated || response.SessionID == "" {
		return "", 0, fmt.Errorf("create retained session: %+v: %w", response, errors.Join(err, errors.New("invalid create response")))
	}
	dir := filepath.Join(state, "s", response.SessionID)
	worker, err := dial(ctx, filepath.Join(dir, "sock"))
	if err != nil {
		return dir, 0, err
	}
	defer func() { _ = worker.Close() }()
	image, err := inspect(ctx, worker)
	return dir, image.PID, err
}

type legacyFixtureProcess struct {
	parent int
	state  string
	args   []string
}

func legacyFixtureProcesses() (map[int]legacyFixtureProcess, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=,stat=,command=").Output()
	if err != nil {
		return nil, fmt.Errorf("list fixture processes: %w", err)
	}
	result := make(map[int]legacyFixtureProcess)
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			return nil, fmt.Errorf("incomplete fixture process listing")
		}
		pid, pidErr := strconv.Atoi(fields[0])
		parent, parentErr := strconv.Atoi(fields[1])
		if err := errors.Join(pidErr, parentErr); err != nil {
			return nil, err
		}
		result[pid] = legacyFixtureProcess{parent, fields[2], fields[3:]}
	}
	return result, nil
}
func legacyWorkerInState(process legacyFixtureProcess, state string) bool {
	args := process.args
	for index, arg := range args {
		if arg == "--dir" && index > 0 && index+1 < len(args) && strings.HasPrefix(args[index+1], filepath.Join(state, "s")+string(filepath.Separator)) && args[1] == "session-worker" {
			return true
		}
	}
	return false
}
func legacyOwnedProcesses(all map[int]legacyFixtureProcess, state string) map[int]bool {
	children := make(map[int][]int)
	owned := make(map[int]bool)
	var queue []int
	for pid, process := range all {
		children[process.parent] = append(children[process.parent], pid)
		if legacyWorkerInState(process, state) {
			owned[pid] = true
			queue = append(queue, pid)
		}
	}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		for _, child := range children[pid] {
			owned[child] = true
			queue = append(queue, child)
		}
	}
	return owned
}
func cleanupLegacyWorkers(state string) error {
	all, err := legacyFixtureProcesses()
	if err != nil {
		return err
	}
	owned := legacyOwnedProcesses(all, state)
	for pid := range owned {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		all, err = legacyFixtureProcesses()
		if err != nil {
			return err
		}
		if !legacyProcessesLive(all, owned) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("fixture processes did not exit: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}
func legacyProcessesLive(all map[int]legacyFixtureProcess, owned map[int]bool) bool {
	for pid := range owned {
		if process, exists := all[pid]; exists && !strings.HasPrefix(process.state, "Z") {
			return true
		}
	}
	return false
}
func testLegacyWorkerFixtureCleansFailedImageProbe(t *testing.T, binary string) {
	t.Helper()
	state, cleanup := startLegacyFixture(t, binary)
	dir, pid, err := createLegacySession(t.Context(), state, func(ctx context.Context, conn net.Conn) (executableImage, error) {
		image, err := peerImage(ctx, conn)
		if err != nil {
			return image, err
		}
		return image, os.ErrPermission
	})
	if !errors.Is(err, os.ErrPermission) || pid <= 0 {
		t.Fatalf("image failure control did not arm: %v", err)
	}
	meta, err := workerstate.ReadMeta(dir)
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	all, err := legacyFixtureProcesses()
	if err != nil {
		t.Fatal(err)
	}
	if legacyProcessesLive(all, map[int]bool{pid: true, meta.PID: true}) {
		t.Fatal("failed probe left worker or shell alive")
	}
	if _, err = os.Stat(state); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cleaned fixture state remains", err)
	}
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
