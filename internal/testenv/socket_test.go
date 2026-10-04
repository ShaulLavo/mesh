package testenv

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSocketScratchCanonicalizesCallerRoot(t *testing.T) {
	root := SocketTempDir(t)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	got, err := socketTempBase(alias+string(filepath.Separator), "")
	if err != nil || got != root {
		t.Fatalf("canonical scratch = %q, %v, want %q", got, err, root)
	}
}

func TestSocketScratchRejectsLongOverrideAndUsesShortRoot(t *testing.T) {
	short := SocketTempDir(t)
	long := filepath.Join(t.TempDir(), strings.Repeat("l", 110))
	if err := os.Mkdir(long, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := socketTempBase(short, long); err == nil || !strings.Contains(err.Error(), "MESH_SHORT_TMP") {
		t.Fatalf("long override = %v, want shorter scratch guidance", err)
	}
	fallback, err := socketTempBase(long, "")
	want, canonicalErr := filepath.EvalSymlinks("/tmp")
	if err != nil || canonicalErr != nil || fallback != want {
		t.Fatalf("long caller scratch fallback = %q, %v, want %q, %v", fallback, err, want, canonicalErr)
	}
	t.Setenv("TMPDIR", long+string(filepath.Separator))
	t.Setenv("MESH_SHORT_TMP", short)
	root := SocketTempDir(t)
	if filepath.Dir(root) != short {
		t.Fatalf("socket scratch %q did not use explicit short root %q", root, short)
	}
	socket := filepath.Join(root, "sessions", strings.Repeat("0", 26), "sock")
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		t.Fatal(err)
	}
	if len(socket) > 103 {
		t.Fatalf("portable worker socket requires %d bytes", len(socket))
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	_ = listener.Close()
}
