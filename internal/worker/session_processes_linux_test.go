//go:build linux

package worker

import (
	"os"
	"os/exec"
	"slices"
	"syscall"
	"testing"
	"time"
)

func TestSessionScopePathMatchesOnlyTheSessionsOwnScope(t *testing.T) {
	for _, tt := range []struct {
		cgroup string
		ok     bool
	}{
		{"0::/user.slice/user-1000.slice/user@1000.service/app.slice/mesh-session-7K3D.scope\n", true},
		{"0::/user.slice/user-1000.slice/user@1000.service/app.slice/mesh.service\n", false},
		{"0::/user.slice/user-1000.slice/user@1000.service/app.slice/mesh-session-7K3DX.scope\n", false},
		{"0::/user.slice/mesh-session-9ABC.scope\n", false},
		{"1:name=systemd:/mesh-session-7K3D.scope\n", false},
	} {
		if _, ok := sessionScopePath("7K3D", tt.cgroup); ok != tt.ok {
			t.Fatalf("scope match for %q = %t; want %t", tt.cgroup, ok, tt.ok)
		}
	}
}

func TestSessionProcessesFallsBackToKernelSessionMembers(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "sleep 30 & wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	leader := cmd.Process.Pid
	deadline := time.Now().Add(5 * time.Second)
	for {
		members, err := SessionProcesses("7K3D", leader)
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(members, os.Getpid()) {
			t.Fatalf("session members %v include the test process", members)
		}
		if len(members) >= 2 && slices.Contains(members, leader) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("session members = %v; want leader %d and its child", members, leader)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
