//go:build linux && amd64

// childstage is a bounded diagnostic observer. Its results never accept a release.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const traceOptions = unix.PTRACE_O_EXITKILL | unix.PTRACE_O_TRACESYSGOOD |
	unix.PTRACE_O_TRACEFORK | unix.PTRACE_O_TRACEVFORK | unix.PTRACE_O_TRACECLONE | unix.PTRACE_O_TRACEEXEC | unix.PTRACE_O_TRACEEXIT

const (
	eventKey    = "event"
	tidKey      = "tid"
	tgidKey     = "tgid"
	creatorKey  = "creatorTid"
	ownerKey    = "owner"
	imageKey    = "image"
	statusKey   = "status"
	unavailable = "unavailable"
)

type call struct {
	Number    uint64    `json:"number"`
	Args      [6]uint64 `json:"args"`
	Clone3    []uint64  `json:"clone3,omitempty"`
	EnteredNS int64     `json:"enteredNs"`
}

type task struct {
	Creator         int
	TGID            int
	Owner           int
	Known           bool
	Parked          bool
	AwaitStop       bool
	PreExec         bool
	Call            *call
	Image           string
	InheritedReturn bool
	Lost            *lostStop
}

type observer struct {
	output  *json.Encoder
	started time.Time
	tasks   map[int]*task
	root    int
	count   int
	limit   int
	code    int
	done    bool
	err     error
	images  map[string]*os.File
	digests map[string]map[string]any
}

func main() {
	trace := flag.String("output", "", "exclusive JSONL output")
	seconds := flag.Int("seconds", 600, "diagnostic wall bound, at most 600")
	limit := flag.Int("records", 16384, "maximum JSONL records, at most 16384")
	flag.Parse()
	if *trace == "" || len(flag.Args()) == 0 || *seconds < 1 || *seconds > 600 || *limit < 16 || *limit > 16384 {
		fmt.Fprintln(os.Stderr, "childstage: require -output FILE [-seconds 1..600] [-records 16..16384] -- PROGRAM ARG...")
		os.Exit(2)
	}
	code, err := run(*trace, time.Duration(*seconds)*time.Second, *limit, flag.Args())
	if err != nil {
		fmt.Fprintln(os.Stderr, "childstage:", err)
		os.Exit(125)
	}
	os.Exit(code)
}

func run(path string, bound time.Duration, limit int, argv []string) (code int, resultErr error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- Owner-selected exclusive diagnostic output; existing files cannot be overwritten.
	if err != nil {
		return 0, fmt.Errorf("create trace: %w", err)
	}
	defer func() { _ = file.Close() }()
	o := &observer{output: json.NewEncoder(file), started: time.Now(), tasks: make(map[int]*task), limit: limit,
		images: make(map[string]*os.File), digests: make(map[string]map[string]any)}
	defer func() {
		o.teardown()
		if resultErr == nil {
			resultErr = o.err
		}
	}()
	pid, err := syscall.ForkExec(argv[0], argv, &syscall.ProcAttr{Env: os.Environ(), // #nosec G204 -- Explicit debugger CLI target, never a remote request or shell interpolation.
		Files: []uintptr{0, 1, 2}, Sys: &syscall.SysProcAttr{Ptrace: true}})
	if err != nil {
		return 0, fmt.Errorf("start diagnostic root: %w", err)
	}
	o.root = pid
	o.tasks[pid] = &task{Known: true, TGID: pid, Owner: pid, Image: imageIdentity(pid), InheritedReturn: true}
	if err := o.arm(pid); err != nil {
		return 0, err
	}
	o.tasks[pid].Image = o.bindImage(pid)
	o.emit(map[string]any{eventKey: "armed", "root": pid, "backend": "ptrace", "context": taskContext(pid),
		"diagnosticOnly": true, "allDescendantThreads": true, "maxTasks": 8192, "maxRecords": limit})
	o.resume(pid, 0)
	o.loop(bound)
	return o.code, o.err
}

