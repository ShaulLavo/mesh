package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/updateinstall"
)

func TestClientRetirementRejectsCaseFoldAmbiguity(t *testing.T) {
	canonical := obsoleteConfigFixture()
	writeClientConfigFixture(t, canonical)
	if err := retireClientAliases(context.Background()); err != nil {
		t.Fatal(err)
	}
	hosts, err := LoadHosts()
	if err != nil || len(hosts) != 1 || hosts[0].ID != "owner" || hosts[0].MeshIdentity != "pin" || hosts[0].Endpoint != "ws://127.0.0.1:7777/mesh" || len(hosts[0].Addresses) != 1 || hosts[0].Addresses[0] != "127.0.0.1" {
		t.Fatalf("canonical effective identity/pin/address control: %v", err)
	}
	for _, field := range []struct{ original, ambiguous string }{
		{`"id":"owner"`, `"id":"owner","ID":"other-owner"`},
		{`"meshIdentity":"pin"`, `"meshIdentity":"pin","MeshIdentity":"other-pin"`},
		{`"meshIdentity":"pin"`, `"meshIdentity":"pin","meſhIdentity":"other-pin"`},
		{`"endpoint":"ws://127.0.0.1:7777/mesh"`, `"endpoint":"ws://127.0.0.1:7777/mesh","Endpoint":"ws://127.0.0.1:8888/mesh"`},
		{`"version":1`, `"version":1,"Version":1`},
		{`"hosts":`, `"Hosts":[],"hosts":`},
		{`"theme":"oled"`, `"theme":"oled","Theme":"oled"`},
		{`"addresses":["127.0.0.1"]`, `"addresses":["127.0.0.1"],"Addresses":["127.0.0.2"]`},
	} {
		t.Run(field.original, func(t *testing.T) {
			before := bytes.Replace(canonical, []byte(field.original), []byte(field.ambiguous), 1)
			path := writeClientConfigFixture(t, before)
			control := bytes.Replace(before, []byte(`"alias":"obsolete",`), nil, 1)
			if _, err := parseHostConfig(control, path); err != nil {
				t.Fatalf("ordinary decoder control: %v", err)
			}
			err := retireClientAliases(context.Background())
			after, readErr := readClientConfigTestFile(path)
			if err == nil || readErr != nil || !bytes.Equal(before, after) {
				t.Fatalf("ambiguous fields must refuse unchanged: err=%v read=%v", err, readErr)
			}
		})
	}
}

func TestClientRetirementRejectsMalformedAliasTypes(t *testing.T) {
	for _, value := range []string{`"obsolete"`, `123`, `true`, `[]`, `{}`, `null`} {
		t.Run(value, func(t *testing.T) {
			before := []byte(fmt.Sprintf(`{"version":1,"hosts":[{"alias":%s,"id":"owner","meshIdentity":"pin","endpoint":"ws://127.0.0.1:7777/mesh"}]}`, value))
			path := writeClientConfigFixture(t, before)
			err := retireClientAliases(context.Background())
			after, readErr := readClientConfigTestFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if value == `"obsolete"` {
				if err != nil || bytes.Contains(after, []byte(`"alias"`)) {
					t.Fatalf("historical string control failed: %v", err)
				}
				return
			}
			if err == nil || !bytes.Equal(before, after) {
				t.Fatalf("malformed alias accepted or rewritten: %v", err)
			}
		})
	}
}

