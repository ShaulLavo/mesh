package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/updateinstall"
)

func TestPendingAdoptionPreservesOrdinaryClientConfig(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "no-journal", true: "committed-awaiting-confirmation"}[committed], func(t *testing.T) {
			pendingAdoptionClientConfig(t, committed)
		})
	}
}

func pendingAdoptionClientConfig(t *testing.T, committed bool) {
	state, host := setupUpdateCLI(t)
	if _, err := update.OpenStore(state); err != nil {
		t.Fatal(err)
	}
	retained := obsoleteConfigFixture()
	config := writeClientConfigFixture(t, retained)
	t.Setenv("MESH_STATE_DIR", state)
	preserved, err := update.SnapshotAdoptionFiles([]string{config})
	if err != nil {
		t.Fatal(err)
	}
	operation := update.Adoption{
		Schema: 1, ID: strings.Repeat("a", 32), Pin: strings.Repeat("b", 64), Signature: strings.Repeat("c", 64), Version: "v0.2.0",
		Host:   update.AdoptionHost{Health: updateinstall.Health{HostID: host.ID, Build: updateTestBuild()}, Service: strings.Repeat("d", 64), Preserved: preserved},
		Policy: update.HostPolicy{Schema: 1, Revision: 1, Mode: update.HostAutomatic}, Phase: update.AdoptionWaiting,
		Request:  updateinstall.Request{ID: strings.Repeat("a", 32), TargetID: host.ID, Generation: 1, Current: updateTestBuild(), Manifest: updateTestManifest()},
		Settings: updateinstall.Settings{StateDir: state}, Unjournaled: true,
	}
	if committed {
		operation.Phase = update.AdoptionConfirming
		writeClientActivationJournal(t, state, updateinstall.Status{Schema: 1, Phase: updateinstall.Committed, Request: operation.Request, Settings: operation.Settings})
	}
	details, err := json.Marshal(operation)
	if err != nil {
		t.Fatal(err)
	}
	envelope := update.HostAdoption{Schema: 1, Operation: operation.ID, Pin: operation.Pin, Details: details}
	data, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(state, "updates"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "updates", "host-adoption.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if pending, err := update.AdoptionPending(t.Context(), state); err != nil || !pending {
		t.Fatalf("canonical pending adoption positive control: %v %v", pending, err)
	}
	if _, _, err := executeCommand(t, Dependencies{}, "device", "identity", "--json"); err != nil {
		t.Fatal(err)
	}
	after, err := readClientConfigTestFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, retained) {
		t.Fatal("ordinary command retired preserved aliases while canonical adoption is unconfirmed")
	}
}
