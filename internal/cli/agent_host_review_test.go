package cli

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/hostprofile"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/transport"
)

func reviewContextHost(t *testing.T) (HostRecord, string) {
	t.Helper()
	state := t.TempDir()
	t.Setenv("MESH_STATE_DIR", state)
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	if _, _, err := identity.LoadOrCreate(state); err != nil {
		t.Fatal(err)
	}
	host := HostRecord{ID: argumentIdentities(t)[1], MeshIdentity: argumentIdentities(t)[1], Endpoint: "ws://127.0.0.1:1/control/ws", MachineName: "pc", NameRevision: 1}
	if err := saveNamedTestHost(t, host); err != nil {
		t.Fatal(err)
	}
	return host, state
}

func saveReviewContext(t *testing.T, host HostRecord, state string, section hostprofile.WireSection, fleet bool) {
	t.Helper()
	zero := uint64(0)
	absent := hostprofile.WireSection{State: hostprofile.StateAbsent, Revision: &zero, Completeness: section.Completeness}
	reply := hostprofile.ContextReply{Host: section, Fleet: absent}
	if fleet {
		reply.Host, reply.Fleet = absent, section
	}
	entry := hostprofile.CachedContext{HostID: host.ID, MeshIdentity: host.MeshIdentity, ReceivedAt: time.Unix(100, 0).UTC(), Observed: &hostprofile.ContextObservation{Version: "cached-version", Platform: "linux/amd64", Daemon: "running"}, Context: reply}
	if _, err := hostprofile.OpenContextCache(state).Save(t.Context(), entry); err != nil {
		t.Fatal(err)
	}
}

func TestAgentHostCachedUnavailableRemainsPartial(t *testing.T) {
	for _, fleet := range []bool{false, true} {
		t.Run(map[bool]string{false: "host", true: "fleet"}[fleet], func(t *testing.T) { checkReviewUnavailable(t, fleet) })
	}
}

func checkReviewUnavailable(t *testing.T, fleet bool) {
	t.Helper()
	host, state := reviewContextHost(t)
	section := hostprofile.WireSection{State: hostprofile.StateUnavailable, Completeness: hostprofile.Full, Problem: "original source read failed"}
	saveReviewContext(t, host, state, section, fleet)
	stdout, _, err := executeCommand(t, Dependencies{}, "inspect", host.ID, "--json", "--cached")
	assertHostReadError(t, stdout, err, agentapi.StateUnavailable, 3)
	detail := runHostDetail(t, Dependencies{}, 3, "inspect", host.ID, "--json", "--cached")
	selected := detail.Host.Declared.Host
	if fleet {
		selected = detail.Host.Declared.Fleet.ProfileSection
	}
	if selected.State != "unavailable" || selected.Problem != section.Problem {
		t.Fatalf("source failure changed: %+v", selected)
	}
	stdout, _, err = executeCommand(t, Dependencies{}, "hosts", "--json", "--cached")
	assertHostReadError(t, stdout, err, agentapi.StateUnavailable, 3)
	dial := agentContextFixtureDial(t, host, t.TempDir(), t.TempDir(), new(atomic.Int32))
	runHostDetail(t, Dependencies{DialPassive: dial}, 0, "inspect", host.ID, "--json")
	runHostDetail(t, Dependencies{}, 0, "inspect", host.ID, "--json", "--cached")
}

func TestAgentHostCachedInvalidSourcePreservesClassification(t *testing.T) {
	for _, fleet := range []bool{false, true} {
		t.Run(map[bool]string{false: "host", true: "fleet"}[fleet], func(t *testing.T) { checkReviewInvalid(t, fleet) })
	}
}

func checkReviewInvalid(t *testing.T, fleet bool) {
	t.Helper()
	host, state := reviewContextHost(t)
	revision := uint64(7)
	section := hostprofile.WireSection{State: hostprofile.StateInvalid, Completeness: hostprofile.Summary, Problem: "profile document is malformed", Revision: &revision}
	saveReviewContext(t, host, state, section, fleet)
	detail := runHostDetail(t, Dependencies{}, 0, "inspect", host.ID, "--json", "--cached")
	selected := detail.Host.Declared.Host
	if fleet {
		selected = detail.Host.Declared.Fleet.ProfileSection
	}
	if selected.State != "invalid" || selected.Problem != section.Problem || selected.Revision == nil || *selected.Revision != revision {
		t.Fatalf("invalid observation changed: %+v", selected)
	}
}

func TestAgentHostContextFailureKeepsValidatedInfo(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(map[bool]string{false: "first read", true: "existing cache"}[cached], func(t *testing.T) { checkReviewValidatedInfo(t, cached) })
	}
}

func checkReviewValidatedInfo(t *testing.T, cached bool) {
	t.Helper()
	host, state := reviewContextHost(t)
	if cached {
		doc := hostprofile.HostProfile{Editable: hostprofile.Editable{SchemaVersion: 1, Instructions: "cached notes"}, HostID: host.ID, Revision: 1, UpdatedAt: time.Unix(100, 0).UTC()}
		section := hostprofile.HostSection(hostprofile.Snapshot[hostprofile.HostProfile]{State: hostprofile.StateValid, Value: &doc, Revision: &doc.Revision}, true)
		saveReviewContext(t, host, state, section, false)
	}
	detail := runHostDetail(t, Dependencies{DialPassive: reviewContextFailureDial(host)}, 3, "inspect", host.ID, "--json")
	if detail.Host.Observed == nil || detail.Host.Observed.Version != "fresh-version" || detail.Host.Observed.Platform != "linux/amd64" || detail.Host.Observed.Freshness != agentapi.FreshnessLive || detail.Host.MachineName != "fresh-pc" || detail.Host.Reachability != "reachable" {
		t.Fatalf("validated information lost: %+v", detail)
	}
	if detail.Access.State != "unavailable" || detail.Access.Grant != "unknown" || detail.Access.ObservedAt != nil {
		t.Fatalf("context failure certified access: %+v", detail.Access)
	}
	if cached && (detail.Host.Declared.Host.Profile == nil || *detail.Host.Declared.Host.Profile.Instructions != "cached notes" || detail.Host.Declared.Host.Freshness != agentapi.FreshnessCached) {
		t.Fatalf("cached context replaced by bad reply: %+v", detail.Host.Declared)
	}
	if !cached && detail.Host.Declared.Host.Profile != nil {
		t.Fatal("bad reply exposed a profile")
	}
}

func reviewContextFailureDial(host HostRecord) HostDialer {
	current := host
	current.MachineName, current.NameRevision = "fresh-pc", 2
	info := testHostDeclaration(current)
	info.Build = &release.Build{Version: "fresh-version", Platform: release.Platform{OS: "linux", Arch: "amd64"}}
	respond := func(request protocol.Control) protocol.Control {
		if request.Type == protocol.TypeHostInfo {
			return protocol.Control{Type: protocol.TypeHostInfoResult, Host: info}
		}
		return protocol.Control{Type: protocol.TypeHostContextResult, Context: json.RawMessage(`{}`)}
	}
	return func(context.Context, HostRecord) (transport.Conn, error) {
		return newServiceRemoteTestConn(respond), nil
	}
}
