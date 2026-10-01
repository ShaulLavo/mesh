package daemon

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/storage"
)

// startedWorkerVariable makes the test binary stand in for a session-worker
// that starts, opens its socket and clears its launching marker, but never
// writes metadata: the real launcher then fails after the process started.
const startedWorkerVariable = "MESH_DAEMON_TEST_WORKER"

func TestMain(m *testing.M) {
	if os.Getenv(startedWorkerVariable) == "started" {
		runStartedWorkerFixture(os.Args[1:])
		return
	}
	os.Exit(m.Run())
}

func runStartedWorkerFixture(args []string) {
	dir := ""
	for index, arg := range args {
		if arg == "--dir" && index+1 < len(args) {
			dir = args[index+1]
		}
	}
	listener, err := net.Listen("unix", paths.Socket(dir))
	if err != nil {
		os.Exit(2)
	}
	if os.WriteFile(filepath.Join(dir, "fixture.pid"), []byte(strconv.Itoa(os.Getpid())), 0o600) != nil { //nolint:gosec // dir is the launcher's own --dir argument
		os.Exit(2)
	}
	if os.Remove(paths.Launching(dir)) != nil { //nolint:gosec // dir is the launcher's own --dir argument
		os.Exit(2)
	}
	// Accepting until the test kills it keeps the worker alive without a timer.
	for {
		connection, err := listener.Accept()
		if err != nil {
			os.Exit(0)
		}
		_ = connection.Close()
	}
}

func TestLabelledLaunchFailureAfterStartKeepsWorkerIdentity(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	sessionsDir := t.TempDir()
	lifecycle := mustLifecycle(t, lifecycleConfig{
		Catalog:     &lifecycleTestCatalog{},
		Connector:   failingLifecycleConnector(),
		Host:        storage.Host{ID: "host-a", MeshIdentity: "mesh-key", LastSeenAt: time.Now()},
		SessionsDir: sessionsDir,
		Executable:  executable,
	})
	// The launcher starts the worker in the session's directory, which must
	// exist on every machine the test runs on.
	id, err := lifecycle.startLabelled(context.Background(), "serve /dev", []string{"sh", "-lc", "vite"}, t.TempDir(),
		[]string{startedWorkerVariable + "=started"})
	entries, _ := os.ReadDir(sessionsDir)
	started := ""
	for _, entry := range entries {
		text, readErr := os.ReadFile(filepath.Join(sessionsDir, entry.Name(), "fixture.pid")) //nolint:gosec // a fixture file under the test's own sessions directory
		if readErr != nil {
			continue
		}
		pid, scanErr := strconv.Atoi(string(text))
		if scanErr == nil && pid > 0 {
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
			started = entry.Name()
		}
	}
	if started == "" {
		t.Fatalf("the fixture worker never started, so this is not a post-start failure: startLabelled = %q, %v", id, err)
	}
	if err == nil {
		t.Fatalf("startLabelled succeeded for worker %s, which never wrote metadata", started)
	}
	if id != started {
		t.Fatalf("worker %s started and is still running, but startLabelled returned %q (%v)", started, id, err)
	}
}
