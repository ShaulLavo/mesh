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

	"github.com/shaul/mesh/internal/update"
)

func TestAutomaticCLIRequiresExistingIdentityAndNeverCreatesIt(t *testing.T) {
	state := t.TempDir()
	t.Setenv("MESH_STATE_DIR", state)
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	_, _, err := executeCommand(t, Dependencies{}, "update", "automatic", "status", "--json")
	if err == nil {
		t.Fatal("automatic status created an identity")
	}
	if _, err := os.Stat(filepath.Join(state, "identity.key")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("identity created: %v", err)
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
