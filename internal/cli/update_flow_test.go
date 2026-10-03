package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/creack/pty"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/updateinstall"
)

func updateFlowFixture(t *testing.T, current bool) (Dependencies, update.Host, *int) {
	t.Helper()
	_, local := setupUpdateCLI(t)
	for _, alias := range []string{"pc", "pi"} {
		host, _, err := identity.LoadOrCreate(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if err = SaveHost(HostRecord{Alias: alias, ID: alias, MeshIdentity: host.ID, Endpoint: "ws://" + alias + ".invalid/mesh"}); err != nil {
			t.Fatal(err)
		}
	}
	manifest := updateTestManifest()
	manifest.Version = "v0.1.160"
	manifest.Compatibility.Transitions = nil
	manifest.Compatibility.StateReadMin, manifest.Compatibility.StateReadMax, manifest.Compatibility.StateWrite = 10, 10, 10
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(manifest) }))
	t.Cleanup(server.Close)
	mutations := new(int)
	caller := updateCallFunc(func(_ context.Context, host update.Host, action string, _, output any) error {
		if action != "info" {
			*mutations++
			return errors.New("fixture forbids update mutations")
		}
		if host.ID != local.ID {
			return &update.RemoteError{Problem: "update administrator is not enrolled on this host; run mesh update trust <redacted-key> on this host"}
		}
		build := release.Build{Version: "v0.1.149", Digest: strings.Repeat("d", 64), Platform: release.Platform{OS: "darwin", Arch: "arm64"}, StateVersion: 10, WorkerProtocol: 1, UpdateProtocol: 1}
		if current {
			build.Version, build.Digest = manifest.Version, manifest.Artifacts[2].BinarySHA256
		}
		*output.(*update.Info) = update.Info{Health: updateinstall.Health{HostID: host.ID, Build: build, Workers: []updateinstall.Worker{{ID: "retained", Build: &release.Build{Version: "v0.1.149"}}}}}
		return nil
	})
	return Dependencies{UpdateRelease: release.Client{BaseURL: server.URL, HTTPClient: server.Client()}, UpdateCaller: caller}, local, mutations
}

func interactiveUpdateFlow(t *testing.T, dependencies Dependencies, answer string, args ...string) (string, error) {
	t.Helper()
	master, terminal, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = master.Close(); _ = terminal.Close() })
	dependencies.Stdin, dependencies.Stdout = terminal, terminal
	var output bytes.Buffer
	command := NewCommand(dependencies)
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs(args)
	if _, err = master.Write([]byte(answer)); err != nil {
		t.Fatal(err)
	}
	err = command.ExecuteContext(context.Background())
	return output.String(), err
}

func TestUpdateFlowFixtureCapture(t *testing.T) {
	dependencies, _, mutations := updateFlowFixture(t, false)
	text, err := interactiveUpdateFlow(t, dependencies, "n\n", "update")
	t.Logf("FIXTURE Linux terminal, modeled Mac daemon149 / release160; remote version remains unknown because authorization refuses metadata.\n%s\nCommand result: %v", text, err)
	if *mutations != 0 {
		t.Fatal("fixture attempted mutation")
	}
}

func TestUpdateFlowDefaultDoesNotDefineFleet(t *testing.T) {
	dependencies, _, mutations := updateFlowFixture(t, false)
	text, _ := interactiveUpdateFlow(t, dependencies, "n\n", "update")
	for _, unwanted := range []string{"complete intended fleet", "Fleet default", "commit ", "Release digest:", "trust ", "pc", "pi"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("default local preview contains %q", unwanted)
		}
	}
	if !strings.Contains(text, "This machine only.") || !strings.Contains(text, "on this machine before retrying") {
		t.Errorf("local next action is unclear: %s", text)
	}
	if !strings.Contains(text, "intermediate release") || strings.Contains(text, "[y/N]") {
		t.Errorf("unsupported jump offered approval: %s", text)
	}
	config, _ := ConfigPath()
	if _, err := os.Stat(filepath.Join(filepath.Dir(config), "fleet.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("default created fleet: %v", err)
	}
	if *mutations != 0 {
		t.Fatal("unsupported jump submitted a plan")
	}
}

