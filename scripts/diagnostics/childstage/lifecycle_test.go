//go:build linux && amd64

package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func testObserver() (*observer, *bytes.Buffer) {
	buffer := &bytes.Buffer{}
	return &observer{output: json.NewEncoder(buffer), tasks: map[int]*task{123: {Known: true}}, limit: 100}, buffer
}

func TestOnlyAnUnreadRelevantReturnCanLoseCoverage(t *testing.T) {
	tests := []struct {
		name string
		task task
		want bool
	}{
		{"unresumed entry", task{}, false},
		{"inherited child exit", task{InheritedReturn: true}, false},
		{"parent clone", task{Call: &call{Number: unix.SYS_CLONE}}, true},
		{"parent clone3", task{Call: &call{Number: unix.SYS_CLONE3}}, true},
		{"preexec setsid", task{PreExec: true, Call: &call{Number: unix.SYS_SETSID}}, true},
		{"preexec ioctl", task{PreExec: true, Call: &call{Number: unix.SYS_IOCTL}}, true},
		{"preexec write", task{PreExec: true, Call: &call{Number: unix.SYS_WRITE}}, true},
		{"preexec exit can be refused", task{PreExec: true, Call: &call{Number: unix.SYS_EXIT}}, true},
		{"worker epoll", task{Call: &call{Number: unix.SYS_EPOLL_CTL}}, false},
	}
	for _, test := range tests {
		if got := criticalReturn(&test.task); got != test.want {
			t.Errorf("%s: criticalReturn = %v, want %v", test.name, got, test.want)
		}
	}
}

func TestESRCHNeedsTerminalWaitAndKeepsRelevantGapFatal(t *testing.T) {
	o, buffer := testObserver()
	o.tasks[123].Call = &call{Number: unix.SYS_CLONE}
	o.lostStop(123, "syscall-info", true)
	if o.tasks[123].Lost == nil || o.err != nil {
		t.Fatal("ESRCH was dropped or classified without a terminal proof")
	}
	o.finished(123, syscall.WaitStatus(0))
	if o.err == nil || !strings.Contains(o.err.Error(), "unobserved relevant") || len(o.tasks) != 0 {
		t.Fatal("terminal exit hid an unobserved relevant return")
	}
	if !strings.Contains(buffer.String(), "terminal-race-resolved") {
		t.Fatal("terminal resolution was not retained")
	}
}

func TestResumeESRCHCanResolveOnlyAfterTerminalWait(t *testing.T) {
	o, _ := testObserver()
	o.lostStop(123, "resume", false)
	o.finished(123, syscall.WaitStatus(unix.SIGKILL))
	if o.err != nil || len(o.tasks) != 0 {
		t.Fatalf("confirmed terminal race: %v", o.err)
	}
}

func TestLiveStopAfterESRCHIsNotAnExitRace(t *testing.T) {
	o, _ := testObserver()
	o.lostStop(123, "syscall-info", false)
	status := syscall.WaitStatus((int(unix.SIGTRAP|0x80) << 8) | 0x7f)
	o.stopped(123, o.tasks[123], status)
	if o.err == nil || !strings.Contains(o.err.Error(), "stopped again") {
		t.Fatal("a live task was allowed to continue after lost coverage")
	}
}

func TestUnresolvedESRCHFailsEvenWhenCleanupFindsNoChildren(t *testing.T) {
	o, buffer := testObserver()
	o.lostStop(123, "syscall-info", false)
	o.checkUnresolved()
	// No real task is signalled. The terminal proof is deliberately absent.
	delete(o.tasks, 123)
	o.teardown()
	if o.err == nil || !strings.Contains(buffer.String(), "no terminal proof") {
		t.Fatal("cleanup erased a missing terminal proof")
	}
}
