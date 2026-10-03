package identity

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestDeviceGrantMutationPreservesKeysAndComments(t *testing.T) {
	directory := t.TempDir()
	first, _, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "authorized_keys")
	if err := os.WriteFile(path, []byte("# owner's grants\n\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{first.ID, second.ID, first.ID} {
		if err := ApproveDevice(directory, id); err != nil {
			t.Fatal(err)
		}
	}
	grants, err := DeviceGrants(path)
	if err != nil || len(grants) != 2 {
		t.Fatalf("grants=%v, error=%v", grants, err)
	}
	if !GrantedIdentity(directory, first.ID) || !GrantedIdentity(directory, second.ID) {
		t.Fatal("approved key denied")
	}
	if err := RevokeDevice(directory, first.ID); err != nil {
		t.Fatal(err)
	}
	if GrantedIdentity(directory, first.ID) || !GrantedIdentity(directory, second.ID) {
		t.Fatal("revocation changed another device")
	}
	contents, err := ReadAuthorizedKeys(path)
	if err != nil || !bytes.HasPrefix(contents, []byte("# owner's grants\n\n")) {
		t.Fatalf("comments lost: %q, %v", contents, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("grant permissions=%v, %v", info, err)
	}
}

func TestDeviceGrantsFailClosed(t *testing.T) {
	host, _, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	if err := ApproveDevice(directory, host.ID); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "authorized_keys")
	contents, err := ReadAuthorizedKeys(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, contents := range [][]byte{append([]byte("command=\"restricted\" "), contents...), append(contents, []byte("broken key\n")...)} {
		if err := os.WriteFile(path, contents, 0600); err != nil {
			t.Fatal(err)
		}
		if GrantedIdentity(directory, host.ID) {
			t.Fatal("restricted or malformed grant file authorized full control")
		}
	}
	if err := os.WriteFile(path, contents, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0666); //nolint:gosec // prove unsafe grants fail closed
	err != nil {
		t.Fatal(err)
	}
	if GrantedIdentity(directory, host.ID) {
		t.Fatal("unsafe permissions accepted")
	}
	if err := os.Remove(path); //nolint:gosec // remove only the test-owned grant
	err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(directory, "identity.key"), path); //nolint:gosec // prove a test-owned symlink fails closed
	err != nil {
		t.Fatal(err)
	}
	if GrantedIdentity(directory, host.ID) {
		t.Fatal("symlink accepted")
	}
}
