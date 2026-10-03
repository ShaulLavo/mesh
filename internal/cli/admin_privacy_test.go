package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/agentresume"
	"github.com/shaul/mesh/internal/privacy"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/update"
	"github.com/spf13/cobra"
)

func TestPrivateGCPlanPreservesAuthoritativeEntries(t *testing.T) {
	mask := privacy.New()
	row := gcRow("7K3D", "detached", 8*time.Hour, 8*time.Hour, false)
	row.Command = []string{"/home/owner/bin/bash", "-c", "echo secret-argument"}
	row.Label = "dev-route owner@machine"
	row.Recovery.Title = "Debug frontend /home/owner/private-project"
	entries := []gcEntry{{host: HostRecord{Alias: "build-box owner@machine"}, row: row, idle: 8 * time.Hour, action: gcLeave, notes: []string{row.Label, "plain shell"}}}
	before, err := json.Marshal(entries[0].row)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := writeGCPlan(&output, entries, mask); err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"owner@machine", "/home/owner", "secret-argument"} {
		if strings.Contains(output.String(), private) {
			t.Fatalf("leaked %q: %s", private, &output)
		}
	}
	for _, public := range []string{"HOST", "MEM", "left running", "plain shell", "bash", "[arguments withheld]", "build-box", "7K3D", "dev-route", "Debug frontend"} {
		if !strings.Contains(output.String(), public) {
			t.Fatalf("missing %q: %s", public, &output)
		}
	}
	after, err := json.Marshal(entries[0].row)
	if err != nil || !bytes.Equal(before, after) || entries[0].notes[0] != row.Label {
		t.Fatal("GC presentation changed authoritative values")
	}
	output.Reset()
	if err := writeGCPlan(&output, entries); err != nil || !strings.Contains(output.String(), "secret-argument") {
		t.Fatalf("default GC display changed: %s, %v", &output, err)
	}
}

func TestPrivateRenameUsesRealAliasesForControl(t *testing.T) {
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	t.Setenv("MESH_PRIVACY", "false")
	id := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	if err := SaveHost(HostRecord{Alias: "private-old", ID: id, MeshIdentity: id, Endpoint: "ws://127.0.0.1:7337/mesh"}); err != nil {
		t.Fatal(err)
	}
	command := NewCommand(Dependencies{})
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs([]string{"--privacy", "rename", "private-old", "private-new"})
	if err := command.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if output.String() != "renamed private-old to private-new\n" {
		t.Fatalf("rename display = %s", &output)
	}
	hosts, err := LoadHosts()
	if err != nil || len(hosts) != 1 || hosts[0].Alias != "private-new" || hosts[0].ID != id {
		t.Fatalf("rename control values = %#v, %v", hosts, err)
	}
}

func TestPrivateAgentBindStatusDoesNotChangeRecipe(t *testing.T) {
	mask := privacy.New()
	recipe := agentresume.Recipe{ConversationID: "12345678-1234-1234-1234-123456789abc", Directory: "/home/owner/private-project"}
	original := recipe
	var output bytes.Buffer
	if err := writeAgentBindStatus(&output, recipe, "7K3D", mask); err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{recipe.ConversationID, "/home/owner"} {
		if strings.Contains(output.String(), private) {
			t.Fatalf("bind leaked %q: %s", private, &output)
		}
	}
	if !reflect.DeepEqual(recipe, original) || (!strings.Contains(output.String(), "mesh recover 7K3D --agent") || !strings.Contains(output.String(), "~/private-project")) {
		t.Fatalf("bind recipe or guidance changed: %#v / %s", recipe, &output)
	}
}

func TestPrivateNativeAgentStreamsRemainRaw(t *testing.T) {
	command := &cobra.Command{}
	command.SetIn(strings.NewReader(""))
	var output, diagnostics bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&diagnostics)
	if err := runNativeAgent(command, "/bin/sh", []string{"-c", "printf '%s' \"$1\"; printf '%s' \"$2\" >&2", "provider", "/home/private/stdout", "private-stderr@example.com"}, "", os.Environ()); err != nil {
		t.Fatal(err)
	}
	if output.String() != "/home/private/stdout" || diagnostics.String() != "private-stderr@example.com" {
		t.Fatalf("program streams filtered: %q / %q", &output, &diagnostics)
	}
}

func TestPrivateAgentSetupPreservesGeneratedHooks(t *testing.T) {
	mask := privacy.New()
	command := &cobra.Command{}
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	if err := setupAgentHooks(command, "claude", "", false, false, mask); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fragment, err := agentresume.StableHookFragment(agentresume.Claude, executable)
	if err != nil {
		t.Fatal(err)
	}
	if output.String() != string(fragment)+"\n" {
		t.Fatal("privacy changed machine-consumed hook fragment")
	}
	path := filepath.Join(t.TempDir(), "private-settings.json")
	output.Reset()
	if err := setupAgentHooks(command, "claude", path, true, false, mask); err != nil {
		t.Fatal(err)
	}
	if output.String() != "Updated "+path+"\n" {
		t.Fatalf("setup display = %s", &output)
	}
	installed, err := os.ReadFile(path) //nolint:gosec // read the hook settings fixture generated in this test's temporary directory
	if err != nil || !bytes.Contains(installed, []byte(executable)) {
		t.Fatalf("installed hook lost operational executable: %s, %v", installed, err)
	}
}

