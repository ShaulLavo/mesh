package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnrollmentRootPreservesConfiguration(t *testing.T) {
	for _, example := range []struct {
		name    string
		command string
		legacy  bool
		refused bool
		apply   bool
	}{
		{name: "checked-current-check", command: "approve-checked"},
		{name: "checked-legacy-check", command: "approve-checked", legacy: true},
		{name: "checked-current-refused-check", command: "approve-checked", refused: true},
		{name: "checked-legacy-refused-check", command: "approve-checked", legacy: true, refused: true},
		{name: "checked-current-refused", command: "approve-checked", refused: true, apply: true},
		{name: "checked-legacy-refused", command: "approve-checked", legacy: true, refused: true, apply: true},
		{name: "fleet-current-check", command: "approve-fleet"},
		{name: "fleet-legacy-check", command: "approve-fleet", legacy: true},
		{name: "fleet-current-refused-check", command: "approve-fleet", refused: true},
		{name: "fleet-legacy-refused-check", command: "approve-fleet", legacy: true, refused: true},
		{name: "fleet-current-refused", command: "approve-fleet", refused: true, apply: true},
		{name: "fleet-legacy-refused", command: "approve-fleet", legacy: true, refused: true, apply: true},
	} {
		t.Run(example.name, func(t *testing.T) {
			fixture := createFleetFixture(t, 1)
			request := fixture.Destinations[0]
			alias := ""
			if example.legacy {
				alias = `"alias":"obsolete",`
			}
			config := fmt.Sprintf(`{"version":1,"hosts":[{%s"id":%q,"meshIdentity":%q,"endpoint":"ws://127.0.0.1:1/mesh"}]}`, alias, fixture.Labels[0], request.Destination)
			requireApprovalMutation(t, os.WriteFile(filepath.Join(fixture.Config, "hosts.json"), []byte(config), 0600))
			mode := "--check"
			if example.apply {
				mode = "--yes"
			}
			if example.refused {
				request.Account += "-wrong-account"
			}
			args := []string{"device", "approve-checked", "--account", request.Account, "--state-dir", request.StateDir, "--destination", request.Destination, mode, "--allow-root", "--", fixture.Source.Destination}
			if example.command == "approve-fleet" {
				mapping := fmt.Sprintf(`{%q:{"target":"fixture-admin","account":%q,"stateDir":%q,"binary":%q}}`, fixture.Labels[0], request.Account, request.StateDir, filepath.Join(filepath.Dir(fixture.MapPath), "staged mesh'cli"))
				requireApprovalMutation(t, os.WriteFile(fixture.MapPath, []byte(mapping), 0600))
				args = []string{"device", "approve-fleet", "--admin-map", fixture.MapPath, mode, "--allow-root", "--", fixture.Labels[0]}
			}
			before := snapshotApprovalFiles(t, fixture.Config) + snapshotApprovalFiles(t, fixture.Source.StateDir) + snapshotApprovalFiles(t, request.StateDir)
			_, _, err := executeCommand(t, Dependencies{}, args...)
			wantError := ""
			if example.refused {
				wantError = "local account does not match the expected daemon account"
			}
			if example.command == "approve-fleet" && example.legacy {
				wantError = `unknown field "alias"`
			}
			if wantError == "" && err != nil || wantError != "" && (err == nil || !strings.Contains(err.Error(), wantError)) {
				t.Fatalf("root enrollment error = %v, want %q", err, wantError)
			}
			after := snapshotApprovalFiles(t, fixture.Config) + snapshotApprovalFiles(t, fixture.Source.StateDir) + snapshotApprovalFiles(t, request.StateDir)
			if before != after {
				t.Fatal("root enrollment changed configuration, files, directories, or daemon state")
			}
		})
	}
}
