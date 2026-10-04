package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestClientCommandRetiresObsoleteAliasState(t *testing.T) {
	t.Setenv("MESH_STATE_DIR", t.TempDir())
	directory := t.TempDir()
	t.Setenv("MESH_CONFIG_DIR", directory)
	path := filepath.Join(directory, "hosts.json")
	retained := []byte(`{"version":1,"hosts":[{"alias":"obsolete","id":"owner","meshIdentity":"pin","endpoint":"ws://127.0.0.1:7777/mesh","addresses":["127.0.0.1"]}],"dashboard":{"theme":"oled"}}`)
	if err := os.WriteFile(path, retained, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil { //nolint:gosec // retirement must accept a legacy user-owned address book
		t.Fatal(err)
	}
	if _, _, err := executeCommand(t, Dependencies{}, "device", "identity", "--json"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("retired config permissions = %04o, want 0600", info.Mode().Perm())
	}
	after, err := readClientConfigTestFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var beforeObject, afterObject map[string]any
	if err := json.Unmarshal(retained, &beforeObject); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(after, &afterObject); err != nil {
		t.Fatal(err)
	}
	delete(beforeObject["hosts"].([]any)[0].(map[string]any), "alias")
	expected, err := json.Marshal(beforeObject)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := json.Marshal(afterObject)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, expected) {
		t.Fatal("client activation did not delete only obsolete aliases")
	}
	if _, err := LoadHosts(); err != nil {
		t.Fatal(err)
	}
}

func TestClientActivationAcceptsExistingConfigPermissions(t *testing.T) {
	for _, mode := range []os.FileMode{0o444, 0o640, 0o644, 0o666, 0o755} {
		t.Run(fmt.Sprintf("%04o", mode), func(t *testing.T) {
			contents := []byte(`{"version":1,"hosts":[]}`)
			path := writeClientConfigFixture(t, contents)
			if err := os.Chmod(path, mode); err != nil { //nolint:gosec // legacy user-owned configuration may have group or other permission bits
				t.Fatal(err)
			}
			directory := filepath.Dir(path)
			if err := os.Chmod(directory, 0o500); err != nil { //nolint:gosec // current-book activation must work in a read-only owned directory
				t.Fatal(err)
			}
			defer func() { _ = os.Chmod(directory, 0o700) }() //nolint:gosec // restore the owned fixture directory for cleanup
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := executeCommand(t, Dependencies{}, "shell-init", "bash"); err != nil {
				t.Fatalf("CLI activation rejected existing config: %v", err)
			}
			// Exercise the daemon's startup hook without starting a host service.
			root := NewCommand(Dependencies{})
			root.SetContext(context.Background())
			daemon, _, err := root.Find([]string{"daemon"})
			if err != nil {
				t.Fatal(err)
			}
			if err := activateClientConfig(daemon); err != nil {
				t.Fatalf("daemon activation rejected existing config: %v", err)
			}
			after, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			expected := mode
			if mode&0o022 != 0 {
				expected = mode & 0o700
			}
			if !os.SameFile(before, after) || after.Mode().Perm() != expected {
				t.Fatalf("activation changed config inode or permissions: got %04o, want %04o", after.Mode().Perm(), expected)
			}
			if _, err := os.Stat(filepath.Join(directory, ".hosts.lock")); !os.IsNotExist(err) {
				t.Fatalf("current-book activation created a writer lock: %v", err)
			}
			actual, err := readClientConfigTestFile(path)
			if err != nil || !bytes.Equal(actual, contents) {
				t.Fatal("activation changed current config contents")
			}
		})
	}
}

func TestClientCommandRefusesUnknownConfigWithoutMutation(t *testing.T) {
	t.Setenv("MESH_STATE_DIR", t.TempDir())
	directory := t.TempDir()
	t.Setenv("MESH_CONFIG_DIR", directory)
	path := filepath.Join(directory, "hosts.json")
	retained := []byte(`{"version":1,"hosts":[],"unexpected":true}`)
	if err := os.WriteFile(path, retained, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := executeCommand(t, Dependencies{}, "device", "identity", "--json"); err == nil {
		t.Fatal("client accepted unrecognized configuration")
	}
	after, err := readClientConfigTestFile(path)
	if err != nil || !bytes.Equal(after, retained) {
		t.Fatal("failed retirement changed unrecognized configuration")
	}
}
