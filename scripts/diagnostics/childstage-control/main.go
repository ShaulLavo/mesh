//go:build linux && amd64

// Only this synthetic process installs a syscall refusal policy.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"syscall"
	"unsafe"

	"github.com/charmbracelet/x/xpty"
	"golang.org/x/sys/unix"
)

const (
	parentMode = "parent"
	clone3Mode = "clone3"
)

func main() {
	if len(os.Args) != 2 {
		os.Exit(2)
	}
	if err := control(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func control(mode string) error {
	runtime.GOMAXPROCS(4)
	// Exercise Go runtime thread following before the launch is refused.
	done := make(chan bool)
	for range 4 {
		go func() { runtime.LockOSThread(); done <- true }()
	}
	for range 4 {
		<-done
	}
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
	err := pty.Start(cmd)
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
		"setsid": true, "setctty": true, "ctty": 0, "goPtyStart": mode != clone3Mode}
	if err := json.NewEncoder(os.Stdout).Encode(value); err != nil {
		return fmt.Errorf("emit synthetic control identity: %w", err)
	}
	return nil
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
