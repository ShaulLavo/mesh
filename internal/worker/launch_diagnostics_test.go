package worker

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/paths"
)

func TestReadinessTimeoutCarriesSafePendingFacts(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(paths.Launching(dir), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteMeta(dir, Meta{State: StateDetached, Command: []string{"SECRET_COMMAND"}}); err != nil {
		t.Fatal(err)
	}
	log := "2026/10/02 00:00:00 worker: own scope mesh-session-7K3D.scope: SECRET_ENV\nworker: open pty: SECRET_PTY\n"
	if err := os.WriteFile(paths.Log(dir), []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}
	err := waitForWorker(dir, 20*time.Millisecond, nil)
	if err == nil {
		t.Fatal("unpublished worker counted as ready")
	}
	for _, fact := range []string{"worker is still publishing state", "elapsed=", "deadline=", "phase=launch-marker-present", "workerPID=", "workerAlive=", "metaState=detached", "startupLog=scope-call-failed,pty-open-failed"} {
		if !strings.Contains(err.Error(), fact) {
			t.Errorf("timeout omitted %q: %v", fact, err)
		}
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("timeout disclosed command, environment or log bytes: %v", err)
	}
}

func TestReadinessTimeoutNamesOriginalWorkerHandle(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(paths.Launching(dir), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("/bin/sleep", "30")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	}()
	err := waitForWorker(dir, 20*time.Millisecond, command.Process)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("workerPID=%d workerAlive=true", command.Process.Pid)) {
		t.Fatalf("timeout lost the original worker handle: %v", err)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()
	err = waitForWorker(dir, 20*time.Millisecond, command.Process)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("workerPID=%d workerAlive=false", command.Process.Pid)) {
		t.Fatalf("reaped worker counted as alive: %v", err)
	}
}

func TestReadinessTimeoutPreservesDialCause(t *testing.T) {
	dir := t.TempDir()
	if err := WriteMeta(dir, Meta{State: StateRunning}); err != nil {
		t.Fatal(err)
	}
	err := waitForWorker(dir, 20*time.Millisecond, nil)
	var dial *net.OpError
	if !errors.As(err, &dial) {
		t.Fatalf("timeout lost socket dial cause: %v", err)
	}
	for _, fact := range []string{"phase=socket-dial-failing", "metaState=running", "startupLog=unavailable"} {
		if !strings.Contains(err.Error(), fact) {
			t.Errorf("timeout omitted %q: %v", fact, err)
		}
	}
}

func TestReadinessTimeoutBoundsAndFiltersOwnFiles(t *testing.T) {
	for _, kind := range []string{"long", "symlink", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(paths.Launching(dir), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(`{"state":"SECRET_STATE","command":["SECRET_COMMAND"]}`), 0o600); err != nil {
				t.Fatal(err)
			}
			log := paths.Log(dir)
			want := "unavailable"
			switch kind {
			case "long":
				want = "unclassified"
				data := "worker: open pty: SECRET_PREFIX\n" + strings.Repeat("SECRET_TAIL", 4096)
				if err := os.WriteFile(log, []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target := filepath.Join(t.TempDir(), "unrelated.log")
				if err := os.WriteFile(target, []byte("worker: open pty: SECRET_UNRELATED\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, log); err != nil {
					t.Fatal(err)
				}
				metadata := filepath.Join(t.TempDir(), "unrelated.json")
				if err := os.WriteFile(metadata, []byte(`{"state":"running"}`), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Join(dir, "meta.json")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(metadata, filepath.Join(dir, "meta.json")); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := syscall.Mkfifo(log, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Join(dir, "meta.json")); err != nil {
					t.Fatal(err)
				}
				if err := syscall.Mkfifo(filepath.Join(dir, "meta.json"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			started := time.Now()
			err := waitForWorker(dir, 20*time.Millisecond, nil)
			if time.Since(started) > time.Second {
				t.Fatal("timeout diagnostics blocked on a fixture file")
			}
			if err == nil || !strings.Contains(err.Error(), "metaState=unknown") || !strings.Contains(err.Error(), fmt.Sprintf("startupLog=%s", want)) {
				t.Fatalf("timeout omitted safe file classifications: %v", err)
			}
			if strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("timeout disclosed untrusted file content: %v", err)
			}
		})
	}
}
