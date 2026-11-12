package identity

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestGrantSnapshotReplacedBeforeDescriptorInspection(t *testing.T) {
	state := t.TempDir()
	path := filepath.Join(state, "authorized_keys")
	old := []byte("# old policy\n")
	next := []byte("# published policy\n")
	if err := publishGrants(state, path, old); err != nil {
		t.Fatal(err)
	}
	reader := grantSnapshotReader{t: t, root: grantFixtureRoot(t, state)}
	reader.afterOpen = func(file *os.File) {
		if len(reader.files) == 1 {
			replaceOpenedGrant(t, state, path, next, file)
		}
	}
	contents, err := readAuthorizedKeys(path, reader.open)
	if err != nil || !bytes.Equal(contents, next) {
		t.Fatalf("snapshot=%q, error=%v; want published policy", contents, err)
	}
	if len(reader.files) != 2 {
		t.Fatalf("opened %d descriptors; want two", len(reader.files))
	}
}

func replaceOpenedGrant(t *testing.T, state, path string, next []byte, file *os.File) {
	t.Helper()
	if err := publishGrants(state, path, next); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	assertRetiredGrantDescriptor(t, file)
}

func assertRetiredGrantDescriptor(t *testing.T, file *os.File) {
	t.Helper()
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 0 {
		_ = file.Close()
		t.Fatalf("replaced descriptor links=%v; want zero", info.Sys())
	}
}

func TestGrantSnapshotRefreshObservesRevocation(t *testing.T) {
	state, id, other := t.TempDir(), grantActor(t), grantActor(t)
	for _, actor := range []string{id, other} {
		if err := ApproveDevice(t.Context(), state, actor); err != nil {
			t.Fatal(err)
		}
	}
	bound, ok := BindIdentity(state, id)
	if !ok {
		t.Fatal("approved fixture grant missing")
	}
	reader := grantSnapshotReader{t: t, root: grantFixtureRoot(t, state)}
	reader.afterOpen = func(file *os.File) {
		if len(reader.files) != 1 {
			return
		}
		if err := RevokeDevice(t.Context(), state, id); err != nil {
			t.Fatal(err)
		}
		assertRetiredGrantDescriptor(t, file)
	}
	contents, err := readAuthorizedKeys(filepath.Join(state, "authorized_keys"), reader.open)
	if err != nil {
		t.Fatal(err)
	}
	grants, err := parseDeviceGrants(contents)
	if err != nil || len(grants) != 1 || grants[0].Identity != other || bound() {
		t.Fatalf("revocation snapshot=%v, error=%v", grants, err)
	}
	if err := ApproveDevice(t.Context(), state, id); err != nil {
		t.Fatal(err)
	}
	if bound() || !GrantedIdentity(state, id) || !GrantedIdentity(state, other) {
		t.Fatal("refresh or reapproval healed revoked binding or lost unrelated grant")
	}
}

type grantSnapshotReader struct {
	t         *testing.T
	root      *os.Root
	files     []*os.File
	afterOpen func(*os.File)
}

func (r *grantSnapshotReader) open(name string, flags int, mode os.FileMode) (*os.File, error) {
	r.t.Helper()
	if flags != os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK || mode != 0 {
		r.t.Fatalf("unsafe snapshot open flags=%d, mode=%v", flags, mode)
	}
	if len(r.files) != 0 {
		_, err := r.files[len(r.files)-1].Stat()
		if !errors.Is(err, os.ErrClosed) {
			r.t.Fatalf("refresh retained prior descriptor: %v", err)
		}
	}
	// Root.OpenFile resolves symlinks inside its root, even with O_NOFOLLOW.
	// Use the native open flags here, as the production opener does.
	fd, err := syscall.Open(filepath.Join(r.root.Name(), filepath.Base(name)), flags|syscall.O_CLOEXEC, uint32(mode))
	if err != nil {
		return nil, grantFixtureError(err)
	}
	file := os.NewFile(uintptr(fd), name)
	r.files = append(r.files, file)
	r.t.Cleanup(func() { _ = file.Close() })
	if r.afterOpen != nil {
		r.afterOpen(file)
	}
	return file, nil
}

func TestGrantSnapshotReplacementIsBounded(t *testing.T) {
	state := t.TempDir()
	path := filepath.Join(state, "authorized_keys")
	policy := []byte("# policy\n")
	if err := publishGrants(state, path, policy); err != nil {
		t.Fatal(err)
	}
	reader := grantSnapshotReader{t: t, root: grantFixtureRoot(t, state)}
	reader.afterOpen = func(file *os.File) { replaceOpenedGrant(t, state, path, policy, file) }
	contents, err := readAuthorizedKeys(path, reader.open)
	if len(contents) != 0 || !errors.Is(err, errRetiredGrantSnapshot) || len(reader.files) != 2 {
		t.Fatalf("snapshot=%q, error=%v, opens=%d; want bounded failure", contents, err, len(reader.files))
	}
	for _, file := range reader.files {
		if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("snapshot retained descriptor after failure: %v", err)
		}
	}
}

func TestGrantSnapshotUnsafeReplacementFailsClosed(t *testing.T) {
	for _, kind := range []string{"hardlink", "symlink", "fifo", "directory", "unsafe-mode", "unreadable", "oversized", "missing"} {
		t.Run(kind, func(t *testing.T) { checkUnsafeGrantSnapshot(t, kind) })
	}
}