func (o *observer) arm(pid int) error {
	var status syscall.WaitStatus
	_, err := syscall.Wait4(pid, &status, 0, nil)
	if err != nil {
		return fmt.Errorf("wait for diagnostic root: %w", err)
	}
	if !status.Stopped() {
		return errors.New("diagnostic root did not stop")
	}
	if err := unix.PtraceSetOptions(pid, traceOptions); err != nil {
		return fmt.Errorf("arm descendant tracking: %w", err)
	}
	return nil
}

func (o *observer) loop(bound time.Duration) {
	for !o.done && o.err == nil && len(o.tasks) > 0 {
		if time.Since(o.started) > bound {
			o.err = errors.New("diagnostic wall bound reached")
			return
		}
		o.wait()
	}
}

func (o *observer) wait() {
	var status syscall.WaitStatus
	pid, err := syscall.Wait4(-1, &status, syscall.WALL|syscall.WNOHANG, nil)
	if errors.Is(err, syscall.EINTR) {
		return
	}
	if err != nil {
		o.err = fmt.Errorf("wait for owned task: %w", err)
		return
	}
	if pid == 0 {
		time.Sleep(50 * time.Microsecond)
		return
	}
	o.handle(pid, status)
}

func (o *observer) handle(pid int, status syscall.WaitStatus) {
	if status.Exited() || status.Signaled() {
		o.finished(pid, status)
		return
	}
	if !status.Stopped() {
		return
	}
	t := o.tasks[pid]
	// A nonleader exec changes its TID to the former leader's PID. That PID
	// may already have had its final wait, so resolve exec before parking it.
	if status.StopSignal() == syscall.SIGTRAP && status.TrapCause() == unix.PTRACE_EVENT_EXEC {
		o.event(pid, t, unix.PTRACE_EVENT_EXEC)
		o.resume(pid, 0)
		return
	}
	if t == nil {
		o.tasks[pid] = &task{Parked: true}
		return
	}
	if !t.Known {
		t.Parked = true
		return
	}
	o.stopped(pid, t, status)
}

func (o *observer) stopped(pid int, t *task, status syscall.WaitStatus) {
	sig := status.StopSignal()
	if t.Lost != nil && !(sig == syscall.SIGTRAP && status.TrapCause() == unix.PTRACE_EVENT_EXIT) {
		o.err = fmt.Errorf("task %d stopped again after unresolved %s ESRCH", pid, t.Lost.Operation)
		return
	}
	if sig == syscall.SIGTRAP|0x80 {
		o.syscallStop(pid, t)
		o.resume(pid, 0)
		return
	}
	if sig == syscall.SIGTRAP && status.TrapCause() > 0 {
		o.event(pid, t, status.TrapCause())
		if status.TrapCause() == unix.PTRACE_EVENT_EXIT {
			o.resumeExiting(pid)
			return
		}
		o.resume(pid, 0)
		return
	}
	if sig == syscall.SIGSTOP && t.AwaitStop {
		t.AwaitStop = false
		o.resume(pid, 0)
		return
	}
	o.resume(pid, int(sig))
}

func (o *observer) event(pid int, t *task, cause int) {
	message, err := unix.PtraceGetEventMsg(pid)
	if err != nil {
		if cause == unix.PTRACE_EVENT_EXIT && errors.Is(err, unix.ESRCH) {
			o.lostStop(pid, "exit-event", false)
			return
		}
		o.err = fmt.Errorf("read owned ptrace event %d for task %d: %w", cause, pid, err)
		return
	}
	if cause == unix.PTRACE_EVENT_EXIT {
		o.emit(map[string]any{eventKey: "exiting", tidKey: pid, tgidKey: t.TGID, statusKey: message, "pendingCall": t.Call})
		return
	}
	if cause == unix.PTRACE_EVENT_EXEC {
		o.execed(pid, int(message), t)
		return
	}
	if cause < unix.PTRACE_EVENT_FORK || cause > unix.PTRACE_EVENT_CLONE {
		o.err = fmt.Errorf("unexpected ptrace event %d", cause)
		return
	}
	o.created(pid, int(message), t)
}

