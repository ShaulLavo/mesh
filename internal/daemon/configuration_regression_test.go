package daemon

import (
	"testing"
	"time"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/update"
)

func TestConfigurationObserverRejectsOlderPublication(t *testing.T) {
	state := t.TempDir()
	host, key, err := identity.LoadOrCreate(state)
	if err != nil {
		t.Fatal(err)
	}
	controller, err := newUpdateController(t.Context(), state, host.ID, key)
	if err != nil {
		t.Fatal(err)
	}
	observation := configurationObservationFixture(t, host.ID)
	observation.State, observation.Enabled = "enabled", true
	observation.Digest = (update.FeedValue{Present: true, URL: "https://feed.fixture/usage.json"}).Digest()
	observation.Publication, observation.FeedRevision, observation.Failing = 3, 1, true
	configurationLocalObservation(t, controller, host.ID, key, observation)
	observation.Publication, observation.Failing = 2, false
	result := configurationLocalObservationResult(t, controller, host.ID, key, observation)
	if result.Problem == "" {
		t.Error("older publication accepted")
	}
	latest := controller.configurationObservers.snapshot(time.Now())
	if len(latest) != 1 {
		t.Fatal(latest)
	}
	if latest[0].Publication != 3 || !latest[0].Failing {
		t.Errorf("signed local older same-generation publication replaced acknowledged failure: publication=%d failing=%t", latest[0].Publication, latest[0].Failing)
	}
}
