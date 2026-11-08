package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/update"
)

func TestAutomaticCLIRequiresExistingIdentityAndNeverCreatesIt(t *testing.T) {
	state := filepath.Join(t.TempDir(), "absent")
	t.Setenv("MESH_STATE_DIR", state)
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	_, _, err := executeCommand(t, Dependencies{}, "update", "automatic", "status", "--json")
	if err == nil {
		t.Fatal("automatic status created an identity")
	}
	if _, err := os.Stat(state); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("status created state directory: %v", err)
	}
}
func TestAutomaticCLIEnablesOnlyDisplayedSavedMembership(t *testing.T) {
	_, host := setupUpdateCLI(t)
	fleet := update.Fleet{Version: 1, Name: "saved", Revision: 7, Members: []update.Host{host}}
	var received update.AutomaticCommand
	caller := updateCallFunc(func(_ context.Context, _ update.Host, action string, input, output any) error {
		if action == "automatic.change" {
			received = input.(update.AutomaticCommand)
		}
		status := update.AutomaticStatus{State: "not enrolled", SavedFleet: &fleet, SavedMembership: fleet.Digest(), PendingEpoch: 1}
		data, err := json.Marshal(status)
		if err != nil {
			return fmt.Errorf("encode automatic CLI fixture: %w", err)
		}
		if err := json.Unmarshal(data, output); err != nil {
			return fmt.Errorf("decode automatic CLI fixture: %w", err)
		}
		return nil
	})
	_, diagnostic, err := executeCommand(t, Dependencies{UpdateCaller: caller}, "update", "automatic", "enable", "--expected-revision", "0", "--yes", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if received.Action != "enable" || received.ExpectedMembership != fleet.Digest() || received.ID == "" || !strings.Contains(diagnostic, fleet.Digest()) {
		t.Fatalf("approval not bound to displayed saved membership: %+v %s", received, diagnostic)
	}
}
func TestAutomaticCLIStatusKeepsUnavailableRevisionUnknown(t *testing.T) {
	setupUpdateCLI(t)
	caller := updateCallFunc(func(context.Context, update.Host, string, any, any) error { return errors.New("coordinator offline") })
	_, _, err := executeCommand(t, Dependencies{UpdateCaller: caller}, "update", "automatic", "disable", "--expected-revision", "0")
	if err == nil {
		t.Fatal("offline coordinator accepted policy mutation")
	}
}

func TestStoppedAutomaticManualApprovalNamesExplicitRetry(t *testing.T) {
	var output bytes.Buffer
	run := update.Run{ID: strings.Repeat("a", 32), Origin: &update.AutomaticOrigin{PolicyID: strings.Repeat("b", 32)}, Stopped: true}
	if err := reportStoppedAutomaticApproval(run, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "mesh update retry "+run.ID+" --expected-policy-revision N") {
		t.Fatalf("manual approval omitted explicit recovery: %s", output.String())
	}
}

func TestAutomaticStatusHumanDefaultAndStructuredPrivacy(t *testing.T) {
	_, host := setupUpdateCLI(t)
	status := update.AutomaticStatus{State: "stopped", Record: &update.AutomaticRecord{Policy: update.AutomaticPolicy{ID: strings.Repeat("a", 32), CoordinatorID: host.ID}, Stop: &update.AutomaticStop{Kind: "failed", Problem: "private-problem"}, Current: &update.Run{ID: strings.Repeat("b", 32), ReleaseDigest: strings.Repeat("c", 64), Release: updateTestManifest(), Targets: []update.Target{{Host: host, State: update.Offline, Build: &release.Build{Version: "v0.1.0"}}}}}}
	caller := updateCallFunc(func(_ context.Context, _ update.Host, action string, _, output any) error {
		if action != "automatic.status" {
			return errors.New("unexpected presentation mutation")
		}
		*output.(*update.AutomaticStatus) = status
		return nil
	})
	for _, privacyFlag := range []string{"--privacy=false", "--privacy"} {
		stdout, _, err := executeCommand(t, Dependencies{UpdateCaller: caller}, "update", "automatic", "status", privacyFlag)
		if err != nil {
			t.Fatal(err)
		}
		for _, private := range []string{host.ID, status.Record.Policy.ID, status.Record.Current.ID, status.Record.Current.ReleaseDigest, "private-problem", host.Endpoint} {
			if strings.Contains(stdout, private) {
				t.Fatalf("default leaked %q: %s", private, stdout)
			}
		}
		if !strings.Contains(stdout, "Target v0.2.0; last observed daemon v0.1.0") || !strings.Contains(stdout, "exact pinned release") {
			t.Fatal(stdout)
		}
	}
	stdout, _, err := executeCommand(t, Dependencies{UpdateCaller: caller}, "update", "automatic", "status", "--json", "--privacy")
	if err != nil {
		t.Fatal(err)
	}
	var decoded update.AutomaticStatus
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatal("JSON preamble or invalid value", stdout, err)
	}
	if decoded.Record.Current.ReleaseDigest != status.Record.Current.ReleaseDigest || decoded.Record.Stop.Problem != "private-problem" {
		t.Fatal("structured status lost original diagnostics")
	}
}
