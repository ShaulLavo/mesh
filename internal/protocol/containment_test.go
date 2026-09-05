package protocol

import (
	"strings"
	"testing"
)

func TestContainingSessionsRoundTrip(t *testing.T) {
	want := []SessionIdentity{
		{HostID: "host-b", SessionID: "7K3D"},
		{HostID: "host-a", SessionID: "91AZ"},
	}
	payload, err := (Control{
		Type:               TypeAttach,
		SessionID:          "NEXT",
		ContainingSessions: want,
	}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeControl(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ContainingSessions) != len(want) {
		t.Fatalf("containing sessions = %#v, want %#v", got.ContainingSessions, want)
	}
	for index := range want {
		if got.ContainingSessions[index] != want[index] {
			t.Fatalf("containing session %d = %#v, want %#v", index, got.ContainingSessions[index], want[index])
		}
	}
}

func TestValidateContainingSessions(t *testing.T) {
	valid := []SessionIdentity{
		{HostID: "host-b", SessionID: "7K3D"},
		{HostID: "host-a", SessionID: "91AZ"},
	}
	if err := ValidateContainingSessions(valid); err != nil {
		t.Fatalf("valid containment rejected: %v", err)
	}
	if err := ValidateContainingSessions(nil); err != nil {
		t.Fatalf("empty containment rejected: %v", err)
	}

	tests := []struct {
		name       string
		identities []SessionIdentity
	}{
		{name: "empty host", identities: []SessionIdentity{{SessionID: "7K3D"}}},
		{name: "noncanonical host", identities: []SessionIdentity{{HostID: " host-b ", SessionID: "7K3D"}}},
		{name: "oversized host", identities: []SessionIdentity{{HostID: strings.Repeat("h", MaxSessionIdentityHostIDBytes+1), SessionID: "7K3D"}}},
		{name: "invalid session", identities: []SessionIdentity{{HostID: "host-b", SessionID: "NOPE"}}},
		{name: "noncanonical session", identities: []SessionIdentity{{HostID: "host-b", SessionID: "7k3d"}}},
		{name: "duplicate cycle", identities: append(append([]SessionIdentity(nil), valid...), valid[0])},
		{name: "too deep", identities: make([]SessionIdentity, MaxContainingSessions+1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateContainingSessions(test.identities); err == nil {
				t.Fatalf("ValidateContainingSessions(%#v) error = nil", test.identities)
			}
		})
	}
}

func TestCloneSessionIdentitiesIsIndependent(t *testing.T) {
	source := []SessionIdentity{{HostID: "host-a", SessionID: "7K3D"}}
	clone := CloneSessionIdentities(source)
	clone[0].HostID = "changed"
	if source[0].HostID != "host-a" {
		t.Fatalf("source changed through clone: %#v", source)
	}
}
