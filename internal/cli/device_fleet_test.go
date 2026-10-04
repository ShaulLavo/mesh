package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/identity"
)

func TestApproveFleetRequiresExplicitSelectionAndConsent(t *testing.T) {
	command := deviceCommand()
	fleet, _, err := command.Find([]string{"approve-fleet"})
	if err != nil || fleet == command {
		t.Fatal("explicit pinned-fleet enrollment is unavailable")
	}
	for _, flag := range []string{"admin-map", "check", "yes", "allow-root"} {
		if fleet.Flags().Lookup(flag) == nil {
			t.Fatalf("missing enrollment control %s", flag)
		}
	}
	for _, args := range [][]string{{"approve-fleet"}, {"approve-fleet", "fixture"}, {"approve-fleet", "--check", "--yes", "fixture"}} {
		command = deviceCommand()
		command.SetArgs(args)
		command.SilenceUsage, command.SilenceErrors = true, true
		if err := command.ExecuteContext(t.Context()); err == nil {
			t.Fatalf("enrollment accepted missing selection or ambiguous consent: %v", args)
		}
	}
}

type fleetFixture struct {
	Source       checkedApproval
	Destinations []checkedApproval
	Labels       []string
	MapPath      string
	LogPath      string
	Config       string
}

func createFleetFixture(t *testing.T, count int) fleetFixture {
	t.Helper()
	source := approvalFixtureRequest(t)
	t.Setenv("MESH_STATE_DIR", source.StateDir)
	config := t.TempDir()
	t.Setenv("MESH_CONFIG_DIR", config)
	tools := t.TempDir()
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	binary := filepath.Join(tools, "staged mesh'cli")
	logPath := filepath.Join(tools, "ssh.log")
	t.Setenv("MESH_175_SSH_LOG", logPath)
	t.Setenv("MESH_175_TEST_BINARY", os.Args[0])
	requireApprovalMutation(t, os.WriteFile(binary, []byte("#!/bin/sh\nMESH_175_FIXTURE_ROLE=cli exec \"$MESH_175_TEST_BINARY\" -test.run=^TestApprovalFixtureProcess$ -- \"$@\"\n"), 0700)) //nolint:gosec // executable fixture scripts in this test's private tools directory
	//nolint:gosec // executable SSH fixture belongs to this test's private tools directory
	requireApprovalMutation(t, os.WriteFile(filepath.Join(tools, "ssh"), []byte(`#!/bin/sh
[ "$1" = '-o' ] && [ "$2" = 'BatchMode=yes' ] && [ "$3" = '-o' ] && [ "$4" = 'StrictHostKeyChecking=yes' ] && [ "$5" = '-o' ] && [ "$6" = 'ConnectTimeout=5' ] && [ "$7" = '--' ] || exit 71
printf '%s\n' "$8 $9" >> "$MESH_175_SSH_LOG"
[ "$8" != "untrusted-admin" ] || { echo 'Host key verification failed.' >&2; exit 255; }
case "$9" in
  *"'--yes'"*)
    if [ "$8" = "$MESH_175_FAIL_AFTER_APPLY" ]; then
      /bin/sh -c "$9" >/dev/null || exit 73
      echo "fixture lost approval receipt" >&2
      exit 74
    fi
    [ "$8" != "$MESH_175_FAIL_APPLY" ] || { echo 'fixture apply failure' >&2; exit 72; }
    [ "$8" != "$MESH_175_REPLACE_PIN" ] || printf '{"version":1,"hosts":[]}\n' > "$MESH_CONFIG_DIR/hosts.json"
    ;;
esac
exec /bin/sh -c "$9"
`), 0700)) //nolint:gosec // executable fixture scripts in this test's private tools directory
	result := fleetFixture{Source: source, MapPath: filepath.Join(tools, "admin.json"), LogPath: logPath, Config: config}
	administration := make(map[string]fleetAdmin)
	for index := 0; index < count; index++ {
		request := approvalFixtureRequest(t)
		startApprovalFixture(t, request.StateDir, request.StateDir, "")
		label := "destination-" + strings.Repeat("x", index+1)
		requireApprovalMutation(t, SaveHost(HostRecord{ID: label, MeshIdentity: request.Destination, Endpoint: "ws://127.0.0.1:1/control/ws"}))
		administration[label] = fleetAdmin{Target: label + "-admin", Account: request.Account, StateDir: request.StateDir, Binary: binary}
		result.Labels = append(result.Labels, label)
		result.Destinations = append(result.Destinations, request)
	}
	contents, err := json.Marshal(administration)
	if err != nil {
		t.Fatal(err)
	}
	requireApprovalMutation(t, os.WriteFile(result.MapPath, contents, 0600))
	return result
}

func executeFleetFixture(t *testing.T, fixture fleetFixture, mode string) (string, error) {
	t.Helper()
	args := []string{"approve-fleet", "--admin-map", fixture.MapPath, mode, "--allow-root"}
	args = append(args, fixture.Labels...)
	var output bytes.Buffer
	command := deviceCommand()
	command.SetArgs(args)
	command.SetOut(&output)
	command.SetErr(&output)
	command.SilenceUsage, command.SilenceErrors = true, true
	err := command.ExecuteContext(t.Context())
	return output.String(), err
}

