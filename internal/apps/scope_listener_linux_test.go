//go:build linux

package apps

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/worker"

	"golang.org/x/sys/unix"
)

const scopeListenerMode = "MESH_TEST_SCOPE_LISTENER_MODE"

type scopeListenerReceipt struct {
	PID         int
	Session     int
	Port        int
	Cgroup      string
	Unavailable bool
}

func TestScopeListenerHelper(t *testing.T) {
	mode := os.Getenv(scopeListenerMode)
	if mode == "" {
		t.Skip("helper for the supervised app listener test")
	}
	path := os.Getenv("MESH_TEST_SCOPE_LISTENER_RECEIPT")
	if mode == "leader" {
		runScopeListenerLeader(t, path)
		return
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	pid := os.Getpid()
	session, err := unix.Getsid(pid)
	if err != nil {
		t.Fatal(err)
	}
	cgroup, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		t.Fatal(err)
	}
	writeScopeListenerReceipt(t, path, scopeListenerReceipt{
		PID: pid, Session: session, Port: listener.Addr().(*net.TCPAddr).Port, Cgroup: string(cgroup),
	})
	time.Sleep(30 * time.Second)
}

func runScopeListenerLeader(t *testing.T, path string) {
	t.Helper()
	id := os.Getenv("MESH_TEST_SCOPE_LISTENER_ID")
	worker.IsolateSession(id)
	cgroup, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(strings.TrimSpace(string(cgroup)), "/mesh-session-"+id+".scope") {
		writeScopeListenerReceipt(t, path, scopeListenerReceipt{Unavailable: true})
		return
	}
	plain := exec.Command(os.Args[0], "-test.run=^TestScopeListenerHelper$") //nolint:gosec // listener in the app cgroup with a separate kernel session
	plain.Env = append(os.Environ(), scopeListenerMode+"=listener", "MESH_TEST_SCOPE_LISTENER_RECEIPT="+path+".plain")
	plain.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	plain.Stdout, plain.Stderr = os.Stdout, os.Stderr
	if err := plain.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = plain.Process.Kill(); _ = plain.Wait() }()
	args := []string{"--user", "--scope", "--quiet", "--expand-environment=no", "--unit=mesh-test-listener-" + id}
	group := strings.TrimPrefix(strings.TrimSpace(string(cgroup)), "0::")
	slice := filepath.Base(filepath.Dir(group))
	if strings.HasSuffix(slice, ".slice") {
		args = append(args, "--slice="+slice)
	}
	args = append(args, os.Args[0], "-test.run=^TestScopeListenerHelper$")
	cmd := exec.Command("systemd-run", args...) //nolint:gosec // launches this test's listener in a uniquely owned scope
	cmd.Env = append(os.Environ(), scopeListenerMode+"=listener")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
}

func writeScopeListenerReceipt(t *testing.T, path string, receipt scopeListenerReceipt) {
	t.Helper()
	contents, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".new", contents, 0o600); err != nil { //nolint:gosec // parent supplies a path in its private test directory
		t.Fatal(err)
	}
	if err := os.Rename(path+".new", path); err != nil { //nolint:gosec // parent supplies both paths in its private test directory
		t.Fatal(err)
	}
}

func TestAppListenerRetainsOwnershipAcrossSystemdScopes(t *testing.T) {
	for _, program := range []string{"busctl", "systemd-run", "systemctl"} {
		if _, err := exec.LookPath(program); err != nil {
			t.Skipf("supervised listener requires %s: %v", program, err)
		}
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "receipt.json")
	id := "listener-" + strconv.Itoa(os.Getpid())
	log, err := os.Create(filepath.Join(dir, "fixture.log")) //nolint:gosec // fixed file inside t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	cmd := exec.Command(os.Args[0], "-test.run=^TestScopeListenerHelper$") //nolint:gosec // re-runs this test binary as a helper
	cmd.Env = append(os.Environ(), scopeListenerMode+"=leader", "MESH_TEST_SCOPE_LISTENER_ID="+id, "MESH_TEST_SCOPE_LISTENER_RECEIPT="+path)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		stopListenerScope(t, "mesh-test-listener-"+id+".scope")
		stopListenerScope(t, "mesh-session-"+id+".scope")
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
	})
	receipt := awaitScopeListenerReceipt(t, path, filepath.Join(dir, "fixture.log"))
	if receipt.Unavailable {
		t.Skip("supervised listener requires a working systemd user scope manager")
	}
	if receipt.Session != cmd.Process.Pid || !strings.HasSuffix(strings.TrimSpace(receipt.Cgroup), "/mesh-test-listener-"+id+".scope") {
		t.Fatalf("listener did not keep the app session in its separate scope: %+v, leader %d", receipt, cmd.Process.Pid)
	}
	processes := func() ([]int, error) { return worker.SessionProcesses(id, cmd.Process.Pid) }
	plain := awaitScopeListenerReceipt(t, path+".plain", filepath.Join(dir, "fixture.log"))
	if plain.Session == cmd.Process.Pid || !strings.HasSuffix(strings.TrimSpace(plain.Cgroup), "/mesh-session-"+id+".scope") {
		t.Fatalf("positive control did not stay in the app scope with a separate session: %+v", plain)
	}
	if _, err := checkServerListener(context.Background(), plain.Port, processes); err != nil {
		t.Fatalf("known-good app-cgroup listener rejected: %v", err)
	}
	if _, err := checkServerListener(context.Background(), receipt.Port, processes); err != nil {
		t.Fatalf("supervised app listener rejected: %v", err)
	}
	members, err := processes()
	if err != nil || !slices.Contains(members, receipt.PID) || !slices.Contains(members, plain.PID) || slices.Contains(members, os.Getpid()) {
		t.Fatalf("app processes = %v, %v; need scoped listener %d and exclude caller %d", members, err, receipt.PID, os.Getpid())
	}
	outside, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = outside.Close() })
	if _, err := checkServerListener(context.Background(), outside.Addr().(*net.TCPAddr).Port, processes); err == nil {
		t.Fatal("listener in an unrelated kernel session was admitted")
	}
}

func awaitScopeListenerReceipt(t *testing.T, path, logPath string) scopeListenerReceipt {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		contents, err := os.ReadFile(path) //nolint:gosec // fixed receipt inside this test's private directory
		if err == nil {
			var receipt scopeListenerReceipt
			if err := json.Unmarshal(contents, &receipt); err != nil {
				t.Fatal(err)
			}
			return receipt
		}
		time.Sleep(20 * time.Millisecond)
	}
	log, _ := os.ReadFile(logPath) //nolint:gosec // fixed log inside this test's private directory
	t.Fatalf("supervised listener did not publish readiness: %s", log)
	return scopeListenerReceipt{}
}

func stopListenerScope(t *testing.T, unit string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, "systemctl", "--user", "stop", unit).Run() //nolint:gosec // only this fixture's uniquely named scopes
}
