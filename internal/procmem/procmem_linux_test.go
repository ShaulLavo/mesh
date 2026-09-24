package procmem

import (
	"os"
	"os/exec"
	"testing"
)

func TestTreeIncludesChildrenOfTheRoot(t *testing.T) {
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })

	table := Snapshot()
	self, ok := linuxProportional(os.Getpid())
	if !ok || self == 0 {
		t.Fatalf("own proportional size = %d, %v", self, ok)
	}
	sleeping, _ := linuxProportional(child.Process.Pid)
	if got := table.Tree(os.Getpid()); got < self+sleeping || sleeping == 0 {
		t.Fatalf("tree = %d, want at least self %d + child %d", got, self, sleeping)
	}
	if parent := table.Parent(child.Process.Pid); parent != os.Getpid() {
		t.Fatalf("parent = %d, want %d", parent, os.Getpid())
	}
	if table.Tree(0) != 0 {
		t.Fatal("pid 0 should measure nothing")
	}
}

func TestSessionRootOnlyClimbsToTheNamedWorker(t *testing.T) {
	table := Snapshot()
	self := os.Getpid()
	if root := table.SessionRoot(self, "7K3D"); root != self {
		t.Fatalf("root = %d, want the child itself %d when the parent is not a session worker", root, self)
	}
	if root := table.SessionRoot(0, "7K3D"); root != 0 {
		t.Fatalf("root of pid 0 = %d", root)
	}
}
