//go:build linux

package main

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/testenv"
	"github.com/shaul/mesh/internal/worker"
)

const runMainVariable = "MESH_TEST_RUN_MAIN"

// The worker tests launch this test binary as `mesh session-worker`, so the
// process under test goes through main's real dispatch, not a test helper.
func TestMain(m *testing.M) {
	if os.Getenv(runMainVariable) == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestSessionWorkerDispatchesWithoutFang(t *testing.T) {
	if !plainAgentCommand([]string{"session-worker", "--id", "7K3D", "--dir", "/state/sessions/7K3D", "--", "sh"}) {
		t.Fatal("session-worker runs under Fang, so its signal policy is whatever Fang's options happen to be")
	}
}

type launchedWorker struct {
	worker.Launched
	root string
	// Meta.PID is the session command; the worker is its parent.
	childPID, workerPID int
}

// launchSignalWorker starts a real detached worker whose systemd scope request
// fails, so the test never reaches the host's user manager.
func launchSignalWorker(t *testing.T) launchedWorker {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	busctl := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"" + filepath.Join(root, "busctl.calls") + "\"\necho 'fixture: no user manager' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "busctl"), []byte(busctl), 0o700); err != nil { //nolint:gosec // the fixture must be executable
		t.Fatal(err)
	}
	env := testenv.ForProcess(root)
	for i, entry := range env {
		if value, ok := strings.CutPrefix(entry, "PATH="); ok {
			env[i] = "PATH=" + bin + ":" + value
		}
	}
	env = append(env, runMainVariable+"=1")
	launched, err := worker.LaunchDetached(worker.LaunchConfig{
		SessionsDir: filepath.Join(root, "state", "sessions"),
		Executable:  os.Args[0],
		Command:     []string{"/bin/sleep", "300"},
		Cwd:         root,
		Env:         env,
	})
	if err != nil {
		t.Fatalf("launch worker: %v", err)
	}
	w := launchedWorker{Launched: launched, root: root}
	t.Cleanup(func() {
		_ = syscall.Kill(launched.Meta.PID, syscall.SIGKILL)
		if w.workerPID > 0 {
			_ = syscall.Kill(w.workerPID, syscall.SIGKILL)
		}
	})
	w.childPID = launched.Meta.PID
	w.workerPID = parentPID(t, w.childPID)
	return w
}

func parentPID(t *testing.T, pid int) int {
	t.Helper()
	status, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(string(status), "\n") {
		if value, ok := strings.CutPrefix(line, "PPid:"); ok {
			parent, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				t.Fatal(err)
			}
			return parent
		}
	}
	t.Fatalf("process %d has no parent in its status", pid)
	return 0
}

func (w launchedWorker) assertServing(t *testing.T, after string) {
	t.Helper()
	if err := syscall.Kill(w.workerPID, 0); err != nil {
		t.Fatalf("worker exited after %s: %v (log: %s)", after, err, readLog(w.Dir))
	}
	conn, err := net.DialTimeout("unix", paths.Socket(w.Dir), time.Second)
	if err != nil {
		t.Fatalf("worker stopped serving after %s: %v", after, err)
	}
	_ = conn.Close()
	if err := syscall.Kill(w.childPID, 0); err != nil {
		t.Fatalf("session command exited after %s: %v", after, err)
	}
}

func readLog(dir string) string {
	data, _ := os.ReadFile(paths.Log(dir))
	return strings.TrimSpace(string(data))
}

// A worker is setsid'd and has no terminal, so SIGINT and SIGHUP can only come
// from someone else's terminal or a stray kill, and SIGTERM from a supervisor
// that is stopping the daemon, not the session. None of them may end it.
func TestSessionWorkerSurvivesTerminationSignals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		signal syscall.Signal
	}{
		{"SIGINT", syscall.SIGINT},
		{"SIGHUP", syscall.SIGHUP},
		{"SIGTERM", syscall.SIGTERM},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := launchSignalWorker(t)
			for range 3 {
				if err := syscall.Kill(w.workerPID, tc.signal); err != nil {
					t.Fatalf("worker exited after %s: %v (log: %s)", tc.name, err, readLog(w.Dir))
				}
				time.Sleep(100 * time.Millisecond)
			}
			w.assertServing(t, tc.name)
		})
	}
}

// Catching a signal resets it to the default in an exec'd child; ignoring it
// does not. The session's command must see Ctrl-C and hangups normally.
func TestSessionCommandInheritsDefaultSignalDispositions(t *testing.T) {
	w := launchSignalWorker(t)
	status, err := os.ReadFile("/proc/" + strconv.Itoa(w.Meta.PID) + "/status")
	if err != nil {
		t.Fatal(err)
	}
	masks := make(map[string]uint64)
	for line := range strings.SplitSeq(string(status), "\n") {
		name, mask, _ := strings.Cut(line, ":")
		if name != "SigIgn" && name != "SigBlk" {
			continue
		}
		bits, err := strconv.ParseUint(strings.TrimSpace(mask), 16, 64)
		if err != nil {
			t.Fatalf("%s mask %q: %v", name, mask, err)
		}
		masks[name] = bits
	}
	for _, name := range []string{"SigIgn", "SigBlk"} {
		bits, ok := masks[name]
		if !ok {
			t.Errorf("session command status is missing %s", name)
			continue
		}
		for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGQUIT} {
			if bits&(1<<(uint(sig)-1)) != 0 {
				t.Errorf("session command starts with %v in %s (%016x)", sig, name, bits)
			}
		}
	}
}

// When the user manager will not create the session's scope, the worker keeps
// its launcher's cgroup and runs the session anyway. That placement is safe
// across daemon restarts only because mesh.service uses KillMode=process.
func TestSessionWorkerRunsInPlaceWhenScopeCreationFails(t *testing.T) {
	before, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		t.Skipf("no cgroup information: %v", err)
	}
	w := launchSignalWorker(t)
	calls, err := os.ReadFile(filepath.Join(w.root, "busctl.calls"))
	if err != nil || !strings.Contains(string(calls), "StartTransientUnit") || !strings.Contains(string(calls), "mesh-session-"+w.Meta.ID+".scope") {
		t.Fatalf("worker did not ask for its own scope through busctl: %q, %v", calls, err)
	}
	if log := readLog(w.Dir); !strings.Contains(log, "own scope mesh-session-"+w.Meta.ID+".scope") {
		t.Fatalf("scope failure was not logged: %q", log)
	}
	after, err := os.ReadFile("/proc/" + strconv.Itoa(w.workerPID) + "/cgroup")
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("worker cgroup = %q, want its launcher's %q", after, before)
	}
	w.assertServing(t, "a failed scope request")
}
