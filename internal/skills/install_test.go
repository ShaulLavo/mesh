package skills

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

type fixture struct {
	manager Manager
	root    string
}

func setup(t *testing.T) fixture {
	t.Helper()
	home := t.TempDir()
	root := filepath.Join(home, "root")
	must(t, os.Mkdir(root, 0700))
	return fixture{Manager{ConfigDir: filepath.Join(home, "config"), StateDir: filepath.Join(home, "state"), MeshVersion: "test", files: []File{{"SKILL.md", []byte("old skill\n")}, {"references/safety.md", []byte("old safety\n")}}}, root}
}
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func apply(t *testing.T, f fixture, action string) Root {
	t.Helper()
	roots, err := f.manager.Apply(t.Context(), action, f.root)
	must(t, err)
	if len(roots) != 1 {
		t.Fatalf("roots: %#v", roots)
	}
	if len(roots[0].Problems) != 0 {
		t.Fatalf("%s: %v", action, roots[0].Problems)
	}
	return roots[0]
}
func read(t *testing.T, path string) []byte {
	t.Helper()
	dir, err := os.OpenRoot(filepath.Dir(path))
	must(t, err)
	defer func() { _ = dir.Close() }()
	data, err := dir.ReadFile(filepath.Base(path))
	must(t, err)
	return data
}
func write(t *testing.T, path string, data []byte) {
	t.Helper()
	must(t, os.WriteFile(path, data, 0600))
}
func changed(f fixture) fixture {
	f.manager.files = []File{{"SKILL.md", []byte("new skill\n")}, {"references/safety.md", []byte("new safety\n")}, {"references/commands.md", []byte("new commands\n")}}
	return f
}
func TestManagedInstallRetryAndCanonicalRegistry(t *testing.T) {
	f := setup(t)
	root := apply(t, f, "install")
	if root.State != stateCurrent || !root.Registered {
		t.Fatalf("install %#v", root)
	}
	before := read(t, filepath.Join(f.root, "mesh", manifestName))
	root = apply(t, f, "install")
	if !bytes.Equal(before, read(t, filepath.Join(f.root, "mesh", manifestName))) {
		t.Fatal("identical install changed manifest")
	}
	alias := filepath.Join(t.TempDir(), "alias")
	must(t, os.Symlink(f.root, alias))
	f.root = alias
	apply(t, f, "install")
	registry, err := f.manager.registrySnapshot()
	must(t, err)
	if len(registry.Roots) != 1 || registry.Roots[0].Path != root.Path {
		t.Fatalf("registry %#v", registry)
	}
}
func TestModifiedAndMissingSameBundle(t *testing.T) {
	f := setup(t)
	apply(t, f, "install")
	customized := filepath.Join(f.root, "mesh", "references", "safety.md")
	write(t, customized, []byte("my safety\n"))
	must(t, os.Remove(filepath.Join(f.root, "mesh", "SKILL.md")))
	roots, err := f.manager.Status(f.root)
	must(t, err)
	if roots[0].State != stateModified || len(roots[0].MissingFiles) != 1 {
		t.Fatalf("status %#v", roots)
	}
	roots, err = f.manager.Apply(t.Context(), "update", f.root)
	must(t, err)
	if roots[0].State != stateModified || len(roots[0].ProposedFiles) != 1 {
		t.Fatalf("update %#v", roots)
	}
	if string(read(t, customized)) != "my safety\n" {
		t.Fatal("customized file replaced")
	}
	if string(read(t, filepath.Join(f.root, "mesh", "SKILL.md"))) != "old skill\n" {
		t.Fatal("missing file not repaired")
	}
	proposed := roots[0].ProposedFiles[0]
	write(t, proposed, []byte("my proposed edit\n"))
	result := apply(t, f, actionUninstall)
	if len(result.RetainedFiles) != 2 || string(read(t, proposed)) != "my proposed edit\n" || string(read(t, customized)) != "my safety\n" {
		t.Fatalf("uninstall lost edits %#v", result)
	}
	if _, err := os.Stat(filepath.Join(f.root, "mesh", manifestName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("manifest retained")
	}
}
func TestUserModifiedProposalSurvivesAnotherUpdate(t *testing.T) {
	f := setup(t)
	apply(t, f, "install")
	f = changed(f)
	write(t, filepath.Join(f.root, "mesh", "SKILL.md"), []byte("custom"))
	roots, err := f.manager.Apply(t.Context(), "update", f.root)
	must(t, err)
	proposal := roots[0].ProposedFiles[0]
	write(t, proposal, []byte("edited proposal"))
	roots, err = f.manager.Apply(t.Context(), "update", f.root)
	must(t, err)
	if len(roots[0].Problems) == 0 || string(read(t, proposal)) != "edited proposal" {
		t.Fatal("proposal edit was not preserved")
	}
	result := apply(t, f, actionUninstall)
	if len(result.RetainedFiles) != 2 {
		t.Fatalf("retained %#v", result)
	}
}
func TestRegistryFailureRetryReconcilesTreeIndependently(t *testing.T) {
	f := setup(t)
	f.manager.syncDir = failOnce(registryName)
	roots, err := f.manager.Apply(t.Context(), "install", f.root)
	must(t, err)
	if len(roots[0].Problems) == 0 || roots[0].RegistrationState != "unconfirmed" {
		t.Fatalf("no partial registration %#v", roots)
	}
	f.manager.syncDir = nil
	apply(t, f, "install")
	registry, err := f.manager.registrySnapshot()
	must(t, err)
	if len(registry.Roots) != 1 {
		t.Fatal("retry did not register exactly once")
	}
	f.manager.syncDir = failOnce(registryName)
	roots, err = f.manager.Apply(t.Context(), actionUninstall, f.root)
	must(t, err)
	if len(roots[0].Problems) == 0 {
		t.Fatal("uninstall registry failure hidden")
	}
	if _, err := os.Stat(filepath.Join(f.root, "mesh")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("tree was not removed")
	}
	f.manager.syncDir = nil
	apply(t, f, actionUninstall)
	registry, err = f.manager.registrySnapshot()
	must(t, err)
	if len(registry.Roots) != 0 {
		t.Fatal("stale registration retained")
	}
}
func failOnce(label string) func(*os.File, string) error {
	failed := false
	return func(dir *os.File, operation string) error {
		if operation == label && !failed {
			failed = true
			return errors.New("injected directory barrier failure")
		}
		return dir.Sync()
	}
}
func TestInterruptedNestedUpdateUsesBothBaselines(t *testing.T) {
	for _, label := range []string{"SKILL.md", "references/safety.md", manifestName} {
		t.Run(label, func(t *testing.T) { checkInterruptedUpdate(t, label) })
	}
}
func checkInterruptedUpdate(t *testing.T, label string) {
	t.Helper()
	f := setup(t)
	apply(t, f, "install")
	f = changed(f)
	f.manager.syncDir = failOnce(label)
	roots, err := f.manager.Apply(t.Context(), "update", f.root)
	must(t, err)
	if len(roots[0].Problems) == 0 {
		t.Fatal("barrier failure acknowledged")
	}
	manifest, err := parseManifest(read(t, filepath.Join(f.root, "mesh", manifestName)))
	must(t, err)
	if manifest.Pending == nil {
		t.Fatal("failed update dropped pending metadata")
	}
	status, err := f.manager.Status(f.root)
	must(t, err)
	if status[0].State != stateInterrupted {
		t.Fatal("interrupted update reported current")
	}
	f.manager.syncDir = nil
	root := apply(t, f, "update")
	if root.State != stateCurrent || len(root.ModifiedFiles) != 0 {
		t.Fatalf("own write classified as customization %#v", root)
	}
	for _, file := range f.manager.files {
		if !bytes.Equal(file.Data, read(t, filepath.Join(f.root, "mesh", file.Path))) {
			t.Fatal("retry lost file")
		}
	}
}
func TestFinalManifestBarrierFailureRetry(t *testing.T) {
	f := setup(t)
	apply(t, f, "install")
	f = changed(f)
	count := 0
	f.manager.syncDir = func(dir *os.File, label string) error {
		if label == manifestName {
			count++
		}
		if count == 2 {
			return errors.New("final manifest barrier")
		}
		return dir.Sync()
	}
	roots, err := f.manager.Apply(t.Context(), "update", f.root)
	must(t, err)
	if len(roots[0].Problems) == 0 || roots[0].State == stateCurrent {
		t.Fatal("unconfirmed final publication claimed success")
	}
	f.manager.syncDir = nil
	apply(t, f, "update")
}
func TestUnsafeManifestDoesNotDeleteFiles(t *testing.T) {
	for _, name := range []string{"../outside", "/absolute", ".", "references/../SKILL.md", "references//safety.md", manifestName, "references/"} {
		t.Run(name, func(t *testing.T) { checkUnsafeManifest(t, name) })
	}
}
func checkUnsafeManifest(t *testing.T, name string) {
	t.Helper()
	f := setup(t)
	apply(t, f, "install")
	manifest := Manifest{Version: 1, Bundle: hashBytes(nil), Files: []ManagedFile{{name, hashBytes([]byte("old skill\n"))}}}
	data, err := json.Marshal(manifest)
	must(t, err)
	write(t, filepath.Join(f.root, "mesh", manifestName), data)
	roots, err := f.manager.Apply(t.Context(), actionUninstall, f.root)
	must(t, err)
	if len(roots[0].Problems) == 0 {
		t.Fatal("unsafe manifest accepted")
	}
	if string(read(t, filepath.Join(f.root, "mesh", "SKILL.md"))) != "old skill\n" {
		t.Fatal("invalid manifest deleted file")
	}
}
func TestUnsafeRootAndManagedLinks(t *testing.T) {
	for _, kind := range []string{"missing", "writable-root", "tree-link", "nested-link", "file-link", "hardlink", "fifo"} {
		t.Run(kind, func(t *testing.T) { checkUnsafeRoot(t, kind) })
	}
}
func checkUnsafeRoot(t *testing.T, kind string) {
	t.Helper()
	f := setup(t)
	if kind == "missing" {
		f.root = filepath.Join(f.root, "absent")
		_, err := f.manager.Apply(t.Context(), "install", f.root)
		if err == nil {
			t.Fatal("missing root accepted")
		}
		return
	}
	if kind == "writable-root" {
		must(t, unix.Chmod(f.root, 0777))
		roots, err := f.manager.Apply(t.Context(), "install", f.root)
		must(t, err)
		if len(roots[0].Problems) == 0 {
			t.Fatal("writable root accepted")
		}
		return
	}
	apply(t, f, "install")
	tree := filepath.Join(f.root, "mesh")
	target := filepath.Join(tree, "SKILL.md")
	switch kind {
	case "tree-link":
		must(t, os.Rename(tree, tree+"-saved"))
		must(t, os.Symlink(tree+"-saved", tree))
	case "nested-link":
		nested := filepath.Join(tree, "references")
		must(t, os.Rename(nested, nested+"-saved"))
		must(t, os.Symlink(nested+"-saved", nested))
	case "file-link":
		must(t, os.Rename(target, target+"-saved"))
		must(t, os.Symlink(target+"-saved", target))
	case "hardlink":
		must(t, os.Link(target, target+"-linked"))
	case "fifo":
		must(t, os.Remove(target))
		must(t, unix.Mkfifo(target, 0600))
	}
	roots, err := f.manager.Apply(t.Context(), actionUninstall, f.root)
	must(t, err)
	if len(roots[0].Problems) == 0 {
		t.Fatal("unsafe managed path accepted")
	}
}
func TestAnchoredDescriptorsDoNotFollowRetargetedTree(t *testing.T) {
	f := setup(t)
	apply(t, f, "install")
	f = changed(f)
	outside := t.TempDir()
	write(t, filepath.Join(outside, "SKILL.md"), []byte("outside sentinel"))
	moved := false
	f.manager.syncDir = func(dir *os.File, label string) error {
		if label == manifestName && !moved {
			moved = true
			must(t, os.Rename(filepath.Join(f.root, "mesh"), filepath.Join(f.root, "old")))
			must(t, os.Symlink(outside, filepath.Join(f.root, "mesh")))
		}
		return dir.Sync()
	}
	apply(t, f, "update")
	if string(read(t, filepath.Join(outside, "SKILL.md"))) != "outside sentinel" {
		t.Fatal("retarget escaped anchored tree")
	}
	if string(read(t, filepath.Join(f.root, "old", "SKILL.md"))) != "new skill\n" {
		t.Fatal("anchored tree was not updated")
	}
}
func TestRegistryBoundsAndAllRoots(t *testing.T) {
	f := setup(t)
	apply(t, f, "install")
	second := filepath.Join(t.TempDir(), "second")
	must(t, os.Mkdir(second, 0700))
	other := f
	other.root = second
	apply(t, other, "install")
	roots, err := f.manager.Status("")
	must(t, err)
	if len(roots) != 2 {
		t.Fatalf("roots %#v", roots)
	}
	registry, err := f.manager.registrySnapshot()
	must(t, err)
	for len(registry.Roots) < maximumRoots {
		registry.Roots = append(registry.Roots, registeredRoot{Path: fmt.Sprintf("/fixture/%d", len(registry.Roots)), InstalledAt: registry.Roots[0].InstalledAt})
	}
	encoded, err := json.Marshal(registry)
	must(t, err)
	write(t, filepath.Join(f.manager.ConfigDir, registryName), encoded)
	third := filepath.Join(t.TempDir(), "third")
	must(t, os.Mkdir(third, 0700))
	other.root = third
	results, err := other.manager.Apply(t.Context(), "install", third)
	must(t, err)
	if len(results[0].Problems) == 0 {
		t.Fatal("33rd registration accepted")
	}
}
func TestManifestStrictValidation(t *testing.T) {
	hash := hashBytes(nil)
	for _, data := range []string{
		`{"version":1,"version":1}`, `{"Version":1}`, `{} {}`, strings.Repeat(" ", maximumMetadata+1),
		fmt.Sprintf(`{"version":1,"bundle":%q,"files":[{"path":"x","sha256":%q},{"path":"X","sha256":%q}]}`, hash, hash, hash),
		fmt.Sprintf(`{"version":1,"bundle":%q,"files":[{"path":"x","sha256":%q},{"path":"x/y","sha256":%q}]}`, hash, hash, hash),
	} {
		if _, err := parseManifest([]byte(data)); err == nil {
			t.Fatalf("unsafe metadata accepted %q", data[:min(120, len(data))])
		}
	}
}
func TestLockIsBoundedAndCancellationDoesNotChangeTree(t *testing.T) {
	f := setup(t)
	apply(t, f, "install")
	dir, err := openDirectory(f.manager.ConfigDir, false)
	must(t, err)
	defer func() { _ = dir.Close() }()
	lock, err := lockDirectory(t.Context(), dir)
	must(t, err)
	defer func() { _ = lock.Close() }()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = f.manager.Apply(ctx, "update", f.root)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("lock ignored cancellation: %v", err)
	}
}

