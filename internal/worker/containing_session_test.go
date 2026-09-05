package worker

import (
	"path/filepath"
	"testing"
)

func TestContainingSessionIDWalksToTheOwningWorker(t *testing.T) {
	processes := map[int]ancestorProcess{
		40: {parentID: 30, args: []string{"mesh"}},
		30: {parentID: 20, args: []string{"claude", "tool"}},
		20: {parentID: 1, args: []string{"/opt/mesh", "session-worker", "--id", "7k3d", "--", "bash"}},
	}
	read := func(pid int) (ancestorProcess, bool) {
		process, ok := processes[pid]
		return process, ok
	}
	if got := containingSessionIDFromAncestors(40, read); got != "7K3D" {
		t.Fatalf("containing session = %q, want 7K3D", got)
	}
}

func TestContainingSessionWorkerReturnsValidatedIDAndDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "7K3D")
	processes := map[int]ancestorProcess{
		40: {parentID: 30, args: []string{"mesh"}},
		30: {parentID: 20, args: []string{"claude", "tool"}},
		20: {parentID: 1, args: []string{"/opt/mesh", "session-worker", "--dir", dir, "--id", "7k3d", "--", "bash"}},
	}
	read := func(pid int) (ancestorProcess, bool) {
		process, ok := processes[pid]
		return process, ok
	}

	got, ok := containingSessionWorkerFromAncestors(40, read)
	if !ok || got.SessionID != "7K3D" || got.Dir != dir {
		t.Fatalf("containing worker = %#v, %t; want 7K3D at %s", got, ok, dir)
	}
}

func TestSessionWorkerLocationRejectsAmbiguousOrUnsafeDirectory(t *testing.T) {
	root := t.TempDir()
	validDir := filepath.Join(root, "7K3D")
	tests := [][]string{
		{"mesh", "session-worker", "--id", "7K3D", "--dir", "relative/7K3D", "--", "bash"},
		{"mesh", "session-worker", "--id", "7K3D", "--dir", filepath.Join(root, "OTHER"), "--", "bash"},
		{"mesh", "session-worker", "--id", "7K3D", "--dir", validDir, "--dir", validDir, "--", "bash"},
		{"mesh", "session-worker", "--id", "7K3D", "--", "bash", "--dir", validDir},
	}
	for _, args := range tests {
		if got, ok := sessionWorkerLocationFromArgs(args); ok {
			t.Errorf("worker args %#v produced location %#v", args, got)
		}
	}
}

func TestContainingSessionIDRejectsLookalikesAndParentLoops(t *testing.T) {
	for _, args := range [][]string{
		{"mesh", "logs", "session-worker", "--id", "7K3D"},
		{"mesh", "session-worker", "--", "echo", "--id", "7K3D"},
		{"mesh", "session-worker", "--id", "NOPE"},
	} {
		if got := sessionIDFromWorkerArgs(args); got != "" {
			t.Errorf("worker args %#v produced session %q", args, got)
		}
	}
	loop := func(pid int) (ancestorProcess, bool) {
		return ancestorProcess{parentID: pid, args: []string{"mesh"}}, true
	}
	if got := containingSessionIDFromAncestors(40, loop); got != "" {
		t.Fatalf("parent loop produced session %q", got)
	}
}
