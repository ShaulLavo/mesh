//go:build linux && amd64

// Only this synthetic process installs a syscall refusal policy.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/charmbracelet/x/xpty"
	"golang.org/x/sys/unix"
)

const (
	parentMode = "parent"
	clone3Mode = "clone3"
)

var controlContext = map[string]any{"gcPercent": 25, "scope": map[string]any{}, "thp": []map[string]any{}}

func main() {
	if len(os.Args) != 2 {
		os.Exit(2)
	}
	if err := runControl(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runControl(mode string) error {
	if strings.HasPrefix(mode, "lifecycle-") {
		return lifecycleControl(mode)
	}
	if mode != "parent-thread" {
		return control(mode)
	}
	// Keep the leader occupied so this refused clone must occur in a followed
	// Go thread rather than accidentally repeating the leader-only control.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	result := make(chan error, 1)
	go func() { result <- control(parentMode) }()
	return <-result
}

func lifecycleControl(mode string) error {
	runtime.GOMAXPROCS(2)
	runtime.LockOSThread()
	ready := make(chan struct{}, 8)
	for range 8 {
		go churnThread(ready, mode == "lifecycle-thread-exit")
	}
	for range 8 {
		<-ready
	}
	switch mode {
	case "lifecycle-thread-exit", "lifecycle-exit-group":
		return nil
	case "lifecycle-kill":
		if err := unix.Kill(os.Getpid(), unix.SIGKILL); err != nil {
			return fmt.Errorf("kill synthetic thread group: %w", err)
		}
		return nil
	case "lifecycle-exec":
		go func() {
			runtime.LockOSThread()
			err := syscall.Exec("/bin/true", []string{"true"}, os.Environ()) // #nosec G204 -- Fixed synthetic image and arguments, using only this owned control's inherited environment.
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}()
		select {}
	}
	return errors.New("unknown lifecycle control")
}

func churnThread(ready chan<- struct{}, exit bool) {
	runtime.LockOSThread()
	ready <- struct{}{}
	if exit {
		return
	}
	for {
		_, _, _ = unix.RawSyscall(unix.SYS_GETPID, 0, 0, 0)
		runtime.Gosched()
	}
}

func control(mode string) error {
	setHugePages(false)
	debug.SetGCPercent(25)
	runtime.GOMAXPROCS(min(2, runtime.GOMAXPROCS(0)))
	controlContext["gomaxprocs"] = runtime.GOMAXPROCS(0)
	isolateControl()
	// Warm Go's pidfd detection before injecting a refusal, so the real PTY
	// clone retains the compiler/runtime's normal flags on this kernel.
	if err := exec.Command("/bin/true").Run(); err != nil {
		return fmt.Errorf("warm synthetic Go pidfd detection: %w", err)
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	pty, err := xpty.NewPty(80, 24)
	if err != nil {
		return fmt.Errorf("control PTY: %w", err)
	}
	defer func() { _ = pty.Close() }()
	denied := []uint32{unix.SYS_SETSID}
	if mode == "terminal" {
		denied = []uint32{unix.SYS_IOCTL}
	}
	if mode == parentMode || mode == clone3Mode {
		denied = []uint32{unix.SYS_CLONE, unix.SYS_CLONE3}
	}
	if mode != parentMode && mode != "child" && mode != "terminal" && mode != clone3Mode {
		return errors.New("unknown synthetic control")
	}
	if err := refuse(denied); err != nil {
		return err
	}
	if mode == clone3Mode {
		return clone3Control()
	}
	return ptyControl(mode, pty)
}

func ptyControl(mode string, pty xpty.Pty) error {
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	err := startControlSession(pty, cmd)
	if err == nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return errors.New("synthetic Go PTY start unexpectedly succeeded")
	}
	if !errors.Is(err, syscall.EPERM) {
		return fmt.Errorf("control requires refused Go PTY start: %w", err)
	}
	if cmd.Process != nil {
		return errors.New("refused control unexpectedly published a child")
	}
	return emitControl(mode)
}

func refuse(numbers []uint32) error {
	filter := []unix.SockFilter{{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0}}
	for _, number := range numbers {
		filter = append(filter, unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: number, Jf: 1},
			unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)})
	}
	filter = append(filter, unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW})
	if len(filter) > 65535 {
		return errors.New("synthetic filter length exceeds kernel ABI")
	}
	program := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]} // #nosec G115 -- The 16-bit length bound is checked immediately above.
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("synthetic no-new-privileges: %w", err)
	}
	if err := unix.Prctl(unix.PR_SET_SECCOMP, unix.SECCOMP_MODE_FILTER, uintptr(unsafe.Pointer(&program)), 0, 0); err != nil { // #nosec G103 -- Fixed local sock_fprog and its filter remain live through the syscall; synthetic thread only.
		return fmt.Errorf("synthetic thread-only filter: %w", err)
	}
	runtime.KeepAlive(filter)
	return nil
}