func TestPrivateAgentTrustPreservesOperationalGuidance(t *testing.T) {
	mask := privacy.New()
	guidance := "not approved yet; " + codexHookReview
	if got := privateAgentTrust(mask, guidance); got != guidance {
		t.Fatalf("doctor changed provider command guidance: %s", got)
	}
	failure := "unknown (read /home/owner/private/config.toml: arbitrary-private-error)"
	if got := privateAgentTrust(mask, failure); strings.Contains(got, "/home/owner") || strings.Contains(got, "arbitrary-private-error") || !strings.HasPrefix(got, "unknown (error-") {
		t.Fatalf("doctor trust error = %s", got)
	}
	if got := privateAgentTrust(nil, failure); got != failure {
		t.Fatal("default trust error changed")
	}
}

func TestPrivateAgentDoctorMasksSettingsPathAndErrors(t *testing.T) {
	root := t.TempDir()
	provider := filepath.Join(root, "codex")
	if err := os.WriteFile(provider, []byte("#!/bin/sh\nprintf 'codex-cli 0.114.0\\n'\n"), 0o700); err != nil { //nolint:gosec // private executable fixture
		t.Fatal(err)
	}
	t.Setenv("PATH", root)
	t.Setenv("CODEX_HOME", "/home/owner/private-provider-home")
	mask := privacy.New()
	command := &cobra.Command{}
	command.SetContext(t.Context())
	var output bytes.Buffer
	command.SetOut(&output)
	if err := diagnoseAgentRecovery(command, "codex", mask); err != nil {
		t.Fatal(err)
	}
	path, err := agentSettingsPath(agentresume.Codex)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "/home/owner") || !strings.Contains(output.String(), mask.Value("path", path)) || !strings.Contains(output.String(), "Hooks: missing") {
		t.Fatalf("doctor display = %s", &output)
	}
	if err := os.Remove(provider); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := diagnoseAgentRecovery(command, "codex", mask); err != nil || !strings.Contains(output.String(), "executable missing (error-") {
		t.Fatalf("doctor missing display = %s, %v", &output, err)
	}
}

func TestPrivateUpdateHumanDisplayLeavesMachineJSONRaw(t *testing.T) {
	mask := privacy.New()
	run := update.Run{ID: "12345678-1234-1234-1234-123456789abc", Release: release.Manifest{Version: "v0.1.52"}, Problem: "private-freeform-problem", Targets: []update.Target{{Host: update.Host{Alias: "build-box owner@machine"}, State: update.Updated, Problem: "private-target-problem"}}}
	preview := updatePreview{Fleet: update.Fleet{Name: "development"}, Release: run.Release, Targets: run.Targets, OutsideFleet: []string{"offline-box", "100.64.0.9"}}
	var output bytes.Buffer
	if err := printUpdatePreview(&output, preview, false, mask); err != nil {
		t.Fatal(err)
	}
	if err := printUpdateRun(&output, run, false, mask); err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{run.ID, run.Problem, "owner@machine", "private-target-problem", "100.64.0.9"} {
		if strings.Contains(output.String(), private) {
			t.Fatalf("update leaked %q: %s", private, &output)
		}
	}
	for _, public := range []string{"v0.1.52", "updated", "1 of 1 machines verified", "build-box", "development", "offline-box"} {
		if !strings.Contains(output.String(), public) {
			t.Fatalf("update lost %q: %s", public, &output)
		}
	}
	output.Reset()
	if err := printUpdateRun(&output, run, true, mask); err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(run)
	if err != nil || output.String() != string(want)+"\n" {
		t.Fatal("privacy changed machine-consumed update JSON")
	}
	output.Reset()
	if err := printUpdatePreview(&output, preview, true, mask); err != nil {
		t.Fatal(err)
	}
	want, err = json.Marshal(preview)
	if err != nil || output.String() != string(want)+"\n" {
		t.Fatal("privacy changed machine-consumed preview JSON")
	}
}

func TestPrivateAgentFallbackPromptRemainsActionable(t *testing.T) {
	mask := privacy.New()
	failure := errors.New("arbitrary-private-provider-error")
	directory := "/home/owner/private-project"
	var output bytes.Buffer
	writeAgentRecoveryShellPrompt(&output, failure, directory, mask)
	if strings.Contains(output.String(), failure.Error()) || strings.Contains(output.String(), directory) {
		t.Fatalf("fallback leaked metadata: %s", &output)
	}
	for _, public := range []string{"Provider could not resume: error-", "Press Enter", "Ctrl+D", "~/private-project"} {
		if !strings.Contains(output.String(), public) {
			t.Fatalf("fallback lost guidance %q: %s", public, &output)
		}
	}
	output.Reset()
	writeAgentRecoveryShellPrompt(&output, failure, directory, nil)
	if !strings.Contains(output.String(), failure.Error()) || !strings.Contains(output.String(), directory) {
		t.Fatal("default fallback prompt changed")
	}
}
