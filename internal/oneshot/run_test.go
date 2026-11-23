package oneshot

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// hangCap only catches a run that never ends; it judges no speed.
const hangCap = 30 * time.Second

func shellSpec(t *testing.T, script string, limit int, timeout time.Duration) Spec {
	t.Helper()
	return Spec{Path: "/bin/sh", Args: []string{"/bin/sh", "-c", script}, Dir: t.TempDir(), Env: os.Environ(), OutputBytes: limit, Timeout: timeout}
}

func runToEnd(t *testing.T, spec Spec) Result {
	t.Helper()
	run, err := Start(spec)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	return waitRun(t, run)
}

func waitRun(t *testing.T, run *Run) Result {
	t.Helper()
	select {
	case <-run.Done():
	case <-time.After(hangCap):
		t.Fatal("run did not finish")
	}
	return run.Result()
}

func exitCode(t *testing.T, result Result) int {
	t.Helper()
	if result.WaitErr != nil || result.State == nil {
		t.Fatalf("wait failed: %v", result.WaitErr)
	}
	return result.State.ExitCode()
}

func signalOf(t *testing.T, result Result) syscall.Signal {
	t.Helper()
	status, ok := result.State.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		t.Fatalf("leader was not signalled: %v", result.State)
	}
	return status.Signal()
}

func TestRunReportsExitAndBothStreams(t *testing.T) {
	result := runToEnd(t, shellSpec(t, "printf out; printf err >&2; exit 7", 1024, hangCap))
	if code := exitCode(t, result); code != 7 {
		t.Fatalf("exit code = %d, want 7", code)
	}
	if string(result.Stdout.Bytes) != "out" || string(result.Stderr.Bytes) != "err" {
		t.Fatalf("output = %q / %q", result.Stdout.Bytes, result.Stderr.Bytes)
	}
	if result.TimedOut || result.Stdout.CloseReason != CloseEOF || result.Stderr.CloseReason != CloseEOF {
		t.Fatalf("result = %+v", result)
	}
}

func TestRunKeepsHeadAndCountsTheRest(t *testing.T) {
	result := runToEnd(t, shellSpec(t, "head -c 200000 /dev/zero | tr '\\0' x", 1000, hangCap))
	if len(result.Stdout.Bytes) != 1000 || result.Stdout.Total != 200000 || !result.Stdout.Truncated {
		t.Fatalf("stdout kept %d of %d, truncated=%t", len(result.Stdout.Bytes), result.Stdout.Total, result.Stdout.Truncated)
	}
	if exitCode(t, result) != 0 {
		t.Fatal("writer blocked or failed on a full pipe")
	}
}

func TestRunKeepsBinaryBytesExactly(t *testing.T) {
	result := runToEnd(t, shellSpec(t, "printf '\\000\\377\\033[2J\\r\\n'", 64, hangCap))
	if want := []byte{0, 0xff, 0x1b, '[', '2', 'J', '\r', '\n'}; !bytes.Equal(result.Stdout.Bytes, want) {
		t.Fatalf("stdout = %q, want %q", result.Stdout.Bytes, want)
	}
}

func TestDeadlineTerminatesAndReportsTheLeaderSeparately(t *testing.T) {
	result := runToEnd(t, shellSpec(t, "echo started; exec sleep 30", 1024, 200*time.Millisecond))
	if !result.TimedOut {
		t.Fatal("deadline was not reported")
	}
	if sig := signalOf(t, result); sig != syscall.SIGTERM {
		t.Fatalf("leader signal = %v, want SIGTERM", sig)
	}
	if string(result.Stdout.Bytes) != "started\n" {
		t.Fatalf("partial output = %q", result.Stdout.Bytes)
	}
}

func TestDeadlineKeepsEscalatingAfterTheLeaderExits(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "member")
	// The member ignores TERM and HUP (exec keeps ignored signals) and holds
	// stdout; the leader exits on the first signal.
	script := `sh -c "trap '' TERM HUP; exec sleep 30" & echo $! > ` + pidFile + `; sleep 30`
	spec := shellSpec(t, script, 1024, 300*time.Millisecond)
	run, err := Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	member := readPID(t, pidFile)
	result := waitRun(t, run)
	if !result.TimedOut || signalOf(t, result) != syscall.SIGTERM {
		t.Fatalf("leader result = %+v", result)
	}
	assertGone(t, member)
}

// The deadline still applies after the leader exits, while a member holds
// stdout open; the leader's own status is kept.
func TestDeadlineAppliesDuringTheDrain(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "member")
	script := "sleep 30 & echo $! > " + pidFile + "; echo hi"
	run, err := Start(shellSpec(t, script, 1024, 100*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	member := readPID(t, pidFile)
	result := waitRun(t, run)
	if !result.TimedOut || exitCode(t, result) != 0 || string(result.Stdout.Bytes) != "hi\n" {
		t.Fatalf("result = %+v", result)
	}
	assertGone(t, member)
}

// Cancelling during the drain hangs the group up before killing it.
func TestCancelDuringTheDrainHangsUpTheGroup(t *testing.T) {
	dir := t.TempDir()
	marker, leaderDone := filepath.Join(dir, "hup"), filepath.Join(dir, "leader-done")
	member := `(trap 'echo hup > ` + marker + `; exit' HUP; while :; do echo x; sleep 0.05; done) &`
	draining := watchDrainStart(t)
	run, err := Start(shellSpec(t, member+" touch "+leaderDone, 1024, hangCap))
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return fileExists(leaderDone) })
	// The leader has exited and the drain began; the member keeps writing.
	select {
	case <-draining:
	case <-time.After(hangCap):
		t.Fatal("the drain never started")
	}
	run.Cancel()
	result := waitRun(t, run)
	if result.TimedOut || exitCode(t, result) != 0 {
		t.Fatalf("result = %+v", result)
	}
	eventually(t, func() bool { return fileExists(marker) })
}

