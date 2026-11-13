//go:build linux && amd64

package main

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func (o *observer) checkUnresolved() {
	for pid, t := range o.tasks {
		if t.Lost == nil {
			continue
		}
		o.err = errors.Join(o.err, fmt.Errorf("task %d has no terminal proof for %s", pid, t.Lost.Operation))
	}
}

type lostStop struct {
	Operation string
	Critical  bool
}

// At an unread entry stop the task has not been resumed into that syscall.
// At an unread return stop, every pre-exec call and parent clone must remain
// unconfirmed even if a later terminal wait proves why ptrace returned ESRCH.
func criticalReturn(t *task) bool {
	if t.Call == nil {
		return false
	}
	return t.PreExec || t.Call.Number == unix.SYS_CLONE || t.Call.Number == unix.SYS_CLONE3
}

func (o *observer) lostStop(pid int, operation string, critical bool) {
	t := o.tasks[pid]
	if t == nil || t.Lost != nil {
		o.err = fmt.Errorf("unidentified or repeated lost stop for task %d during %s", pid, operation)
		return
	}
	t.Lost = &lostStop{Operation: operation, Critical: critical}
	o.emit(map[string]any{eventKey: "terminal-race", tidKey: pid, tgidKey: t.TGID,
		"operation": operation, "criticalReturnUnconfirmed": critical, "pendingCall": t.Call})
}

func (o *observer) resolveLost(pid int, t *task, proof string) {
	if t.Lost == nil {
		return
	}
	lost := t.Lost
	t.Lost = nil
	o.emit(map[string]any{eventKey: "terminal-race-resolved", tidKey: pid,
		"operation": lost.Operation, "proof": proof, "criticalReturnUnconfirmed": lost.Critical})
	if lost.Critical {
		o.err = fmt.Errorf("task %d exited with an unobserved relevant syscall return after %s", pid, lost.Operation)
	}
}

func (o *observer) resumeExiting(pid int) {
	if o.err != nil {
		return
	}
	err := unix.PtraceCont(pid, 0)
	if err == nil {
		return
	}
	if err != unix.ESRCH {
		o.err = fmt.Errorf("resume exiting task %d: %w", pid, err)
		return
	}
	if o.tasks[pid].Lost == nil {
		o.lostStop(pid, "exit-resume", false)
	}
}
