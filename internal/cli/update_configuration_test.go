package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigurationCLIRequiresExistingIdentityAndExplicitLocalGrant(t *testing.T) {
	state := t.TempDir()
	t.Setenv("MESH_STATE_DIR", state)
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	_, _, err := executeCommand(t, Dependencies{}, "update", "config", "status", strings.Repeat("a", 32), "--json")
	if err == nil {
		t.Fatal("missing identity succeeded")
	}
	if _, err = os.Stat(filepath.Join(state, "identity.key")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("identity was created", err)
	}
	setupUpdateCLI(t)
	for _, arguments := range [][]string{
		{"allow", strings.Repeat("a", 43), "--expected-revision", "0"},
		{"revoke", "--expected-revision", "0", "--coordinator", "remote"},
	} {
		_, _, err = executeCommand(t, Dependencies{}, append([]string{"update", "config"}, arguments...)...)
		if err == nil {
			t.Fatal("invalid grant invocation succeeded", arguments)
		}
	}
}
func TestConfigurationCLIRejectsUnselectedKeysAndNeedsExactApproval(t *testing.T) {
	setupUpdateCLI(t)
	input := filepath.Join(t.TempDir(), "change.json")
	root, err := os.OpenRoot(filepath.Dir(input))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err = root.WriteFile("change.json", []byte(`{"targets":[],"usageFeed":{"url":""},"domainPolicy":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, _, err = executeCommand(t, Dependencies{}, "update", "config", "preview", "--file", input, "--json")
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatal(err)
	}
	_, _, err = executeCommand(t, Dependencies{}, "update", "config", "retry", strings.Repeat("a", 32), "--json")
	if err == nil || !strings.Contains(err.Error(), "--expected-revision") {
		t.Fatal(err)
	}
}
