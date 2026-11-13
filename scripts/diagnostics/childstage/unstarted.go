//go:build linux && amd64

package main

import (
	"fmt"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

func (o *observer) parkUnidentified(pid int, t *task, stop syscall.WaitStatus) {
	if t.ExitOnly && stop.StopSignal() == unix.SIGTRAP && stop.TrapCause() == unix.PTRACE_EVENT_EXIT {
		o.event(pid, t, unix.PTRACE_EVENT_EXIT)
		o.resumeExiting(pid)
		return
	}
	if t.ExitOnly || stop.StopSignal() != unix.SIGSTOP && stop.TrapCause() != unix.PTRACE_EVENT_EXIT {
		o.err = fmt.Errorf("unidentified task %d reached a live stop %d", pid, int(stop))
		return
	}
	t.Parked, t.PendingStop = true, stop
	o.emit(map[string]any{eventKey: "parked-stop", tidKey: pid, statusKey: int(stop)})
}

// An unresumed automatically traced thread can outlive its creator's clone
// notification during group exit. Require the leader's exit_group or SIGKILL
// and wait until every identified thread is exiting: no creator can still
// deliver that notification then. This path
// never resumes the unidentified thread into user code or another syscall.
func (o *observer) releaseUnstartedThreads() {
	if o.err != nil {
		return
	}
	for pid, t := range o.tasks {
		if !t.Parked || t.Known || t.ExitOnly {
			continue
		}
		o.releaseUnstarted(pid, t)
	}
}

func (o *observer) releaseUnstarted(pid int, t *task) {
	facts := status(pid)
	tgid, _ := strconv.Atoi(facts["Tgid"])
	group := o.tasks[tgid]
	if tgid == pid || group == nil || !group.Known || !group.Exiting || !group.GroupExit || !o.groupExiting(tgid) {
		return
	}
	tracer, _ := strconv.Atoi(facts["TracerPid"])
	if tracer != unix.Gettid() {
		o.err = fmt.Errorf("unstarted thread %d lost its tracer identity", pid)
		return
	}
	t.TGID, t.Owner, t.Image = tgid, group.Owner, group.Image
	t.ExitOnly, t.Parked = true, false
	o.emit(map[string]any{eventKey: "unstarted-thread-exit", tidKey: pid, tgidKey: tgid, ownerKey: t.Owner,
		"neverResumed": true, "creatorNotification": unavailable, "tracerTid": tracer, statusKey: int(t.PendingStop)})
	if t.PendingStop.TrapCause() == unix.PTRACE_EVENT_EXIT {
		o.event(pid, t, unix.PTRACE_EVENT_EXIT)
		o.resumeExiting(pid)
		return
	}
	// The consumed initial stop retains this traced task's numeric identity
	// until its terminal wait. SIGKILL prevents any uncovered child return.
	if err := unix.Kill(pid, unix.SIGKILL); err != nil && err != unix.ESRCH {
		o.err = fmt.Errorf("terminate unstarted owned thread %d: %w", pid, err)
		return
	}
	o.resumeExiting(pid)
}

func endingGroup(pid int, t *task, result uint) bool {
	if pid != t.TGID {
		return false
	}
	return result == uint(unix.SIGKILL) || t.Call != nil && t.Call.Number == unix.SYS_EXIT_GROUP
}

func (o *observer) groupExiting(tgid int) bool {
	for _, t := range o.tasks {
		if t.Known && t.TGID == tgid && !t.Exiting {
			return false
		}
	}
	return true
}
