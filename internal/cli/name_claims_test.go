package cli

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/machinename"
	"github.com/shaul/mesh/internal/protocol"
)

func TestResolveArgumentExactDestinationID(t *testing.T) {
	host := HostRecord{MachineName: "viewer-label", ID: "exact-destination-identity"}
	target, err := ResolveArgument(host.ID, []HostRecord{host})
	if err != nil || target.Host == nil || target.Host.ID != host.ID {
		t.Fatalf("exact destination ID did not resolve: target=%+v err=%v", target, err)
	}
}

func TestStateWatchRefusesAnotherDestinationName(t *testing.T) {
	host := HostRecord{MachineName: "destination", ID: "expected-id", MeshIdentity: "expected-pin"}
	for _, response := range []protocol.Control{
		{Type: protocol.TypeStateSnapshot, StateSnapshot: &protocol.StateSnapshot{Seq: 1, Host: &protocol.HostInfo{ID: "another-id", MeshIdentity: "expected-pin", MachineName: "forged", NameRevision: 1}}},
		{Type: protocol.TypeStateEvent, StateEvent: &protocol.StateEvent{Seq: 2, Kind: "host.changed", Payload: protocol.StatePayload{Host: &protocol.HostInfo{ID: "expected-id", MeshIdentity: "another-pin", MachineName: "forged", NameRevision: 1}}}},
	} {
		if err := validateStateHost(host, response); err == nil {
			t.Fatalf("another destination's name accepted in %s", response.Type)
		}
	}
}

func TestResolveArgumentRefusesKnownNameConflictAndKeepsExactIDs(t *testing.T) {
	a := HostRecord{ID: "a-destination-id", MachineName: "shared", NameRevision: 1}
	b := HostRecord{ID: "b-destination-id", MachineName: "shared", NameRevision: 7}
	for _, hosts := range [][]HostRecord{{a, b}, {b, a}} {
		if target, err := ResolveArgument("SHARED", hosts); err == nil || target.Host != nil || !strings.Contains(err.Error(), "a-destination-id, b-destination-id") {
			t.Fatalf("ambiguous name could select another host: %+v %v", target, err)
		}
		for _, host := range hosts {
			target, err := ResolveArgument(host.ID, hosts)
			if err != nil || target.Host == nil || target.Host.ID != host.ID || target.Host.targetName != "" {
				t.Fatalf("exact-ID conflict target: %+v %v", target, err)
			}
		}
	}
	b.MachineName, b.NameRevision = "renamed", 8
	target, err := ResolveArgument("shared", []HostRecord{b, a})
	if err != nil || target.Host == nil || target.Host.ID != a.ID || target.Host.targetName != "shared" {
		t.Fatalf("owner rename did not resolve the conflict: %+v %v", target, err)
	}
}

func TestStateNameGapReplayAndMixedPayloadCannotPoisonCacheOrView(t *testing.T) {
	f := namedDestination(t)
	if _, _, err := f.names.Rename(t.Context(), f.host.ID, "renamed", 1); err != nil {
		t.Fatal(err)
	}
	info := f.info()
	view := StateView{}
	now := time.Now()
	initial := protocol.Control{Type: protocol.TypeStateSnapshot, StateSnapshot: &protocol.StateSnapshot{Seq: 1, Host: &info, Current: map[string]protocol.Observation{protocol.TopicHost: {}}}}
	if err := applyVerifiedState(t.Context(), f.host, &view, initial, now, 0); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		kind     string
		seq      uint64
		id       string
		name     string
		revision uint64
		want     error
	}{
		{"host.changed", 5, f.host.ID, "next-name", 3, ErrStateGap},
		{"host.changed", 2, f.host.ID, "destination", 1, machinename.ErrReplay},
		{"host.changed", 2, f.host.ID, "equivocated", 2, machinename.ErrEquivocation},
		{"host.changed", 2, "another-owner", "forged", 99, nil},
		{"session.changed", 2, f.host.ID, "smuggled", 3, nil},
	} {
		next := info
		next.ID, next.MachineName, next.NameRevision = test.id, test.name, test.revision
		response := protocol.Control{Type: protocol.TypeStateEvent, StateEvent: &protocol.StateEvent{Seq: test.seq, Kind: test.kind, Payload: protocol.StatePayload{Host: &next}}}
		if test.id != f.host.ID {
			response.StateSnapshot = initial.StateSnapshot
		}
		err := applyVerifiedState(t.Context(), f.host, &view, response, now.Add(time.Second), 0)
		if err == nil || test.want != nil && !errors.Is(err, test.want) {
			t.Fatalf("invalid event accepted: kind=%s revision=%d err=%v", test.kind, test.revision, err)
		}
		if view.Seq != 1 || view.Name != declaredName(info) {
			t.Fatal("rejected event changed the shown subject")
		}
	}
	path, err := ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	cached, err := machinename.CachedClaim(filepath.Dir(path), f.host.ID)
	if err != nil || cached != declaredName(info) {
		t.Fatalf("rejected event changed cached claim: %+v %v", cached, err)
	}
	view.Connection = StateUnreachable
	if view.Name != declaredName(info) {
		t.Fatal("disconnect lost the last authenticated name")
	}
}

