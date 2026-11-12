package update

import (
	"context"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/release"
)

func TestHostStatusUsesAuthenticatedDiscoveryAndKeepsPendingClock(t *testing.T) {
	f := newHostFixture(t)
	f.tick()
	status, err := f.store.HostStatus(t.Context())
	if err != nil || status.Discovery == nil || status.Discovery.Target.Metadata == "" || status.Discovery.Source != f.current {
		t.Fatalf("authenticated discovery absent: %+v %v", status, err)
	}
	prior := *status.Discovery
	f.now = f.now.Add(time.Hour)
	var cursor DiscoveryCursor
	if err := f.store.readAuthority("host-discovery.json", &cursor); err != nil {
		t.Fatal(err)
	}
	source, err := f.documents.Current(t.Context(), f.current)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.updater.observeDiscovery(t.Context(), f.current, source, DiscoveryStep{Cursor: cursor}, cursor.Metadata); err != nil {
		t.Fatal(err)
	}
	status, err = f.store.HostStatus(t.Context())
	if err != nil || !status.Discovery.PendingSince.Equal(prior.PendingSince) || !status.Discovery.ObservedAt.After(prior.ObservedAt) {
		t.Fatalf("poll reset pending clock: %+v %v", status, err)
	}
	cursor.Head.SHA256 = "wrong pin"
	if err := f.updater.observeDiscovery(t.Context(), f.current, source, DiscoveryStep{Cursor: cursor}, cursor.Metadata); err == nil {
		t.Fatal("unverified cursor supplied advisory target")
	}
	changed := f.current
	changed.Digest = prior.Target.Metadata
	if err := f.updater.observeDiscovery(t.Context(), changed, source, DiscoveryStep{Cursor: cursor}, cursor.Metadata); err == nil {
		t.Fatal("advisory source differed from executing identity")
	}
}

func TestHostStatusPartialDiscoveryStaysUnknown(t *testing.T) {
	f := newHostFixture(t)
	if err := f.updater.observeDiscovery(t.Context(), release.Build{}, HostSource{}, DiscoveryStep{}, nil); err != nil {
		t.Fatal(err)
	}
	status, err := f.store.HostStatus(t.Context())
	if err != nil || status.Discovery != nil || status.Last != nil {
		t.Fatalf("partial discovery became status: %+v %v", status, err)
	}
}

func TestHostStatusSettlesAndNewTargetResetsPendingClock(t *testing.T) {
	f := newHostFixture(t)
	f.tick()
	first, err := f.store.HostStatus(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	f.finish()
	f.tick()
	f.now = f.now.Add(time.Hour)
	f.tick()
	f.finish()
	f.tick()
	f.now = f.now.Add(time.Hour)
	f.tick()
	settled, err := f.store.HostStatus(t.Context())
	if err != nil || !settled.Discovery.PendingSince.IsZero() || settled.Discovery.Source != f.current || settled.Last == nil || settled.Last.Version != "v0.1.3" || settled.Last.Metadata == "" {
		t.Fatalf("settlement retained pending state or lost exact commit: %+v %v", settled, err)
	}
	f.addRelease(4, []uint64{3})
	f.head(4)
	f.now = f.now.Add(time.Hour)
	f.tick()
	next, err := f.store.HostStatus(t.Context())
	if err != nil || next.Discovery.Target.Version != "v0.1.4" || !next.Discovery.PendingSince.After(first.Discovery.PendingSince) {
		t.Fatalf("new target reused old wait clock: %+v %v", next, err)
	}
	deps, err := f.store.HostDependents(t.Context())
	if err != nil || len(deps) != 0 {
		t.Fatalf("status read acknowledged retention: %+v %v", deps, err)
	}
}

func TestHostStatusWaitSinceSurvivesPollingAndRestart(t *testing.T) {
	f := newHostFixture(t)
	if err := f.updater.wait(context.Background(), HostUpdate{Schema: 1, Phase: HostIdle}, "Waiting for canary"); err != nil {
		t.Fatal(err)
	}
	first, err := f.store.HostState(t.Context())
	if err != nil || first.ProblemSince.IsZero() {
		t.Fatal(first, err)
	}
	f.now = f.now.Add(time.Hour)
	f.updater = f.newUpdater()
	if err := f.updater.wait(t.Context(), first, "Waiting for canary"); err != nil {
		t.Fatal(err)
	}
	second, err := f.store.HostState(t.Context())
	if err != nil || !second.ProblemSince.Equal(first.ProblemSince) {
		t.Fatalf("restart reset wait: %+v %v", second, err)
	}
	if err := f.updater.wait(t.Context(), second, "Waiting for running update"); err != nil {
		t.Fatal(err)
	}
	third, err := f.store.HostState(t.Context())
	if err != nil || !third.ProblemSince.After(first.ProblemSince) {
		t.Fatalf("changed reason did not reset wait: %+v %v", third, err)
	}
}
