package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/shaul/mesh/internal/updateinstall"
)

func TestVersionClientRetirementActivationBoundary(t *testing.T) {
	for _, example := range []struct {
		name       string
		phase      updateinstall.Phase
		clientOnly bool
		installed  bool
		retired    bool
	}{
		{name: "accepted-client", phase: updateinstall.Accepted, clientOnly: true, installed: true},
		{name: "staged-client", phase: updateinstall.Staged, clientOnly: true, installed: true},
		{name: "validating-staged-image", phase: updateinstall.Validating, clientOnly: true},
		{name: "validating-installed-client", phase: updateinstall.Validating, clientOnly: true, installed: true, retired: true},
		{name: "validating-daemon-metadata", phase: updateinstall.Validating, installed: true},
		{name: "committed-client-metadata", phase: updateinstall.Committed, clientOnly: true, installed: true},
	} {
		t.Run(example.name, func(t *testing.T) {
			path := writeClientConfigFixture(t, obsoleteConfigFixture())
			state := t.TempDir()
			t.Setenv("MESH_STATE_DIR", state)
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			if !example.installed {
				executable = filepath.Join(t.TempDir(), "installed-image")
				if err := os.WriteFile(executable, []byte("different installed image"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			status := updateinstall.Status{Schema: 1, Phase: example.phase, Settings: updateinstall.Settings{StateDir: state, Executable: executable, ClientOnly: example.clientOnly}}
			writeClientActivationJournal(t, state, status)
			if _, _, err := executeCommand(t, Dependencies{}, "version", "--json"); err != nil {
				t.Fatal(err)
			}
			after, err := readClientConfigTestFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(after, []byte(`"alias"`)) == example.retired {
				t.Fatal("client retirement crossed its installed validation boundary")
			}
		})
	}
}

func TestVersionMetadataDoesNotCreateOrRetireState(t *testing.T) {
	path := writeClientConfigFixture(t, obsoleteConfigFixture())
	state := filepath.Join(t.TempDir(), "absent-state")
	t.Setenv("MESH_STATE_DIR", state)
	if _, _, err := executeCommand(t, Dependencies{}, "version", "--json"); err != nil {
		t.Fatal(err)
	}
	after, err := readClientConfigTestFile(path)
	if err != nil || !bytes.Equal(after, obsoleteConfigFixture()) {
		t.Fatal("metadata probe retired client state")
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("metadata probe created workload state: %v", err)
	}
}

func TestVersionInstalledValidationRefusesUnknownConfig(t *testing.T) {
	contents := []byte(`{"version":1,"hosts":[],"unexpected":true}`)
	path := writeClientConfigFixture(t, contents)
	state := t.TempDir()
	t.Setenv("MESH_STATE_DIR", state)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	writeClientActivationJournal(t, state, updateinstall.Status{Schema: 1, Phase: updateinstall.Validating, Settings: updateinstall.Settings{StateDir: state, Executable: executable, ClientOnly: true}})
	if _, _, err := executeCommand(t, Dependencies{}, "version", "--json"); err == nil {
		t.Fatal("installed-client validation ignored unknown configuration")
	}
	after, err := readClientConfigTestFile(path)
	if err != nil || !bytes.Equal(after, contents) {
		t.Fatal("failed validation changed configuration")
	}
}

func writeClientActivationJournal(t *testing.T, state string, status updateinstall.Status) {
	t.Helper()
	data, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(state, "update")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "installation.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}