func TestApproveFleetOneWayPinsAndReadOnlyPreview(t *testing.T) {
	fixture := createFleetFixture(t, 2)
	sourceBefore := snapshotApprovalFiles(t, fixture.Source.StateDir)
	configBefore := snapshotApprovalFiles(t, fixture.Config)
	before := sourceBefore + configBefore
	for _, destination := range fixture.Destinations {
		before += snapshotApprovalFiles(t, destination.StateDir)
	}
	output, err := executeFleetFixture(t, fixture, "--check")
	if err != nil || strings.Count(output, ": approval available") != 2 {
		t.Fatalf("fleet preview: %s %v", output, err)
	}
	after := snapshotApprovalFiles(t, fixture.Source.StateDir) + snapshotApprovalFiles(t, fixture.Config)
	for _, destination := range fixture.Destinations {
		after += snapshotApprovalFiles(t, destination.StateDir)
	}
	if before != after {
		t.Fatal("fleet preview changed identity, config or grants")
	}
	output, err = executeFleetFixture(t, fixture, "--yes")
	if err != nil || strings.Count(output, ": approved") != 2 {
		t.Fatalf("fleet apply: %s %v", output, err)
	}
	for _, destination := range fixture.Destinations {
		if !identity.GrantedIdentity(destination.StateDir, fixture.Source.Destination) {
			t.Fatal("outgoing source did not receive destination's grant")
		}
		if identity.GrantedIdentity(fixture.Source.StateDir, destination.Destination) {
			t.Fatal("fleet command approved an incoming pin on the source")
		}
	}
	if snapshotApprovalFiles(t, fixture.Source.StateDir) != sourceBefore || snapshotApprovalFiles(t, fixture.Config) != configBefore {
		t.Fatal("fleet command changed source state or configuration")
	}
	log, err := os.ReadFile(fixture.LogPath)
	if err != nil || strings.Count(string(log), "'--check'") != 4 || strings.Count(string(log), "'--yes'") != 2 {
		t.Fatalf("bounded explicit SSH preflight/apply sequence: %s %v", log, err)
	}
}

func TestApproveFleetStopsWithPartialCompletion(t *testing.T) {
	fixture := createFleetFixture(t, 3)
	t.Setenv("MESH_175_FAIL_APPLY", fixture.Labels[1]+"-admin")
	output, err := executeFleetFixture(t, fixture, "--yes")
	if err == nil || !strings.Contains(output, "1 of 3 destinations approved") || !strings.Contains(output, fixture.Labels[2]+": pending") {
		t.Fatalf("partial completion missing: %s %v", output, err)
	}
	for index, destination := range fixture.Destinations {
		if identity.GrantedIdentity(destination.StateDir, fixture.Source.Destination) != (index == 0) {
			t.Fatal("failure wrote an unapplied destination or rolled back an approved one")
		}
	}
	log, err := os.ReadFile(fixture.LogPath)
	if err != nil || strings.Count(string(log), "'--yes'") != 2 {
		t.Fatalf("fleet continued applying after failure: %s %v", log, err)
	}
}

func TestApproveFleetRefusesUnknownHostKeyBeforeMutation(t *testing.T) {
	fixture := createFleetFixture(t, 2)
	contents, err := os.ReadFile(fixture.MapPath)
	if err != nil {
		t.Fatal(err)
	}
	contents = bytes.ReplaceAll(contents, []byte(fixture.Labels[1]+"-admin"), []byte("untrusted-admin"))
	requireApprovalMutation(t, os.WriteFile(fixture.MapPath, contents, 0600)) //nolint:gosec // fixed administration fixture path
	output, err := executeFleetFixture(t, fixture, "--yes")
	if err == nil || !strings.Contains(err.Error(), "Host key verification failed") || !strings.Contains(output, "0 of 2 destinations approved") {
		t.Fatalf("untrusted system SSH destination accepted: %s %v", output, err)
	}
	for _, destination := range fixture.Destinations {
		if identity.GrantedIdentity(destination.StateDir, fixture.Source.Destination) {
			t.Fatal("preflight failure left a destination grant")
		}
	}
}

func TestApproveFleetRevalidatesChangedSelection(t *testing.T) {
	fixture := createFleetFixture(t, 2)
	t.Setenv("MESH_175_REPLACE_PIN", fixture.Labels[0]+"-admin")
	output, err := executeFleetFixture(t, fixture, "--yes")
	if err == nil || !strings.Contains(output, "1 of 2 destinations approved") || !strings.Contains(err.Error(), "saved pin changed") {
		t.Fatalf("changed configuration was trusted at next apply boundary: %s %v", output, err)
	}
	if identity.GrantedIdentity(fixture.Destinations[1].StateDir, fixture.Source.Destination) {
		t.Fatal("changed source selection authorized another apply")
	}
}