func TestUpdateFlowRemoteAuthorizationIsNotFailure(t *testing.T) {
	dependencies, _, _ := updateFlowFixture(t, false)
	text, _, err := executeCommand(t, dependencies, "update", "--host", "local", "--host", "pc", "--host", "pi", "--check")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"not managed here", "version unknown", "Run mesh update --local", "on pc", "on pi"} {
		if !strings.Contains(text, want) {
			t.Errorf("preview lacks %q: %s", want, text)
		}
	}
	if strings.Contains(text, "failed") || strings.Contains(text, "trust ") {
		t.Errorf("authorization misreported or key instructions exposed: %s", text)
	}
}

func TestUpdateFlowKnownCurrentControl(t *testing.T) {
	dependencies, _, mutations := updateFlowFixture(t, true)
	text, _, err := executeCommand(t, dependencies, "update", "--local", "--check")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "up to date") {
		t.Fatalf("known current not observable: %s", text)
	}
	if *mutations != 0 {
		t.Fatal("check attempted mutation")
	}
}

func TestUpdateFlowExplicitFleetRetainsExactScope(t *testing.T) {
	dependencies, local, _ := updateFlowFixture(t, true)
	fleet := scopedUpdateFleet("chosen", []update.Host{local})
	file := filepath.Join(t.TempDir(), "fleet.json")
	saveUpdateTestFleet(t, file, fleet)
	before, err := os.ReadFile(file) //nolint:gosec // file belongs to this test's temporary directory
	if err != nil {
		t.Fatal(err)
	}
	text, _, err := executeCommand(t, dependencies, "update", "--fleet", file, "--check", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var preview updatePreview
	if err := json.Unmarshal([]byte(text), &preview); err != nil {
		t.Fatal(err)
	}
	if preview.Fleet.Digest() != fleet.Digest() || len(preview.Targets) != 1 {
		t.Fatal("explicit scope changed")
	}
	after, err := os.ReadFile(file) //nolint:gosec // file belongs to this test's temporary directory
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("check changed explicit fleet")
	}
}

func TestUpdateFlowUnsupportedApprovalNeverCreatesOperation(t *testing.T) {
	dependencies, _, mutations := updateFlowFixture(t, false)
	stateDir := os.Getenv("MESH_STATE_DIR")
	_, _, err := executeCommand(t, dependencies, "update", "--local", "--yes")
	if err == nil {
		t.Fatal("unsupported jump approved")
	}
	if *mutations != 0 {
		t.Fatal("unsupported jump submitted a plan")
	}
	//nolint:gosec // stateDir is set to a temporary directory by this fixture
	if _, err := os.Stat(filepath.Join(stateDir, "updates", "runs")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("blocked approval created operation: %v", err)
	}
}

func TestUpdateFlowRollbackFailureBlocksFreshApproval(t *testing.T) {
	dependencies, _, mutations := updateFlowFixture(t, true)
	caller := dependencies.UpdateCaller
	dependencies.UpdateCaller = updateCallFunc(func(ctx context.Context, host update.Host, action string, input, output any) error {
		if err := caller.Call(ctx, host, action, input, output); err != nil {
			return fmt.Errorf("fixture inspection: %w", err)
		}
		output.(*update.Info).Installation = &updateinstall.Status{Phase: updateinstall.RollbackFailed}
		return nil
	})
	text, _, err := executeCommand(t, dependencies, "update", "--local", "--yes")
	if err == nil || *mutations != 0 {
		t.Fatal("unrecovered installation accepted approval")
	}
	if !strings.Contains(text, "recovery") && !strings.Contains(err.Error(), "blocked") {
		t.Fatal("missing recovery guidance")
	}
}

func TestUpdateFlowCurrentApprovalStillReturnsJSON(t *testing.T) {
	dependencies, _, mutations := updateFlowFixture(t, true)
	text, _, err := executeCommand(t, dependencies, "update", "--local", "--yes", "--json")
	var preview updatePreview
	if err != nil || json.Unmarshal([]byte(text), &preview) != nil || *mutations != 0 {
		t.Fatalf("current approval output = %q, %v", text, err)
	}
}