func TestStateCurrentCannotSmuggleUnverifiedHostEvent(t *testing.T) {
	f := namedDestination(t)
	info := f.info()
	view := StateView{}
	now := time.Now()
	initial := protocol.Control{Type: protocol.TypeStateSnapshot, StateSnapshot: &protocol.StateSnapshot{Seq: 1, Host: &info}}
	if err := applyVerifiedState(t.Context(), f.host, &view, initial, now, 0); err != nil {
		t.Fatal(err)
	}
	forged := info
	forged.ID, forged.MachineName, forged.NameRevision = "another-owner", "forged", 99
	response := protocol.Control{
		Type:         protocol.TypeStateCurrent,
		StateCurrent: &protocol.StateCurrent{Seq: 2},
		StateEvent:   &protocol.StateEvent{Seq: 2, Kind: "host.changed", Payload: protocol.StatePayload{Host: &forged}},
	}
	if err := applyVerifiedState(t.Context(), f.host, &view, response, now, 0); err == nil {
		t.Fatal("state.current applied an unverified host event")
	}
	if view.Name != declaredName(info) || view.Seq != 1 {
		t.Fatal("rejected mixed control changed the shown subject")
	}
}

func TestPolledLegacyOwnerRetainsCachedNameAsUnverified(t *testing.T) {
	f := namedDestination(t)
	view := StateView{Name: f.names.Current(), NameVerified: true, Sections: map[string]ObservedSection{}}
	legacy := protocol.HostInfo{ID: f.host.ID, MeshIdentity: f.host.MeshIdentity}
	response := protocol.Control{Type: protocol.TypeHostInfoResult, Host: &legacy}
	if err := applyPolledSection(f.host, protocol.TopicHost, response, &view, time.Now(), 0); err != nil {
		t.Fatal(err)
	}
	if view.Name != f.names.Current() || view.NameVerified {
		t.Fatalf("legacy owner erased or freshly verified cached name: %+v", view)
	}
}

func TestAuthenticatedOwnerCannotDeclareAnotherCryptographicIdentity(t *testing.T) {
	f := namedDestination(t)
	if _, err := listRemoteHost(t.Context(), f.host, dialControlHost, HostQueryBudget{Setup: remoteConnectTimeout, Reply: defaultCatalogTimeout}); err != nil {
		t.Fatalf("legitimate named B/B: %v", err)
	}
	other, _, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path, err := ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	original := machinename.Claim{ID: other.ID, MachineName: "other-real-owner", Revision: 1}
	if _, err := machinename.RememberClaim(t.Context(), filepath.Dir(path), other.ID, original); err != nil {
		t.Fatal(err)
	}
	forged := f.info()
	forged.ID, forged.MachineName, forged.NameRevision = other.ID, "forged-owner", 99
	f.reported.Store(&forged)
	expected := f.host
	expected.ID = other.ID
	if _, err := listRemoteHost(t.Context(), expected, dialControlHost, HostQueryBudget{Setup: remoteConnectTimeout, Reply: defaultCatalogTimeout}); err == nil {
		t.Fatal("authenticated owner admitted a claim for another cryptographic ID")
	}
	if f.operations.Load() != 1 {
		t.Fatal("foreign claim reached a session effect")
	}
	view := StateView{Name: original, NameVerified: true, Seq: 1, Sections: map[string]ObservedSection{}}
	view = view.Clone()
	before := view.Clone()
	for _, response := range []protocol.Control{
		{Type: protocol.TypeStateSnapshot, StateSnapshot: &protocol.StateSnapshot{Seq: 2, Host: &forged}},
		{Type: protocol.TypeStateEvent, StateEvent: &protocol.StateEvent{Seq: 2, Kind: "host.changed", Payload: protocol.StatePayload{Host: &forged}}},
	} {
		if err := applyVerifiedState(t.Context(), expected, &view, response, time.Now(), 0); err == nil {
			t.Fatal("foreign claim entered watched state")
		}
		if !reflect.DeepEqual(view, before) {
			t.Fatal("foreign watched claim changed view")
		}
	}
	if err := applyPolledSection(expected, protocol.TopicHost, protocol.Control{Type: protocol.TypeHostInfoResult, Host: &forged}, &view, time.Now(), 0); err == nil {
		t.Fatal("foreign claim entered polled state")
	}
	if !reflect.DeepEqual(view, before) {
		t.Fatal("foreign polled claim changed view")
	}
	cached, err := machinename.CachedClaim(filepath.Dir(path), other.ID)
	if err != nil || cached != original {
		t.Fatalf("foreign claim changed owner cache: %+v %v", cached, err)
	}
	legacy := forged
	legacy.MachineName, legacy.NameRevision = "", 0
	if err := validateHostInfo(expected, legacy); err != nil {
		t.Fatalf("unnamed legacy diagnostic changed: %v", err)
	}
}