func TestClientRetirementReadOnlyCurrentBook(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires an unprivileged user to prove directory write refusal")
	}
	current := []byte(`{"version":1,"hosts":[]}`)
	path := writeClientConfigFixture(t, current)
	if _, _, err := executeCommand(t, Dependencies{}, "shell-init", "bash"); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(filepath.Dir(path), ".hosts.lock")
	if err := os.Remove(lock); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o500); err != nil { //nolint:gosec // owned directory needs execute permission for the read-only fixture
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(filepath.Dir(path), 0o700) }() //nolint:gosec // restore the owned fixture directory for t.TempDir cleanup
	if _, _, err := executeCommand(t, Dependencies{}, "shell-init", "bash"); err != nil {
		t.Fatalf("unchanged read-only command: %v", err)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatalf("read-only activation created a writer lock: %v", err)
	}
	if err := os.WriteFile(path, obsoleteConfigFixture(), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := executeCommand(t, Dependencies{}, "shell-init", "bash"); err == nil {
		t.Fatal("retirement requiring a directory write succeeded")
	}
	after, err := readClientConfigTestFile(path)
	if err != nil || !bytes.Equal(after, obsoleteConfigFixture()) {
		t.Fatal("failed read-only retirement changed bytes")
	}
}

func TestClientRetirementWaitsForUpdateCommit(t *testing.T) {
	for _, phase := range []updateinstall.Phase{updateinstall.Accepted, updateinstall.Staged, updateinstall.Granted, updateinstall.Activating, updateinstall.Validating, updateinstall.RollingBack, updateinstall.RolledBack, updateinstall.RollbackFailed, updateinstall.Cancelled, updateinstall.Failed, updateinstall.Committed} {
		t.Run(string(phase), func(t *testing.T) {
			before := obsoleteConfigFixture()
			path := writeClientConfigFixture(t, before)
			state := t.TempDir()
			t.Setenv("MESH_STATE_DIR", state)
			writeClientActivationJournal(t, state, updateinstall.Status{Schema: 1, Phase: phase, Settings: updateinstall.Settings{StateDir: state, ClientOnly: true}})
			if _, _, err := executeCommand(t, Dependencies{}, "shell-init", "bash"); err != nil {
				t.Fatal(err)
			}
			after, err := readClientConfigTestFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if phase == updateinstall.Committed {
				if bytes.Contains(after, []byte(`"alias"`)) {
					t.Fatal("committed activation retained obsolete state")
				}
				return
			}
			if !bytes.Equal(before, after) {
				t.Fatal("configuration changed before committed cutover")
			}
		})
	}
}

func TestClientMetadataNeverRetiresDuringValidation(t *testing.T) {
	path := writeClientConfigFixture(t, obsoleteConfigFixture())
	state := t.TempDir()
	t.Setenv("MESH_STATE_DIR", state)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	writeClientActivationJournal(t, state, updateinstall.Status{Schema: 1, Phase: updateinstall.Validating, Settings: updateinstall.Settings{StateDir: state, Executable: executable, ClientOnly: true}})
	if _, _, err := executeCommand(t, Dependencies{}, "version", "--json"); err != nil {
		t.Fatal(err)
	}
	after, err := readClientConfigTestFile(path)
	if err != nil || !bytes.Equal(after, obsoleteConfigFixture()) {
		t.Fatal("installed validation metadata deleted rollback state")
	}
}

func TestDashboardMalformedAliasRetainsError(t *testing.T) {
	before := []byte(`{"version":1,"hosts":[{"alias":123,"id":"owner","meshIdentity":"pin","endpoint":"ws://127.0.0.1:7777/mesh"}]}`)
	path := writeClientConfigFixture(t, before)
	if _, _, err := identity.LoadOrCreate(os.Getenv("MESH_STATE_DIR")); err != nil {
		t.Fatal(err)
	}
	views := 0
	_, _, err := executeCommand(t, Dependencies{Dashboard: func(_ context.Context, input DashboardInput) error {
		views++
		if input.ConfigError == nil || !strings.Contains(input.ConfigError.Error(), "JSON string") || input.ConfigWatch == nil || input.Watch != nil || len(input.Hosts) != 0 {
			t.Fatal("malformed alias did not retain an actionable error view")
		}
		return nil
	}}, "dashboard", "--wall")
	after, readErr := readClientConfigTestFile(path)
	if err != nil || readErr != nil || views != 1 || !bytes.Equal(before, after) {
		t.Fatalf("malformed TV configuration changed or exited as failure: views=%d err=%v read=%v", views, err, readErr)
	}
}
