package protocol

import (
	"reflect"
	"testing"
	"time"
)

func TestLifecycleControlRoundTrip(t *testing.T) {
	createdAt := time.Date(2026, time.August, 29, 12, 34, 56, 0, time.UTC)
	attachedAt := createdAt.Add(time.Minute)
	exitCode := 7
	want := Control{
		Type:      TypeListed,
		RequestID: "request-7",
		Sessions: []SessionInfo{{
			ID:                 "7K3D",
			HostID:             "host-public-key",
			Command:            []string{"sh", "-lc", "printf ready"},
			Cwd:                "/tmp/work",
			State:              "exited",
			CreatedAt:          createdAt,
			LastAttachedAt:     &attachedAt,
			ExitCode:           &exitCode,
			LastOutputSequence: 4096,
		}},
	}

	payload, err := want.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeControl(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded control = %#v, want %#v", got, want)
	}
}

func TestHostInfoControlRoundTrip(t *testing.T) {
	want := Control{
		Type:      TypeHostInfoResult,
		RequestID: "request-8",
		Host: &HostInfo{
			ID:            "host-public-key",
			MeshIdentity:  "host-public-key",
			TailscaleName: "desktop.example.ts.net",
		},
	}

	payload, err := want.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeControl(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded control = %#v, want %#v", got, want)
	}
}
