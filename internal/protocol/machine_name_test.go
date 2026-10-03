package protocol

import (
	"math"
	"reflect"
	"testing"
)

func TestMachineNameControlRoundTrips(t *testing.T) {
	host := &HostInfo{ID: "destination-id", MeshIdentity: "destination-id", MachineName: "destination-pc", NameRevision: math.MaxUint64}
	for _, want := range []Control{
		{Type: TypeHostRename, RequestID: "rename", Rename: &HostRename{TargetID: host.ID, MachineName: host.MachineName, ExpectedRevision: math.MaxUint64}},
		{Type: TypeHostRenamed, RequestID: "rename", Host: host},
		{Type: TypeHostInfoResult, RequestID: "read", Host: host},
		{Type: TypeStateSnapshot, StateSnapshot: &StateSnapshot{Host: host}},
		{Type: TypeStateEvent, StateEvent: &StateEvent{Kind: "host.changed", Payload: StatePayload{Host: host}}},
	} {
		t.Run(want.Type, func(t *testing.T) {
			payload, err := want.Encode()
			if err != nil {
				t.Fatal(err)
			}
			got, err := DecodeControl(payload)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("destination name wire round trip failed: %v", err)
			}
		})
	}
}

func TestMachineNameTopicIsExplicit(t *testing.T) {
	watch := StateWatch{Topics: []string{TopicHost, TopicSessions, TopicServices, TopicMetrics}}
	if err := watch.Validate(); err != nil {
		t.Fatal(err)
	}
}
