//go:build linux && amd64

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestGroupExitCancelsCloneNotification(t *testing.T) {
	worker := os.Getenv("CHILDSTAGE_LIFECYCLE_WORKER")
	if worker == "" {
		t.Skip("requires the separately built Go 1.27.0 lifecycle control")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	buffer := &bytes.Buffer{}
	o := &observer{output: json.NewEncoder(buffer), started: time.Now(), tasks: make(map[int]*task), limit: 16384,
		images: make(map[string]*os.File), digests: make(map[string]map[string]any)}
	defer o.teardown()
	pid, err := syscall.ForkExec(worker, []string{worker, "lifecycle-thread-exit"}, &syscall.ProcAttr{Env: os.Environ(),
		Files: []uintptr{0, 1, 2}, Sys: &syscall.SysProcAttr{Ptrace: true}}) // #nosec G204 G702 -- Local control driver supplies only its separately built synthetic worker, never a remote input.
	if err != nil {
		t.Fatal(err)
	}
	o.root = pid
	o.tasks[pid] = &task{Known: true, TGID: pid, Owner: pid, InheritedReturn: true}
	if err := o.arm(pid); err != nil {
		t.Fatal(err)
	}
	o.tasks[pid].Image = o.bindImage(pid)
	o.resume(pid, 0)
	child := killAtCloneEvent(t, o)
	o.loop(10 * time.Second)
	t.Logf("cancelled clone child %d, trace:\n%s", child, buffer.String())
	if o.err != nil || !o.done || len(o.tasks) != 0 {
		t.Fatalf("group exit left %d tasks, done %v: %v", len(o.tasks), o.done, o.err)
	}
}

func TestUnstartedExitWaitsForEveryPossibleCreator(t *testing.T) {
	o, _ := testObserver()
	o.tasks[123].TGID, o.tasks[123].Exiting = 123, true
	o.tasks[124] = &task{Known: true, TGID: 123}
	if o.groupExiting(123) {
		t.Fatal("live creator's pending notification was discarded")
	}
	o.tasks[124].Exiting = true
	if !o.groupExiting(123) {
		t.Fatal("confirmed group exit was not recognized")
	}
}

func TestUnstartedExitCannotResumeToAnUncoveredSyscall(t *testing.T) {
	o, _ := testObserver()
	task := &task{ExitOnly: true}
	stop := syscall.WaitStatus((int(unix.SIGTRAP|0x80) << 8) | 0x7f)
	o.parkUnidentified(124, task, stop)
	if o.err == nil {
		t.Fatal("exit-only task was allowed to reach a syscall stop")
	}
}

func TestRootExitStillWaitsForUnstartedTerminalProof(t *testing.T) {
	o, _ := testObserver()
	o.done = true
	o.tasks[124] = &task{ExitOnly: true}
	if !o.waitingForLifecycle() {
		t.Fatal("root exit discarded an unstarted thread's pending terminal wait")
	}
	delete(o.tasks, 124)
	if o.waitingForLifecycle() {
		t.Fatal("ordinary known descendants extended observation after root exit")
	}
}

func TestOrdinaryThreadExitCannotAuthorizeUnstartedThreadKill(t *testing.T) {
	task := &task{TGID: 123, Call: &call{Number: unix.SYS_EXIT}}
	if endingGroup(123, task, 0) {
		t.Fatal("leader's thread-only exit was treated as group termination")
	}
	if !endingGroup(123, task, uint(unix.SIGKILL)) {
		t.Fatal("leader's fatal group signal was not recognized")
	}
	task.Call.Number = unix.SYS_EXIT_GROUP
	if !endingGroup(123, task, 0) || endingGroup(124, task, 0) {
		t.Fatal("exit_group proof did not require the leader identity")
	}
}

func killAtCloneEvent(t *testing.T, o *observer) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && o.err == nil {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &status, syscall.WALL|syscall.WNOHANG, nil)
		if err != nil {
			t.Fatal(err)
		}
		if pid == 0 {
			time.Sleep(50 * time.Microsecond)
			continue
		}
		if status.Stopped() && status.TrapCause() == unix.PTRACE_EVENT_CLONE {
			return cancelCloneEvent(t, o, pid)
		}
		o.handle(pid, status)
	}
	t.Fatalf("owned worker did not reach a clone event: %v", o.err)
	return 0
}

func cancelCloneEvent(t *testing.T, o *observer, creator int) int {
	t.Helper()
	message, err := unix.PtraceGetEventMsg(creator)
	if err != nil {
		t.Fatal(err)
	}
	child := int(message)
	if taskTGID(child) != o.root {
		t.Fatal("control clone did not create an owned Go thread")
	}
	// A group exit can withdraw the creator's pending clone notification while
	// its automatically attached child still has an initial stop to report.
	if err := unix.Kill(o.root, unix.SIGKILL); err != nil {
		t.Fatal(err)
	}
	return child
}