func TestUpdateFlowDefaultApprovalOnlySubmitsLocalPlan(t *testing.T) {
	dependencies, local, _ := updateFlowFixture(t, false)
	dependencies.UpdateRelease, _ = updateTestRelease(t)
	plans := 0
	dependencies.UpdateCaller = updateCallFunc(func(_ context.Context, host update.Host, action string, input, output any) error {
		if host.ID != local.ID {
			return errors.New("default contacted another machine")
		}
		if action == "info" {
			*output.(*update.Info) = update.Info{Health: updateinstall.Health{HostID: host.ID, Build: updateTestBuild()}}
			return nil
		}
		if action != "plan" {
			t.Fatalf("unexpected action %s", action)
		}
		plans++
		plan := input.(update.Plan)
		if len(plan.Fleet.Members) != 1 || plan.Fleet.Members[0].ID != local.ID {
			t.Fatal("approval included another machine")
		}
		*output.(*update.Run) = update.Run{Release: plan.Manifest, Targets: []update.Target{{Host: local, State: update.Updated}}}
		return nil
	})
	text, err := interactiveUpdateFlow(t, dependencies, "y\n", "update")
	if err != nil || plans != 1 || !strings.Contains(text, "Update Mesh on this machine?") {
		t.Fatalf("local approval = %s, %v, plans %d", text, err, plans)
	}
	config, _ := ConfigPath()
	if _, err := os.Stat(filepath.Join(filepath.Dir(config), "fleet.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("local approval saved fleet: %v", err)
	}
}

func TestUpdateFlowSavedFleetAndAllKeepExplicitMembership(t *testing.T) {
	dependencies, local, _ := updateFlowFixture(t, true)
	hosts, err := LoadHosts()
	if err != nil {
		t.Fatal(err)
	}
	fleet := scopedUpdateFleet("chosen", []update.Host{local, adoptedUpdateHost(hosts[0])})
	config, _ := ConfigPath()
	saveUpdateTestFleet(t, filepath.Join(filepath.Dir(config), "fleet.json"), fleet)
	for _, args := range [][]string{{"update", "--check", "--json"}, {"update", "--all", "--check", "--json"}} {
		text, _, err := executeCommand(t, dependencies, args...)
		var preview updatePreview
		if err != nil || json.Unmarshal([]byte(text), &preview) != nil || preview.Fleet.Digest() != fleet.Digest() {
			t.Fatalf("saved scope = %q, %v", text, err)
		}
	}
}

func TestUpdateFlowAuthenticatedAccessRemainsUnknown(t *testing.T) {
	for _, test := range []struct{ problem, label string }{
		{"authenticate update endpoint: transport: Mesh peer authentication failed: remote error: tls: bad certificate", "access unverified"},
		{"transport: device key is not approved", "not managed here"},
		{"authenticate update endpoint: transport: Mesh peer authentication failed: transport: destination Mesh identity changed", "failed"},
	} {
		t.Run(test.label, func(t *testing.T) {
			dependencies, _, mutations := updateFlowFixture(t, true)
			dependencies.UpdateCaller = updateCallFunc(func(context.Context, update.Host, string, any, any) error { return errors.New(test.problem) })
			text, _, err := executeCommand(t, dependencies, "update", "--host", "pc", "--check")
			if err != nil || !strings.Contains(text, test.label) || !strings.Contains(text, "version unknown") || *mutations != 0 {
				t.Fatalf("access preview = %q, %v", text, err)
			}
		})
	}
}

func TestUpdateFlowGenuineFailureRemainsFailure(t *testing.T) {
	var output bytes.Buffer
	target := update.Target{Host: update.Host{Alias: "pc"}, State: update.Failed, Problem: "invalid update signature"}
	if err := printUpdateTargets(&output, []update.Target{target}, updateTestManifest()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "failed") || strings.Contains(output.String(), "not managed here") {
		t.Fatal(output.String())
	}
}
