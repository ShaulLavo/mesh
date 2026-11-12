package updatebootstrap

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func linkedServiceFixture(t *testing.T) (string, string) {
	t.Helper()
	physical := t.TempDir()
	root := filepath.Join(t.TempDir(), "linked-root")
	if err := os.Symlink(physical, root); err != nil {
		t.Fatal(err)
	}
	runtime := filepath.Join(root, "run", "user", "systemd", "user")
	home := filepath.Join(root, "home", "user", ".config", "systemd", "user")
	for _, directory := range []string{runtime, home} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(home, "mesh.service")
	if err := os.WriteFile(target, []byte("[Service]\nKillMode=process\n"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(runtime, "mesh.service")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	return link, target
}

func TestServiceFileDigestAcceptsNativeLinkedDefinition(t *testing.T) {
	link, target := linkedServiceFixture(t)
	digest, err := serviceFileDigest(link)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := serviceFileDigest(target)
	if err != nil || actual != digest {
		t.Fatal("runtime link did not read its exact owned definition")
	}
	files, err := captureServiceFiles([]string{link})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(files, map[string]string{resolved: digest}) {
		t.Fatal("snapshot did not bind the resolved definition path and content")
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(filepath.Dir(link), target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(relative, link); err != nil {
		t.Fatal(err)
	}
	relativeFiles, err := captureServiceFiles([]string{link})
	if err != nil || !reflect.DeepEqual(relativeFiles, files) {
		t.Fatal("relative native link changed the captured definition")
	}
}

func TestServiceFileSnapshotRejectsChangedTarget(t *testing.T) {
	link, target := linkedServiceFixture(t)
	before, err := captureServiceFiles([]string{link})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("[Service]\nKillMode=process\nEnvironment=FIXTURE=changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	after, err := captureServiceFiles([]string{link})
	if err != nil {
		t.Fatal(err)
	}
	if serviceFilesDigest(before) == serviceFilesDigest(after) {
		t.Fatal("changed linked contents retained the same service snapshot")
	}
}

func TestServiceFileSnapshotBindsIdenticalContentTargetPath(t *testing.T) {
	link, target := linkedServiceFixture(t)
	before, err := captureServiceFiles([]string{link})
	if err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(filepath.Dir(target), "other.service")
	if err := os.WriteFile(second, []byte("[Service]\nKillMode=process\n"), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(filepath.Dir(target))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	file, err := root.Open(filepath.Base(target))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	identity, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(second, link); err != nil {
		t.Fatal(err)
	}
	if err := settleServiceDefinition(link, target, file, identity); err == nil {
		t.Fatal("retarget during inspection retained the original identity")
	}
	after, err := captureServiceFiles([]string{link})
	if err != nil {
		t.Fatal(err)
	}
	if serviceFilesDigest(before) == serviceFilesDigest(after) {
		t.Fatal("identical bytes at another target lost their path binding")
	}
}

func TestServiceFileDigestRejectsInvalidTargets(t *testing.T) {
	for _, kind := range []string{"missing", "directory", "fifo", "loop", "writable", "oversized"} {
		t.Run(kind, func(t *testing.T) { testInvalidServiceTarget(t, kind) })
	}
}

func testInvalidServiceTarget(t *testing.T, kind string) {
	link, target := linkedServiceFixture(t)
	root, err := os.OpenRoot(filepath.Dir(target))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if kind != "writable" && kind != "oversized" {
		if err := os.Remove(target); err != nil {
			t.Fatal(err)
		}
	}
	switch kind {
	case "missing":
	case "directory":
		if err := os.Mkdir(target, 0700); err != nil {
			t.Fatal(err)
		}
	case "fifo":
		if err := syscall.Mkfifo(target, 0600); err != nil {
			t.Fatal(err)
		}
	case "loop":
		if err := os.Symlink(link, target); err != nil {
			t.Fatal(err)
		}
	case "writable":
		if err := root.Chmod("mesh.service", 0622); err != nil {
			t.Fatal(err)
		}
	case "oversized":
		if err := root.WriteFile("mesh.service", []byte(strings.Repeat("x", (1<<20)+1)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := serviceFileDigest(link); err == nil {
		t.Fatal("invalid linked service definition was accepted")
	}
}

type serviceDefinitionOwnerInfo struct {
	os.FileInfo
	stat syscall.Stat_t
}

func (info serviceDefinitionOwnerInfo) Sys() any { return &info.stat }

func TestServiceFileDefinitionRejectsForeignOwner(t *testing.T) {
	_, target := linkedServiceFixture(t)
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	stat := *info.Sys().(*syscall.Stat_t)
	stat.Uid = 12345
	if int64(stat.Uid) == int64(os.Geteuid()) {
		stat.Uid++
	}
	if err := validateServiceDefinition(serviceDefinitionOwnerInfo{FileInfo: info, stat: stat}); err == nil {
		t.Fatal("another account's definition was accepted")
	}
}