func (o *observer) created(parent, child int, p *task) {
	if len(o.tasks) >= 8192 {
		o.err = errors.New("owned task bound reached")
		return
	}
	t := o.tasks[child]
	if t == nil {
		t = &task{AwaitStop: true}
		o.tasks[child] = t
	}
	t.Known, t.Creator, t.Owner, t.Image = true, parent, p.Owner, p.Image
	t.InheritedReturn = true
	t.TGID = taskTGID(child)
	if t.TGID == 0 {
		o.err = errors.New("owned task has no TGID identity")
		return
	}
	t.PreExec = t.TGID != p.TGID
	o.emit(map[string]any{eventKey: "created", tidKey: child, tgidKey: t.TGID, creatorKey: parent,
		"creatorTgid": p.TGID, ownerKey: t.Owner, "preExec": t.PreExec})
	if t.Parked {
		t.Parked = false
		t.AwaitStop = false
		o.resume(child, 0)
	}
}

func (o *observer) execed(pid, former int, t *task) {
	if t == nil {
		t = &task{}
		o.tasks[pid] = t
	}
	if former != pid {
		o.resolveLost(pid, t, "exec-leader-replacement")
		old := o.tasks[former]
		if old == nil || !old.Known {
			o.err = errors.New("nonleader exec has no owned former TID")
			return
		}
		*t = *old
		o.resolveLost(former, t, "exec-tid-replacement")
		delete(o.tasks, former)
	}
	t.Call, t.PreExec, t.TGID = nil, false, pid
	t.InheritedReturn = true
	// File metadata ties this event to the independently held mapped-image digest.
	t.Image = o.bindImage(pid)
	t.Owner = pid
	o.emit(map[string]any{eventKey: "exec", tidKey: pid, "formerTid": former, tgidKey: pid,
		creatorKey: t.Creator, ownerKey: t.Owner, imageKey: t.Image, "context": taskContext(pid)})
}

func (o *observer) syscallStop(pid int, t *task) {
	info, err := syscallInfo(pid)
	if err != nil {
		if errors.Is(err, unix.ESRCH) {
			o.lostStop(pid, "syscall-info", criticalReturn(t))
			return
		}
		o.err = fmt.Errorf("read exact syscall stop for task %d: %w", pid, err)
		return
	}
	if info.Op == unix.PTRACE_SYSCALL_INFO_NONE {
		o.lostStop(pid, "syscall-info-none", criticalReturn(t))
		return
	}
	if info.Arch != unix.AUDIT_ARCH_X86_64 {
		o.err = errors.New("owned task changed syscall architecture")
		return
	}
	if info.Op == unix.PTRACE_SYSCALL_INFO_ENTRY {
		t.InheritedReturn = false
		var args [6]uint64
		copy(args[:], info.Data[1:7])
		t.Call = &call{Number: info.Data[0], Args: args, EnteredNS: time.Since(o.started).Nanoseconds()}
		o.cloneArgs(pid, t.Call)
		return
	}
	if info.Op != unix.PTRACE_SYSCALL_INFO_EXIT {
		o.err = errors.New("syscall stop has no entry or exit identity")
		return
	}
	o.returned(pid, t, int64(info.Data[0])) // #nosec G115 -- Kernel ABI encodes signed syscall returns in this 64-bit union word.
}

func (o *observer) cloneArgs(pid int, c *call) {
	if c.Number != unix.SYS_CLONE3 {
		return
	}
	var buffer [88]byte
	n, err := unix.PtracePeekData(pid, uintptr(c.Args[0]), buffer[:min(c.Args[1], 88)])
	if err != nil {
		o.err = fmt.Errorf("read owned clone3 arguments: %w", err)
		return
	}
	c.Clone3 = decodeWords(buffer[:n])
}

