package agentapi

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestProfileRevisionAndAccessObservations(t *testing.T) {
	zero := uint64(0)
	known := uint64(7)
	for _, test := range []struct {
		section  ProfileSection
		revision string
	}{
		{ProfileSection{State: "absent", Revision: &zero, Completeness: CompletenessSummary}, `"revision":0`},
		{ProfileSection{State: "invalid", Revision: &known, Completeness: CompletenessFull}, `"revision":7`},
		{ProfileSection{State: "invalid", Revision: nil, Completeness: CompletenessFull}, `"revision":null`},
	} {
		encoded, err := json.Marshal(test.section)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(encoded), test.revision) || strings.Contains(string(encoded), "knownRevision") {
			t.Fatal(string(encoded))
		}
	}
	encoded, err := json.Marshal(HostDetail{Access: Access{State: "unavailable", Grant: "unknown", Reason: "No current authenticated observation."}})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"observedAt":null`, `"principal":null`, `"scopedPermissions":null`, `"grant":"unknown"`, `"reason":"No current authenticated observation."`} {
		if !strings.Contains(string(encoded), field) {
			t.Fatalf("missing %s in %s", field, encoded)
		}
	}
}

func TestSupportedDTOEmptyCollections(t *testing.T) {
	at := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	values := []any{
		Profile{Labels: map[string]string{}, Warnings: []string{}},
		HostsData{ReceivedAt: at, Hosts: []HostSummary{}},
		SessionPage{Sessions: []SessionSummary{}},
		SavedInspection{Command: []string{}, Preview: []string{}},
		ServicesData{Services: []ServiceRow{}, HostErrors: []Error{}},
		ProfileWrite{Members: []Delivery{}},
		FleetStatus{Members: []Delivery{}},
		SkillResult{Roots: []SkillRoot{{ModifiedFiles: []string{}, MissingFiles: []string{}, ProposedFiles: []string{}, RetainedFiles: []string{}, Problems: []Error{}}}},
	}
	for _, value := range values {
		checkCollectionEncoding(t, value)
	}
}

func checkCollectionEncoding(t *testing.T, value any) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"hosts", "labels", "warnings", "sessions", "command", "preview", "services", "hostErrors", "members", "roots", "modifiedFiles", "missingFiles", "proposedFiles", "retainedFiles", "problems"} {
		if strings.Contains(string(encoded), `"`+name+`":null`) {
			t.Fatal(string(encoded))
		}
	}
}
