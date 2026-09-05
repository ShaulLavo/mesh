package inhibit

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestManagerSharesLeaseAndReapsOnLastWorker(t *testing.T) {
	manager := New(func(err error) { t.Errorf("unexpected report: %v", err) })
	starts := 0
	manager.command = func() (*exec.Cmd, error) {
		starts++
		return exec.Command("/bin/cat"), nil
	}
	t.Cleanup(func() { _ = manager.Close() })
	manager.Update(false)
	if starts != 0 {
		t.Fatal("idle daemon started an inhibitor")
	}
	manager.Update(true)
	first := manager.lease
	for range 5 {
		manager.Update(true)
	}
	if starts != 1 || first == nil {
		t.Fatalf("active updates started %d inhibitors; lease = %v", starts, first)
	}
	manager.Update(false)
	if !first.finished() || first.command.ProcessState == nil {
		t.Fatal("last worker exit did not reap the inhibitor")
	}
	manager.Update(true)
	if starts != 2 {
		t.Fatalf("next worker started %d inhibitors total, want 2", starts)
	}
	second := manager.lease
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	manager.Update(true)
	if !second.finished() || starts != 2 {
		t.Fatal("Close did not release permanently")
	}
}

func TestManagerReportsMissingMechanismOnlyOnce(t *testing.T) {
	var reported []error
	manager := New(func(err error) { reported = append(reported, err) })
	starts := 0
	manager.command = func() (*exec.Cmd, error) {
		starts++
		return nil, errors.New("logind unavailable")
	}
	for range 10 {
		manager.Update(true)
		manager.Update(false)
	}
	if starts != 1 || len(reported) != 1 || !strings.Contains(reported[0].Error(), "logind unavailable") {
		t.Fatalf("starts = %d, reports = %v", starts, reported)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestManagerReportsUtilityThatCannotAcquireInhibitor(t *testing.T) {
	var reported []error
	manager := New(func(err error) { reported = append(reported, err) })
	manager.command = func() (*exec.Cmd, error) {
		return exec.Command("/bin/sh", "-c", "exit 42"), nil
	}
	t.Cleanup(func() { _ = manager.Close() })
	manager.Update(true)
	lease := manager.lease
	if lease == nil {
		t.Fatal("utility did not start")
	}
	waitForExit(t, lease)
	for range 5 {
		manager.Update(true)
	}
	if len(reported) != 1 || !strings.Contains(reported[0].Error(), "exit status 42") {
		t.Fatalf("reports = %v", reported)
	}
	if _, err := lease.input.Write([]byte("closed")); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("failed utility kept its lifetime pipe open: %v", err)
	}
}

func TestManagerMissingExecutableIsNonfatal(t *testing.T) {
	var reported []error
	manager := New(func(err error) { reported = append(reported, err) })
	missing := filepath.Join(t.TempDir(), "missing-inhibitor")
	manager.command = func() (*exec.Cmd, error) { return exec.Command(missing), nil }
	manager.Update(true)
	manager.Update(true)
	if len(reported) != 1 || !errors.Is(reported[0], os.ErrNotExist) {
		t.Fatalf("reports = %v", reported)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestManagerForcesAndReapsUncooperativeUtility(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "state")
	manager := New(func(err error) { t.Errorf("unexpected report: %v", err) })
	manager.timeout = 100 * time.Millisecond
	manager.command = func() (*exec.Cmd, error) {
		return exec.Command("/bin/sh", "-c", `printf held > "$1"; while :; do :; done`, "mesh-inhibit-test", marker), nil //nolint:gosec // Fixed script receives only a test temp path as a quoted positional argument.
	}
	t.Cleanup(func() { _ = manager.Close() })
	manager.Update(true)
	lease := manager.lease
	waitForMarker(t, marker, "held")
	started := time.Now()
	err := manager.Close()
	if err == nil || !strings.Contains(err.Error(), "lifetime pipe closed") {
		t.Fatalf("Close = %v, want forced cleanup report", err)
	}
	if time.Since(started) > time.Second || !lease.finished() {
		t.Fatal("uncooperative utility was not promptly killed and reaped")
	}
}

func TestDaemonSIGKILLClosesInhibitorLifetimePipe(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "state")
	command := exec.Command(os.Args[0], "-test.run=^TestInhibitorDaemonHelper$") //nolint:gosec // Re-exec the current test binary with a fixed helper selector.
	command.Env = append(os.Environ(), "MESH_INHIBITOR_HELPER="+marker)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	waitForMarker(t, marker, "held")
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("daemon helper was not killed")
	}
	waitForMarker(t, marker, "released")
}

func TestInhibitorDaemonHelper(t *testing.T) {
	marker := os.Getenv("MESH_INHIBITOR_HELPER")
	if marker == "" {
		return
	}
	manager := New(func(error) { os.Exit(2) })
	manager.command = func() (*exec.Cmd, error) {
		return exec.Command("/bin/sh", "-c", `printf held > "$1"; /bin/cat; printf released > "$1"`, "mesh-inhibit-test", marker), nil //nolint:gosec // The parent test supplies its temp marker as a quoted positional argument to a fixed script.
	}
	manager.Update(true)
	if manager.lease == nil {
		os.Exit(3)
	}
	<-manager.lease.done
	os.Exit(4)
}

func TestPlatformCommands(t *testing.T) {
	linux, err := platformCommand("linux")
	if err != nil {
		t.Fatal(err)
	}
	wantLinux := []string{"systemd-inhibit", "--no-ask-password", "--what=sleep:idle", "--mode=block", "--who=mesh", "--why=Mesh sessions are running", "/bin/cat"}
	if !reflect.DeepEqual(linux.Args, wantLinux) {
		t.Fatalf("Linux arguments = %v", linux.Args)
	}
	mac, err := platformCommand("darwin")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(mac.Args, []string{"/usr/bin/caffeinate", "-i", "/bin/cat"}) {
		t.Fatalf("macOS arguments = %v", mac.Args)
	}
	if _, err := platformCommand("unsupported"); err == nil {
		t.Fatal("unsupported platform reported an inhibitor")
	}
}

func TestRealMacInhibitorExitsOnRelease(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("requires macOS caffeinate")
	}
	manager := New(func(err error) { t.Errorf("caffeinate failed: %v", err) })
	manager.Update(true)
	if manager.lease == nil {
		t.Fatal("caffeinate did not start")
	}
	lease := manager.lease
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if !lease.finished() {
		t.Fatal("caffeinate utility was not reaped")
	}
}

func waitForExit(t *testing.T, lease *processLease) {
	t.Helper()
	select {
	case <-lease.done:
	case <-time.After(5 * time.Second):
		t.Fatal("inhibitor utility did not exit")
	}
}

func waitForMarker(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		contents, err := os.ReadFile(path) //nolint:gosec // The marker path is generated by t.TempDir and shared only with the test helper.
		if err == nil && string(contents) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	contents, err := os.ReadFile(path) //nolint:gosec // Read the same test temp marker to report a failed assertion.
	t.Fatalf("inhibitor state = %q (%v), want %q", contents, err, want)
}