func checkUnsafeGrantSnapshot(t *testing.T, kind string) {
	t.Helper()
	state := t.TempDir()
	path := filepath.Join(state, "authorized_keys")
	if err := publishGrants(state, path, []byte("# policy\n")); err != nil {
		t.Fatal(err)
	}
	reader := grantSnapshotReader{t: t, root: grantFixtureRoot(t, state)}
	reader.afterOpen = func(file *os.File) {
		if len(reader.files) != 1 {
			return
		}
		replaceOpenedGrant(t, state, path, []byte("# replacement\n"), file)
		damageGrantSnapshot(t, state, reader.root, kind)
	}
	contents, err := readAuthorizedKeys(path, reader.open)
	if len(contents) != 0 || err == nil || errors.Is(err, errRetiredGrantSnapshot) {
		t.Fatalf("unsafe snapshot=%q, error=%v", contents, err)
	}
}

func damageGrantSnapshot(t *testing.T, state string, root *os.Root, kind string) {
	t.Helper()
	var err error
	switch kind {
	case "hardlink":
		err = root.Link("authorized_keys", "alias")
	case "unsafe-mode":
		err = root.Chmod("authorized_keys", 0620)
	case "unreadable":
		err = root.Chmod("authorized_keys", 0200)
	case "oversized":
		err = root.WriteFile("authorized_keys", bytes.Repeat([]byte("#"), AuthorizedKeysMaximum+1), 0600)
	default:
		err = root.Remove("authorized_keys")
		if err == nil {
			err = specialGrantSnapshot(state, root, kind)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
}

func specialGrantSnapshot(state string, root *os.Root, kind string) error {
	var err error
	switch kind {
	case "symlink":
		err = root.WriteFile("target", []byte("# policy\n"), 0600)
		if err == nil {
			err = root.Symlink("target", "authorized_keys")
		}
	case "fifo":
		err = syscall.Mkfifo(filepath.Join(state, "authorized_keys"), 0600)
	case "directory":
		err = root.Mkdir("authorized_keys", 0700)
	}
	return grantFixtureError(err)
}

func TestGrantSnapshotDoesNotRefreshOtherFailures(t *testing.T) {
	state := t.TempDir()
	reader := grantSnapshotReader{t: t, root: grantFixtureRoot(t, state)}
	if err := reader.root.WriteFile("authorized_keys", []byte("# policy\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := reader.root.Link("authorized_keys", "alias"); err != nil {
		t.Fatal(err)
	}
	contents, err := readAuthorizedKeys(filepath.Join(state, "authorized_keys"), reader.open)
	if len(contents) != 0 || err == nil || errors.Is(err, errRetiredGrantSnapshot) || len(reader.files) != 1 {
		t.Fatalf("hard link snapshot=%q, error=%v, opens=%d", contents, err, len(reader.files))
	}
	calls := 0
	open := func(string, int, os.FileMode) (*os.File, error) { calls++; return nil, os.ErrPermission }
	if _, err := readAuthorizedKeys("unused", open); !errors.Is(err, os.ErrPermission) || calls != 1 {
		t.Fatalf("open failure=%v, calls=%d; want one failed open", err, calls)
	}
}

func TestGrantSnapshotDoesNotRefreshDescriptorErrors(t *testing.T) {
	for _, kind := range []string{"stat", "read", "unsafe-unlinked"} {
		t.Run(kind, func(t *testing.T) { checkGrantDescriptorFailure(t, kind) })
	}
}

func checkGrantDescriptorFailure(t *testing.T, kind string) {
	t.Helper()
	state := t.TempDir()
	path := filepath.Join(state, "authorized_keys")
	root := grantFixtureRoot(t, state)
	if err := root.WriteFile("authorized_keys", []byte("# policy\n"), 0600); err != nil {
		t.Fatal(err)
	}
	reader := grantSnapshotReader{t: t, root: root}
	reader.afterOpen = func(file *os.File) {
		failGrantDescriptor(t, state, path, kind, file)
	}
	open := func(name string, flags int, mode os.FileMode) (*os.File, error) {
		if kind == "read" {
			// A real write-only descriptor passes stat but fails the bounded read.
			file, err := root.OpenFile("authorized_keys", os.O_WRONLY, 0)
			reader.files = append(reader.files, file)
			return file, grantFixtureError(err)
		}
		return reader.open(name, flags, mode)
	}
	contents, err := readAuthorizedKeys(path, open)
	if len(contents) != 0 || err == nil || errors.Is(err, errRetiredGrantSnapshot) || len(reader.files) != 1 {
		t.Fatalf("snapshot=%q, error=%v, opens=%d; want original descriptor failure", contents, err, len(reader.files))
	}
}

func failGrantDescriptor(t *testing.T, state, path, kind string, file *os.File) {
	t.Helper()
	switch kind {
	case "stat":
		_ = file.Close()
	case "unsafe-unlinked":
		if err := file.Chmod(0620); err != nil {
			t.Fatal(err)
		}
		replaceOpenedGrant(t, state, path, []byte("# safe replacement\n"), file)
	}
}
