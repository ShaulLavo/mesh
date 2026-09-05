package protocol

import (
	"testing"
	"time"
)

func TestNestingCapabilityRoundTripIncludesEmptySet(t *testing.T) {
	for _, kind := range []string{TypeAttached, TypeNesting} {
		payload, err := (Control{Type: kind, NestingSupported: true}).Encode()
		if err != nil {
			t.Fatal(err)
		}
		message, err := DecodeControl(payload)
		if err != nil || !message.NestingSupported || len(message.Nested) != 0 {
			t.Fatalf("empty capability round trip = %#v, %v", message, err)
		}
	}
	message, err := DecodeControl([]byte(`{"type":"session.attached"}`))
	if err != nil || message.NestingSupported {
		t.Fatalf("legacy response = %#v, %v", message, err)
	}
}

func TestNestingIdentityRoundTrip(t *testing.T) {
	identity := SessionIdentity{HostID: "pc", SessionID: "7K3D"}
	payload, err := (Control{Type: TypeNest, NestedSession: &identity}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	message, err := DecodeControl(payload)
	if err != nil || message.NestedSession == nil || *message.NestedSession != identity {
		t.Fatalf("registration round trip = %#v, %v", message, err)
	}
}

func TestInspectionValidatesNestedIdentities(t *testing.T) {
	identity := SessionIdentity{HostID: "pc", SessionID: "7K3D"}
	for _, nested := range [][]SessionIdentity{nil, {identity}} {
		if err := ValidateSessionInspection(SessionInspection{ObservedAt: time.Now(), Nested: nested}); err != nil {
			t.Fatalf("valid nesting rejected: %v", err)
		}
	}
	for _, nested := range [][]SessionIdentity{
		{{HostID: "", SessionID: "7K3D"}},
		{identity, identity},
		make([]SessionIdentity, MaxNestedSessions+1),
	} {
		if err := ValidateSessionInspection(SessionInspection{ObservedAt: time.Now(), Nested: nested}); err == nil {
			t.Fatalf("invalid nesting accepted: %#v", nested)
		}
	}
}
