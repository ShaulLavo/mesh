package update

import (
	"testing"
	"time"
)

func TestUnfinishedIgnoresMachinesALaterRunVerified(t *testing.T) {
	at := func(minutes int) time.Time { return time.Date(2026, 10, 3, 0, minutes, 0, 0, time.UTC) }
	target := func(id string, state State) Target { return Target{Host: Host{ID: id}, State: state} }
	old := Run{ID: "old", CreatedAt: at(0), Targets: []Target{target("mac", Updated), target("vps", Failed)}}
	partial := Run{ID: "partial", CreatedAt: at(10), Targets: []Target{target("pi", Failed)}}
	fleet := Run{ID: "fleet", CreatedAt: at(20), Targets: []Target{target("mac", Updated), target("vps", Updated)}}
	newest := Run{ID: "newest", CreatedAt: at(30), Targets: []Target{target("mac", Pending)}}

	got := Unfinished([]Run{newest, fleet, partial, old})
	if len(got) != 2 || got[0].ID != "newest" || got[1].ID != "partial" {
		ids := make([]string, len(got))
		for i, run := range got {
			ids[i] = run.ID
		}
		t.Fatalf("unfinished runs = %v, want [newest partial]: vps was verified by a later run, pi and mac were not", ids)
	}
}
