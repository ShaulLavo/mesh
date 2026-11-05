package protocol

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	meshserve "github.com/shaul/mesh/internal/serve"
)

func TestServiceRunCarriesTheKeptLifetime(t *testing.T) {
	service := meshserve.Service{Name: "ai", Kind: meshserve.Proxy, Target: "8317", Demand: &meshserve.Demand{
		Command: "./run", Cwd: "/srv/proxy", ReadyTimeout: time.Minute, KeepRunning: true,
	}}
	info := ServiceDefinitionInfo(service)
	if info.Run == nil || !info.Run.KeepRunning || info.Run.IdleMillis != 0 {
		t.Fatalf("wire recipe = %+v, want kept with no idle window", info.Run)
	}
	encoded, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"keepRunning":true`) || strings.Contains(string(encoded), `"idleMs"`) {
		t.Fatalf("wire form = %s", encoded)
	}
	var decoded ServiceInfo
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if back := ServiceFromInfo(decoded); !back.Demand.Equal(service.Demand) {
		t.Fatalf("round trip = %+v, want %+v", back.Demand, service.Demand)
	}
	service.Demand.KeepRunning, service.Demand.Idle = false, time.Minute
	encoded, err = json.Marshal(ServiceDefinitionInfo(service))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "keepRunning") {
		t.Fatalf("a finite recipe encodes a kept field: %s", encoded)
	}
}

func TestServiceDemandCarriesHeld(t *testing.T) {
	for _, held := range []bool{false, true} {
		encoded, err := json.Marshal(ServiceDemand{State: DemandStopped, Held: held})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), `"held":true`) != held || !held && strings.Contains(string(encoded), "held") {
			t.Fatalf("held=%v encodes as %s", held, encoded)
		}
		var decoded ServiceDemand
		if err := json.Unmarshal(encoded, &decoded); err != nil || decoded.Held != held {
			t.Fatalf("held=%v decodes as %+v, %v", held, decoded, err)
		}
	}
}
