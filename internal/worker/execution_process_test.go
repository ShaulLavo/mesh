package worker

import (
	"context"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestChildExitObservationDoesNotReap(t *testing.T) {
	for _, fast := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "already_exited"}[fast], func(t *testing.T) { testChildExitObservation(t, fast) })
	}
}
func testChildExitObservation(t *testing.T, fast bool) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "exit 7")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if fast {
		time.Sleep(30 * time.Millisecond)
	}
	if err := observeChildExit(cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(cmd.Process.Pid, 0); err != nil {
		t.Fatalf("observer released child identifier: %v", err)
	}
	state, err := cmd.Process.Wait()
	if err != nil || state.ExitCode() != 7 {
		t.Fatalf("sole reap: %v %v", state, err)
	}
}
func TestSignalAndReapShareOperationGate(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "exit 0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	w := &Worker{cmd: cmd}
	if err := observeChildExit(cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	w.operationMu.Lock()
	done := make(chan struct{})
	go func() { _, _ = w.reapChild(); close(done) }()
	select {
	case <-done:
		t.Fatal("reaper crossed admitted operation")
	case <-time.After(20 * time.Millisecond):
	}
	if err := syscall.Kill(cmd.Process.Pid, 0); err != nil {
		t.Fatal("gate failed to retain child")
	}
	w.operationMu.Unlock()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	outcome, err := w.deliverSignal("", "kill")
	if err != nil || outcome != "already_retired" {
		t.Fatalf("post-reap delivery: %q %v", outcome, err)
	}
}