// A silent member whose hang-up handler takes longer than the drain's
// silence window still has stdout when it writes, because during the
// grace the cleanup loop, not the silence timer, decides when pipes close.
func TestSilentMemberKeepsStdoutThroughItsHangUp(t *testing.T) {
	dir := t.TempDir()
	trapped, wrote := filepath.Join(dir, "trapped"), filepath.Join(dir, "wrote")
	member := `(trap 'sleep 0.35; echo late; touch ` + wrote + `; exit' HUP; touch ` + trapped + `; while :; do sleep 0.05; done) &`
	// The leader exits only once the member's trap is installed.
	leader := ` while [ ! -e ` + trapped + ` ]; do sleep 0.01; done`
	draining := watchDrainStart(t)
	run, err := Start(shellSpec(t, member+leader, 1024, hangCap))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-draining:
	case <-time.After(hangCap):
		t.Fatal("the drain never started")
	}
	run.Cancel()
	waitRun(t, run)
	if !fileExists(wrote) {
		t.Fatal("the member lost stdout during its hang-up grace")
	}
}

func TestNormalExitCleansUpTheGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "member")
	script := "sleep 30 >/dev/null 2>&1 & echo $! > " + pidFile + "; echo done"
	result := runToEnd(t, shellSpec(t, script, 1024, hangCap))
	if exitCode(t, result) != 0 || result.TimedOut {
		t.Fatalf("result = %+v", result)
	}
	assertGone(t, readPID(t, pidFile))
}

func TestDescendantHoldingStdoutIsCutOff(t *testing.T) {
	result := runToEnd(t, shellSpec(t, "sleep 30 & echo done", 1024, hangCap))
	if exitCode(t, result) != 0 || string(result.Stdout.Bytes) != "done\n" {
		t.Fatalf("result = %+v", result)
	}
	if result.Stdout.CloseReason != CloseDrainTimeout {
		t.Fatalf("stdout close reason = %q, want %q", result.Stdout.CloseReason, CloseDrainTimeout)
	}
}

func TestCancelHangsUpTheGroup(t *testing.T) {
	run, err := Start(shellSpec(t, "exec sleep 30", 1024, hangCap))
	if err != nil {
		t.Fatal(err)
	}
	run.Cancel()
	result := waitRun(t, run)
	if result.TimedOut || signalOf(t, result) != syscall.SIGHUP {
		t.Fatalf("result = %+v", result)
	}
}

func TestStartFailureStartsNothing(t *testing.T) {
	spec := shellSpec(t, "true", 16, hangCap)
	spec.Dir = filepath.Join(spec.Dir, "missing")
	if _, err := Start(spec); err == nil {
		t.Fatal("start in a missing directory succeeded")
	}
}

func TestLookPathUsesTheRunEnvironment(t *testing.T) {
	dir := t.TempDir()
	tool := filepath.Join(dir, "only-here")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // the fixture tool must be executable
		t.Fatal(err)
	}
	got, err := LookPath("only-here", []string{"PATH=relative:" + dir})
	if err != nil || got != tool {
		t.Fatalf("LookPath = %q, %v", got, err)
	}
	if _, err := LookPath("only-here", []string{"PATH=/nonexistent"}); err == nil {
		t.Fatal("found a tool outside the run's PATH")
	}
	if got, _ := LookPath("./x", nil); got != "./x" {
		t.Fatalf("a path with a slash was searched: %q", got)
	}
	// A candidate this process may not execute is passed over.
	denied := t.TempDir()
	if err := os.WriteFile(filepath.Join(denied, "only-here"), []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := LookPath("only-here", []string{"PATH=" + denied + ":" + dir}); err != nil || got != tool {
		t.Fatalf("LookPath past a denied candidate = %q, %v", got, err)
	}
}

// A deadline due at the same moment the drain ends is still reported. The
// loop sees both ready at once, many times over.
func TestADeadlineDueAtTheEndOfTheDrainIsReported(t *testing.T) {
	grace = time.Millisecond
	t.Cleanup(func() { grace = Grace })
	for range 50 {
		member := exec.Command("sleep", "30")
		member.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := member.Start(); err != nil {
			t.Fatal(err)
		}
		deadline := make(chan time.Time, 1)
		deadline <- time.Now()
		drained := make(chan struct{})
		close(drained)
		c := &cleanup{group: member.Process.Pid, deadline: deadline, drained: drained, leaderGone: true, stopDrain: make(chan struct{}), gate: newCutGate()}
		c.run()
		_ = member.Wait()
		if !c.timedOut || !c.killed {
			t.Fatalf("a due deadline was dropped: timedOut=%t killed=%t", c.timedOut, c.killed)
		}
	}
}

// watchDrainStart installs the drain-start hook for this test and returns
// the channel it signals.
func watchDrainStart(t *testing.T) <-chan struct{} {
	t.Helper()
	draining := make(chan struct{}, 1)
	hook := func() {
		select {
		case draining <- struct{}{}:
		default:
		}
	}
	drainStartingHook.Store(&hook)
	t.Cleanup(func() { drainStartingHook.Store(nil) })
	return draining
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(hangCap)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition never held")
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(hangCap)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path) //nolint:gosec // the pid file belongs to this test's temporary directory
		if pid, convErr := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && convErr == nil {
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no pid in %s", path)
	return 0
}

// assertGone waits for the member to disappear; init reaps it once it dies.
func assertGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(hangCap)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("group member %d outlived its run", pid)
}
