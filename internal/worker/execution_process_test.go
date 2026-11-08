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

func TestLiveChildWaitDoesNotHoldSignalGate(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/sleep", "30")
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })
	w := &Worker{cmd: command}
	result := make(chan error, 1)
	go func() { _, err := w.waitChild(); result <- err }()
	outcome, err := w.deliverSignal("", "term")
	if err != nil || outcome != "delivered" {
		t.Fatalf("signal blocked behind live wait: %s %v", outcome, err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("sole reaper did not join after admitted signal")
	}
}

func TestStoppedChildDoesNotSatisfyExitObserver(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/sleep", "30")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Signal(syscall.SIGCONT); _ = command.Process.Kill(); _ = command.Wait() })
	if err := command.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	observed := make(chan error, 1)
	go func() { observed <- observeChildExit(command.Process.Pid) }()
	select {
	case err := <-observed:
		t.Fatalf("stopped child reported as terminated: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := command.Process.Signal(syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-observed:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, err := command.Process.Wait(); err != nil {
		t.Fatal(err)
	}
}
