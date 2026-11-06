package machinename

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentCommandNamesRejectNewAdmission(t *testing.T) {
	store, _, id := nameFixture(t)
	for _, name := range []string{"hosts", "inspect", "profile", "session", "skill"} {
		if _, _, err := store.Rename(t.Context(), id, name, 1); err == nil {
			t.Errorf("new name %q accepted", name)
		}
		if got := Initial(id, name+".lan"); !strings.HasPrefix(got, "host-") {
			t.Errorf("initial name %q accepted as %q", name, got)
		}
	}
}

func TestAgentCommandNamesPreserveStoredAndReceivedClaims(t *testing.T) {
	_, directory, id := nameFixture(t)
	current := record{Version: 1, Claim: Claim{ID: id, MachineName: "session", Revision: 1}}
	data, err := json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, stateName), data, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(t.Context(), directory, id, "replacement")
	if err != nil {
		t.Fatal(err)
	}
	if got := store.Current(); got != current.Claim {
		t.Fatalf("stored claim changed: %+v", got)
	}
	if err := ValidateClaim(id, current.Claim); err != nil {
		t.Fatal(err)
	}
}
