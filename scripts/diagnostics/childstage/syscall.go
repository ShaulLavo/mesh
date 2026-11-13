//go:build linux && amd64

package main

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Layout from linux/ptrace.h. The tagged union avoids entry/exit toggle drift
// after exec, fork and signal-delivery stops.
type stopInfo struct {
	Op      uint8
	Padding [3]byte
	Arch    uint32
	IP      uint64
	SP      uint64
	Data    [8]uint64
}

func syscallInfo(pid int) (stopInfo, error) {
	var info stopInfo
	n, _, errno := unix.Syscall6(unix.SYS_PTRACE, unix.PTRACE_GET_SYSCALL_INFO, uintptr(pid),
		unsafe.Sizeof(info), uintptr(unsafe.Pointer(&info)), 0, 0) // #nosec G103 -- Kernel writes a fixed-size local ptrace_syscall_info; pointer stays live for the syscall.
	if errno != 0 {
		return info, fmt.Errorf("PTRACE_GET_SYSCALL_INFO: %w", errno)
	}
	if n < 33 || info.Op == unix.PTRACE_SYSCALL_INFO_ENTRY && n < 80 {
		return info, fmt.Errorf("short syscall info: %d bytes", n)
	}
	return info, nil
}