func TestApproveFleetMissingInputsCreateNothing(t *testing.T) {
	fixture := createFleetFixture(t, 1)
	for _, absent := range []string{"state", "key", "config"} {
		t.Run(absent, func(t *testing.T) {
			sourceState := fixture.Source.StateDir
			configState := fixture.Config
			switch absent {
			case "state":
				sourceState = filepath.Join(t.TempDir(), "missing")
				t.Setenv("MESH_STATE_DIR", sourceState)
			case "key":
				sourceState = t.TempDir()
				t.Setenv("MESH_STATE_DIR", sourceState)
			case "config":
				configState = filepath.Join(t.TempDir(), "missing")
				t.Setenv("MESH_CONFIG_DIR", configState)
			}
			before := snapshotApprovalFiles(t, sourceState) + snapshotApprovalFiles(t, configState)
			if _, err := executeFleetFixture(t, fixture, "--check"); err == nil {
				t.Fatal("missing existing source/configuration accepted")
			}
			if before != snapshotApprovalFiles(t, sourceState)+snapshotApprovalFiles(t, configState) {
				t.Fatal("enrollment created missing source/configuration")
			}
			if _, err := os.Stat(fixture.LogPath); !os.IsNotExist(err) {
				t.Fatal("invalid source/configuration reached system SSH")
			}
		})
	}
}

func TestApproveFleetMapAndReceiptBoundaries(t *testing.T) {
	for _, admin := range []fleetAdmin{{}, {Target: "-oProxyCommand=bad", Account: "owner", StateDir: "/state", Binary: "/mesh"}, {Target: "explicit", Account: "owner", StateDir: "relative", Binary: "/mesh"}} {
		if err := validateFleetAdmin(admin); err == nil {
			t.Fatal("implicit or unsafe administration accepted")
		}
	}
	for _, contents := range []string{`{"extra":true}`, `{} {}`, strings.Repeat(" ", fleetInputLimit+1)} {
		var receipt approvalReceipt
		if err := decodeFleetJSON(strings.NewReader(contents), &receipt); err == nil {
			t.Fatal("invalid receipt accepted")
		}
	}
	fixture := createFleetFixture(t, 1)
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if account.Username == "root" {
		command := deviceCommand()
		command.SetArgs([]string{"approve-fleet", "--admin-map", fixture.MapPath, "--yes", fixture.Labels[0]})
		if err := command.ExecuteContext(t.Context()); err == nil {
			t.Fatal("root destination approval did not require acknowledgement")
		}
	}
	if _, err := selectedFleet([]string{fixture.Labels[0], fixture.Labels[0]}, fixture.MapPath); err == nil {
		t.Fatal("duplicate configured destination accepted")
	}
	if _, err := selectedFleet([]string{"unconfigured"}, fixture.MapPath); err == nil {
		t.Fatal("administration map inferred an unconfigured host")
	}
}

func TestApproveFleetMissingStagedBinaryRefusesBeforeMutation(t *testing.T) {
	fixture := createFleetFixture(t, 1)
	contents, err := os.ReadFile(fixture.MapPath)
	if err != nil {
		t.Fatal(err)
	}
	var administration map[string]fleetAdmin
	if err := json.Unmarshal(contents, &administration); err != nil {
		t.Fatal(err)
	}
	admin := administration[fixture.Labels[0]]
	admin.Binary += ".absent"
	administration[fixture.Labels[0]] = admin
	contents, err = json.Marshal(administration)
	if err != nil {
		t.Fatal(err)
	}
	requireApprovalMutation(t, os.WriteFile(fixture.MapPath, contents, 0600)) //nolint:gosec // fixed missing-binary administration fixture
	before := snapshotApprovalFiles(t, fixture.Destinations[0].StateDir)
	if _, err := executeFleetFixture(t, fixture, "--yes"); err == nil {
		t.Fatal("unavailable staged binary permitted approval")
	}
	if before != snapshotApprovalFiles(t, fixture.Destinations[0].StateDir) {
		t.Fatal("unavailable binary wrote destination state")
	}
}

func TestApproveFleetLostReceiptReportsUnknownOutcome(t *testing.T) {
	fixture := createFleetFixture(t, 2)
	t.Setenv("MESH_175_FAIL_AFTER_APPLY", fixture.Labels[0]+"-admin")
	output, err := executeFleetFixture(t, fixture, "--yes")
	if err == nil || !strings.Contains(output, fixture.Labels[0]+": approval outcome unknown") {
		t.Fatalf("lost approval receipt did not report an unknown grant outcome: %s %v", output, err)
	}
	if !identity.GrantedIdentity(fixture.Destinations[0].StateDir, fixture.Source.Destination) {
		t.Fatal("lost receipt fixture did not apply its grant")
	}
	if identity.GrantedIdentity(fixture.Destinations[1].StateDir, fixture.Source.Destination) {
		t.Fatal("fleet continued after an unknown apply outcome")
	}
	output, err = executeFleetFixture(t, fixture, "--check")
	if err != nil || !strings.Contains(output, fixture.Labels[0]+": already approved") {
		t.Fatalf("read-only retry could not resolve unknown outcome: %s %v", output, err)
	}
}