func (o *observer) returned(pid int, t *task, result int64) {
	c := t.Call
	t.Call = nil
	if c == nil {
		if !t.InheritedReturn || result < 0 {
			o.err = errors.New("unpaired syscall return")
		}
		t.InheritedReturn = false
		return
	}
	if result != -int64(unix.EPERM) && c.Number != unix.SYS_CLONE && c.Number != unix.SYS_CLONE3 {
		return
	}
	record := map[string]any{eventKey: "return", tidKey: pid, tgidKey: t.TGID, creatorKey: t.Creator,
		ownerKey: t.Owner, imageKey: t.Image, "preExec": t.PreExec, "call": c, "result": result}
	if result == -int64(unix.EPERM) {
		record["context"] = taskContext(pid)
		record["errno"] = int(unix.EPERM)
		record["mappedImage"] = o.mappedImage(t.Image)
		if c.Number == unix.SYS_IOCTL && c.Args[0] < 1<<20 {
			record["ioctlTerminal"] = terminalFact(fmt.Sprintf("/proc/%d/", pid), int(c.Args[0])) // #nosec G115 -- Descriptor is bounded below 2^20 by the preceding condition.
		}
	}
	o.emit(record)
}

func (o *observer) resume(pid, signal int) {
	if o.err != nil || o.tasks[pid].Lost != nil {
		return
	}
	if err := unix.PtraceSyscall(pid, signal); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			o.lostStop(pid, "resume", false)
			return
		}
		o.err = fmt.Errorf("resume owned task %d: %w", pid, err)
	}
}

func (o *observer) finished(pid int, status syscall.WaitStatus) {
	t := o.tasks[pid]
	if t == nil || !t.Known {
		o.err = fmt.Errorf("terminal wait for task %d without owned creation identity", pid)
		return
	}
	o.resolveLost(pid, t, "terminal-wait")
	delete(o.tasks, pid)
	o.emit(map[string]any{eventKey: "exit", tidKey: pid, statusKey: int(status)})
	if pid != o.root {
		return
	}
	o.code = status.ExitStatus()
	if status.Signaled() {
		o.code = 128 + int(status.Signal())
	}
	o.done = true
}

func (o *observer) emit(record map[string]any) {
	if o.count >= o.limit {
		o.err = errors.New("diagnostic record bound reached")
		return
	}
	o.count++
	record["sequence"], record["monotonicNs"] = o.count, time.Since(o.started).Nanoseconds()
	if err := o.output.Encode(record); err != nil {
		o.err = fmt.Errorf("write trace: %w", err)
	}
}

func (o *observer) teardown() {
	defer o.closeImages()
	o.checkUnresolved()
	for pid := range o.tasks {
		_ = unix.Kill(pid, unix.SIGKILL)
		_ = unix.PtraceCont(pid, int(unix.SIGKILL))
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(o.tasks) > 0 && time.Now().Before(deadline) {
		o.reap()
	}
	if len(o.tasks) > 0 {
		o.err = errors.Join(o.err, errors.New("owned task teardown incomplete"))
	}
	// This final bounded record is reserved even when the normal record limit hit.
	err := o.output.Encode(map[string]any{eventKey: "teardown", "remainingTasks": len(o.tasks), "observerError": errorText(o.err)})
	if err != nil {
		o.err = errors.Join(o.err, fmt.Errorf("write teardown: %w", err))
	}
}

func (o *observer) reap() {
	var status syscall.WaitStatus
	pid, err := syscall.Wait4(-1, &status, syscall.WALL|syscall.WNOHANG, nil)
	if errors.Is(err, syscall.ECHILD) {
		clear(o.tasks)
		return
	}
	if pid > 0 && (status.Exited() || status.Signaled()) {
		delete(o.tasks, pid)
		return
	}
	if pid > 0 {
		_ = unix.PtraceCont(pid, int(unix.SIGKILL))
	}
	time.Sleep(time.Millisecond)
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
