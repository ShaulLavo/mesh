package updatebootstrap

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/shaul/mesh/internal/updateinstall"
)

func TestDarwinMappedExecutableChild(t *testing.T) {
	socket := os.Getenv("MESH_MAPPED_IMAGE_SOCKET")
	if socket == "" {
		t.Skip("child process for native executable mapping proof")
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	conn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = io.Copy(io.Discard, conn)
}

func TestDarwinMappedExecutableSurvivesAtomicReplacement(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "mesh-map-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(root) }()
	data, err := os.ReadFile(os.Args[0]) //nolint:gosec // copy this Go test executable into an isolated native mapping fixture
	if err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(root, "mesh")
	if err = os.WriteFile(original, data, 0700); err != nil { //nolint:gosec // executable child fixture
		t.Fatal(err)
	}
	socket := filepath.Join(root, "sock")
	child := exec.Command(original, "-test.run=^TestDarwinMappedExecutableChild$") //nolint:gosec // this exact test binary runs only the Unix socket child fixture
	child.Env = append(os.Environ(), "MESH_MAPPED_IMAGE_SOCKET="+socket)
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn := connectMappedChild(t, ctx, socket)
	defer func() { _ = conn.Close() }()
	before, err := peerImage(conn)
	if err != nil || before.PID != child.Process.Pid {
		t.Fatalf("initial loaded image: %+v %v", before, err)
	}
	var stat unix.Stat_t
	if err = unix.Stat(original, &stat); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(root, ".mesh-update-proof.previous")
	if err = os.Link(original, backup); err != nil {
		t.Fatal(err)
	}
	writeMappedImageJournal(t, root, original)
	if active := activeInstallationPath(root, executableImage{Path: backup, Installed: backup, PID: child.Process.Pid}); active != original {
		t.Fatalf("hardlink name became installation destination: %s", active)
	}
	candidate := filepath.Join(root, "candidate")
	if err = os.WriteFile(candidate, []byte("different replacement image"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(candidate, original); err != nil {
		t.Fatal(err)
	}
	after, err := peerImage(conn)
	if err != nil || after.PID != before.PID {
		t.Fatalf("retained mapping lost after atomic replacement: %+v %v", after, err)
	}
	retained, err := os.ReadFile(after.Path) //nolint:gosec // OS-identified image path produced by the real native peer probe
	if err != nil || hashBytes(retained) != hashBytes(data) {
		t.Fatalf("probe identified replacement bytes as retained image: %v", err)
	}
	device, inode := darwinDevice(stat.Dev), stat.Ino
	fallback, ok := mappedMeshImage(original, device, inode, child.Process.Pid)
	if !ok || fallback.Path != backup {
		t.Fatalf("stale lsof pathname could not resolve exact retained inode: %+v", fallback)
	}
	copyPath := filepath.Join(root, "copy")
	if err = os.WriteFile(copyPath, data, 0600); err != nil { //nolint:gosec // fixed same-byte control file inside this test's private temporary directory
		t.Fatal(err)
	}
	if _, ok = loadedMeshImage(copyPath, device, inode, child.Process.Pid); ok {
		t.Fatal("same bytes with a different inode were accepted as mapped image")
	}
	if _, ok = loadedMeshImage(backup, device+1, inode, child.Process.Pid); ok {
		t.Fatal("same inode on a different device was accepted")
	}
	t.Logf("native replacement proof: pid=%d device=%#x inode=%d retained=%s installed=%s", child.Process.Pid, device, inode, after.Path, original)
}

func connectMappedChild(t *testing.T, ctx context.Context, socket string) net.Conn {
	t.Helper()
	for ctx.Err() == nil {
		conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
		if err == nil {
			return conn
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("native mapped-image child did not listen")
	return nil
}

func writeMappedImageJournal(t *testing.T, stateDir, executable string) {
	t.Helper()
	dir := filepath.Join(stateDir, "update")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(updateinstall.Status{Schema: 1, Settings: updateinstall.Settings{Executable: executable}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "installation.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}
