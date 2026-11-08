package cli

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/shaul/mesh/internal/update"
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

func TestConfigurationCurrentStatusHumanAndCompleteJSON(t *testing.T) {
	_, host := setupUpdateCLI(t)
	status := update.ConfigurationStatus{Revision: 12, Change: &update.ConfigurationChange{Proposal: update.ConfigurationProposal{ID: strings.Repeat("a", 32), CoordinatorID: host.ID, Digest: strings.Repeat("b", 64)}, Blocked: "private blockage", Targets: []update.ConfigurationTarget{{HostID: host.ID, State: "uncertain", Issued: true, Problem: "private error"}}}}
	caller := updateCallFunc(func(_ context.Context, _ update.Host, action string, input, output any) error {
		if action != "configuration.status" {
			return errors.New("unexpected presentation mutation")
		}
		encoded, err := json.Marshal(input)
		if err != nil || !strings.Contains(string(encoded), `"id":""`) {
			t.Error("current status did not use existing empty selector", string(encoded), err)
		}
		*output.(*update.ConfigurationStatus) = status
		return nil
	})
	stdout, _, err := executeCommand(t, Dependencies{UpdateCaller: caller}, "update", "config", "status", "--privacy")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "Configuration delivery blocked") || !strings.Contains(stdout, "publication uncertain") {
		t.Fatal(stdout)
	}
	for _, private := range []string{host.ID, status.Change.Proposal.ID, status.Change.Proposal.Digest, "private blockage", "private error"} {
		if strings.Contains(stdout, private) {
			t.Fatalf("default leaked %q: %s", private, stdout)
		}
	}
	stdout, _, err = executeCommand(t, Dependencies{UpdateCaller: caller}, "update", "config", "status", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var decoded update.ConfigurationStatus
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatal(stdout, err)
	}
	if decoded.Revision != 12 || decoded.Change.Proposal.Digest != status.Change.Proposal.Digest || decoded.Change.Targets[0].Problem != "private error" {
		t.Fatal(decoded)
	}
	for _, action := range []string{"retry", "cancel"} {
		_, _, err = executeCommand(t, Dependencies{}, "update", "config", action)
		if err == nil {
			t.Fatalf("%s accepted absent change ID", action)
		}
	}
}