func TestDroppedModifiedFilesAndUnregisteredRetry(t *testing.T) {
	f := setup(t)
	apply(t, f, "install")
	customized := filepath.Join(f.root, "mesh", "references", "safety.md")
	write(t, customized, []byte("retained dropped file"))
	f.manager.files = []File{{"SKILL.md", []byte("new skill")}}
	roots, err := f.manager.Apply(t.Context(), "update", f.root)
	must(t, err)
	if roots[0].State != stateModified {
		t.Fatal("dropped customization not reported")
	}
	status, err := f.manager.Status(f.root)
	must(t, err)
	if status[0].State != stateModified {
		t.Fatal("manifest forgot dropped customized file")
	}
	f.manager.syncDir = failOnce(registryName)
	roots, err = f.manager.Apply(t.Context(), actionUninstall, f.root)
	must(t, err)
	if roots[0].RegistrationState != "unconfirmed" {
		t.Fatal("registration failure hidden")
	}
	f.manager.syncDir = nil
	apply(t, f, actionUninstall)
	registry, err := f.manager.registrySnapshot()
	must(t, err)
	if len(registry.Roots) != 0 || string(read(t, customized)) != "retained dropped file" {
		t.Fatal("stale registration or deleted customization")
	}
}
func TestAbsentRegistrationRepairAndMissingRegisteredRoot(t *testing.T) {
	f := setup(t)
	blocked := false
	f.manager.syncDir = func(dir *os.File, label string) error {
		if label == "mesh" && !blocked {
			blocked = true
			must(t, os.Symlink("absent-target", filepath.Join(f.manager.ConfigDir, registryName)))
		}
		return dir.Sync()
	}
	roots, err := f.manager.Apply(t.Context(), "install", f.root)
	must(t, err)
	if len(roots[0].Problems) == 0 {
		t.Fatal("registration symlink accepted")
	}
	must(t, os.Remove(filepath.Join(f.manager.ConfigDir, registryName)))
	f.manager.syncDir = nil
	apply(t, f, "install")
	must(t, os.Rename(f.root, f.root+"-moved"))
	status, err := f.manager.Status(f.root)
	must(t, err)
	if len(status) != 1 || status[0].State != stateMissing || !status[0].Registered {
		t.Fatalf("missing registered root %#v", status)
	}
	apply(t, f, actionUninstall)
}