func clone3Control() error {
	var args [11]uint64
	_, _, err := unix.RawSyscall(unix.SYS_CLONE3, uintptr(unsafe.Pointer(&args)), unsafe.Sizeof(args), 0) // #nosec G103 -- Local zeroed clone_args buffer; the synthetic thread's filter refuses the call before any child exists.
	if err != unix.EPERM {
		return fmt.Errorf("synthetic clone3 refusal: %w", err)
	}
	return emitControl(clone3Mode)
}

func emitControl(mode string) error {
	value := map[string]any{"mode": mode, "errno": 1, "worker": os.Getpid(), "tid": unix.Gettid(),
		parentMode: os.Getppid(), "go": runtime.Version(), "build": buildFacts(),
		"setsid": true, "setctty": true, "ctty": 0, "goPtyStart": mode != clone3Mode, "runtimeContext": controlContext}
	if err := json.NewEncoder(os.Stdout).Encode(value); err != nil {
		return fmt.Errorf("emit synthetic control identity: %w", err)
	}
	return nil
}

func setHugePages(enabled bool) {
	disable := uintptr(1)
	if enabled {
		disable = 0
	}
	err := unix.Prctl(unix.PR_SET_THP_DISABLE, disable, 0, 0, 0)
	recordTHP(map[string]any{"operation": "set", "disabled": disable, "error": errorText(err)})
}

func hugePagesDisabled() bool {
	disabled, err := unix.PrctlRetInt(unix.PR_GET_THP_DISABLE, 0, 0, 0, 0)
	recordTHP(map[string]any{"operation": "query", "disabled": disabled, "error": errorText(err)})
	return err == nil && disabled != 0
}

func recordTHP(row map[string]any) {
	controlContext["thp"] = append(controlContext["thp"].([]map[string]any), row)
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func startControlSession(pty xpty.Pty, cmd *exec.Cmd) error {
	if !hugePagesDisabled() {
		return startError(pty.Start(cmd))
	}
	setHugePages(true)
	err := pty.Start(cmd)
	setHugePages(false)
	debug.FreeOSMemory()
	return startError(err)
}

func startError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("synthetic child start: %w", err)
}

func isolateControl() {
	current, err := os.ReadFile("/proc/self/cgroup")
	result := map[string]any{"before": string(current), "readError": errorText(err), "callTimeoutSeconds": 2, "settleSeconds": 1}
	controlContext["scope"] = result
	defer func() {
		after, readErr := os.ReadFile("/proc/self/cgroup")
		result["after"], result["afterReadError"] = string(after), errorText(readErr)
	}()
	path := unifiedCgroupPath(string(current))
	if err != nil || path == "" {
		return
	}
	id := "childstage-control-" + strconv.Itoa(os.Getpid())
	unit := "mesh-session-" + id + ".scope"
	result["unit"] = unit
	if strings.HasSuffix(path, "/"+unit) {
		result["alreadyJoined"] = true
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "busctl", startScopeArgs(id, os.Getpid(), parentSlice(path))...).CombinedOutput() //nolint:gosec // Fixed busctl method; the unit uses this control's numeric PID and the slice comes from its kernel cgroup.
	result["callError"], result["output"] = errorText(err), string(output[max(0, len(output)-1024):])
	if err != nil {
		return
	}
	result["joined"] = waitScope(unit)
}

func waitScope(unit string) bool {
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		current, err := os.ReadFile("/proc/self/cgroup")
		if err == nil && strings.HasSuffix(unifiedCgroupPath(string(current)), "/"+unit) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func startScopeArgs(id string, pid int, slice string) []string {
	properties := []string{"PIDs", "au", "1", strconv.Itoa(pid), "Description", "s", "Mesh session " + id,
		"CollectMode", "s", "inactive-or-failed"}
	count := 3
	if slice != "" {
		properties = append(properties, "Slice", "s", slice)
		count++
	}
	args := []string{"--user", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1",
		"org.freedesktop.systemd1.Manager", "StartTransientUnit", "ssa(sv)a(sa(sv))", "mesh-session-" + id + ".scope", "fail", strconv.Itoa(count)}
	args = append(args, properties...)
	return append(args, "0")
}

func unifiedCgroupPath(contents string) string {
	for line := range strings.SplitSeq(contents, "\n") {
		if path, ok := strings.CutPrefix(line, "0::"); ok {
			return path
		}
	}
	return ""
}

func parentSlice(path string) string {
	segments := strings.Split(strings.Trim(path, "/"), "/")
	if len(segments) < 2 {
		return ""
	}
	parent := segments[len(segments)-2]
	if !strings.HasSuffix(parent, ".slice") {
		return ""
	}
	return parent
}

func buildFacts() map[string]string {
	result := make(map[string]string)
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return result
	}
	for _, setting := range info.Settings {
		switch setting.Key {
		case "CGO_ENABLED", "GOOS", "GOARCH", "GOAMD64", "-compiler":
			result[setting.Key] = setting.Value
		}
	}
	for _, dep := range info.Deps {
		if dep.Path == "github.com/charmbracelet/x/xpty" || dep.Path == "github.com/creack/pty" {
			result[dep.Path] = dep.Version
		}
	}
	return result
}
