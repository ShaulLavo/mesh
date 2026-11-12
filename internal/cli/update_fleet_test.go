package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/updateinstall"
)

func currentFleetSummary(now time.Time) *protocol.HostUpdateSummary {
	zero := uint64(0)
	published := now.Add(-time.Hour)
	return &protocol.HostUpdateSummary{Mode: "automatic", Phase: "idle", Installation: "managed", SourceVerified: true,
		Latest: &protocol.HostUpdateRelease{Version: "v0.1.0", Metadata: strings.Repeat("a", 64), PublishedAt: &published}, Lag: &zero, ObservedAt: &now}
}

func TestFleetUpdateCLIExitAndOrdinaryHostInfo(t *testing.T) {
	for _, kind := range []string{fleetCurrent, fleetPending, fleetFailed, "package", "client", "development", fleetUnknown} {
		t.Run(kind, func(t *testing.T) { testFleetUpdateCLI(t, kind) })
	}
}

func testFleetUpdateCLI(t *testing.T, kind string) {
	_, local := setupUpdateCLI(t)
	path, _ := ConfigPath()
	saveUpdateTestFleet(t, filepath.Join(filepath.Dir(path), "fleet.json"), scopedUpdateFleet("fixture", []update.Host{local}))
	now := time.Now().UTC()
	summary := currentFleetSummary(now)
	want := changeFleetFixture(summary, now, kind)
	if kind == fleetUnknown {
		summary = nil
	}
	dial := func(_ context.Context, host HostRecord) (transport.Conn, error) {
		return newServiceRemoteTestConn(func(request protocol.Control) protocol.Control {
			if request.Type != protocol.TypeHostInfo {
				t.Errorf("fleet status requested %s", request.Type)
			}
			build := release.Build{Version: "v0.1.0", Digest: strings.Repeat("c", 64)}
			return protocol.Control{Type: protocol.TypeHostInfoResult, Host: &protocol.HostInfo{ID: host.ID, MeshIdentity: host.ID, Build: &build, HostUpdate: summary}}
		}), nil
	}
	deps := Dependencies{DialPassive: dial, Now: func() time.Time { return now }, UpdateCaller: updateCallFunc(func(context.Context, update.Host, string, any, any) error {
		t.Fatal("fleet status required update administrator authority")
		return nil
	})}
	stdout, _, err := executeCommand(t, deps, "update", "fleet", "--json")
	code, _ := StatusCode(err)
	if code != want || want == 0 && err != nil {
		t.Fatalf("%s: exit %d, want %d: %v", kind, code, want, err)
	}
	var status FleetUpdateStatus
	if err := json.Unmarshal([]byte(stdout), &status); err != nil || status.ExitCode != want || len(status.Members) != 1 {
		t.Fatalf("fleet JSON %s: %v", stdout, err)
	}
	if (len(status.Problems) > 0) != (want == 1) || status.Members[0].State != kind {
		t.Fatalf("fleet result not classified: %+v", status)
	}
}

func changeFleetFixture(summary *protocol.HostUpdateSummary, now time.Time, kind string) int {
	switch kind {
	case fleetCurrent:
		return 0
	case fleetPending:
		at := now.Add(-time.Hour)
		summary.Latest.Version, summary.Lag, summary.PendingSince = "v0.2.0", nil, &at
		summary.Pending = "Waiting for canary."
		return 2
	case fleetFailed:
		summary.Phase, summary.Problem = "attention", "rollback failed: diagnostic"
	case "package", "client", "development":
		summary.Installation = kind
	}
	return 1
}

func TestFleetUpdateOverdueOfflineAndRefusal(t *testing.T) {
	now := time.Now().UTC()
	for _, age := range []time.Duration{time.Hour, 25 * time.Hour} {
		member := FleetUpdateMember{State: fleetOffline, LastReply: now.Add(-age)}
		want := 2
		if age > 24*time.Hour {
			want = 1
		}
		if code := classifyFleetUpdate(&member, now); code != want || member.State != fleetOffline {
			t.Fatalf("offline %s: %d %+v", age, code, member)
		}
	}
	summary := currentFleetSummary(now)
	changeFleetFixture(summary, now, fleetPending)
	at := now.Add(-9 * time.Hour)
	summary.PendingSince, summary.Soak = &at, 2*time.Hour
	member := FleetUpdateMember{Build: &release.Build{Version: "v0.1.0"}, Update: summary}
	if classifyFleetUpdate(&member, now) != 1 || member.State != fleetFailed {
		t.Fatalf("overdue wait remained pending: %+v", member)
	}
	if fleetObservationError(transport.ErrAuthentication) != fleetRefused || fleetObservationError(&hostIdentityError{}) != fleetRefused || fleetObservationError(errors.New("invalid reply")) != fleetFailed {
		t.Fatal("observation failures collapsed into offline")
	}
	if combineFleetExit(1, 2) != 1 || combineFleetExit(2, 1) != 1 || combineFleetExit(0, 2) != 2 {
		t.Fatal("pending concealed failure")
	}
}

func TestFleetUpdateUnknownPolicyAndStaleCheck(t *testing.T) {
	now := time.Now().UTC()
	summary := currentFleetSummary(now)
	old := now.Add(-time.Hour)
	summary.ObservedAt = &old
	member := FleetUpdateMember{Build: &release.Build{Version: "v0.1.0", Digest: strings.Repeat("c", 64)}, Update: summary}
	if classifyFleetUpdate(&member, now) != 1 || member.State != fleetUnknown {
		t.Fatal("stale check counted current", member)
	}
	summary.Mode, summary.Phase, summary.Problem = "", "attention", "authority state unreadable"
	member.State = ""
	if classifyFleetUpdate(&member, now) != 1 || member.State != fleetFailed || member.Problem != summary.Problem {
		t.Fatal("unknown policy was reported as off", member)
	}
}

func TestFleetUpdateLastReplyReadsExistingCacheWithoutCreatingOne(t *testing.T) {
	state, local := setupUpdateCLI(t)
	members := []update.Host{local}
	path := filepath.Join(state, catalogDatabaseName)
	if replies := fleetLastReplies(members); len(replies) != 0 {
		t.Fatal("missing cache supplied a verified reply")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("fleet read created a metadata database", err)
	}
	store, err := storage.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	cache := &SQLiteCatalogCache{store: store, now: func() time.Time { return at }, stateDir: state}
	if saved, err := cache.Save(t.Context(), HostRecord{ID: local.ID, MeshIdentity: local.ID}, nil); err != nil || !saved {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if replies := fleetLastReplies(members); !replies[local.ID].Equal(at) {
		t.Fatal("fleet read lost the exact verified reply", replies)
	}
}

func TestFleetUpdatePlainOutputAndDashboardAttention(t *testing.T) {
	now := time.Now().UTC()
	summary := currentFleetSummary(now)
	summary.Problem = "schema hash deadbeef internal-packet"
	member := FleetUpdateMember{Host: update.Host{ID: strings.Repeat("a", 64)}, Build: &release.Build{Version: "v0.1.0"}, Update: summary}
	classifyFleetUpdate(&member, now)
	var out bytes.Buffer
	if err := printFleetUpdates(&out, FleetUpdateStatus{Members: []FleetUpdateMember{member}}, false, presentationMask(nil)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), summary.Problem) || strings.Contains(out.String(), member.Host.ID) || !strings.Contains(out.String(), "needs attention") {
		t.Fatal(out.String())
	}
	out.Reset()
	if err := printFleetUpdates(&out, FleetUpdateStatus{Members: []FleetUpdateMember{member}}, true, presentationMask(nil)); err != nil || !strings.Contains(out.String(), summary.Problem) {
		t.Fatal("details lost diagnostics", out.String(), err)
	}
	rows := DashboardHostUpdateRows([]DashboardHostView{{Host: DashboardHost{ID: "fixture"}, Build: *member.Build, HostUpdate: summary, Connection: StateReachable}}, now)
	if len(rows) != 1 || !rows[0].Attention || strings.Contains(fmt.Sprint(rows), summary.Problem) {
		t.Fatalf("dashboard lost attention or exposed diagnostics: %+v", rows)
	}
}

func TestFleetUpdateStateSnapshotOwnsSummary(t *testing.T) {
	now := time.Now().UTC()
	summary := currentFleetSummary(now)
	state := StateView{HostUpdate: summary}
	clone := state.Clone()
	clone.HostUpdate.Latest.Version = "changed"
	*clone.HostUpdate.Lag = 3
	*clone.HostUpdate.Latest.PublishedAt = now
	if summary.Latest.Version != "v0.1.0" || *summary.Lag != 0 {
		t.Fatal("state clone shared update summary storage")
	}
	if summary.Latest.PublishedAt.Equal(now) {
		t.Fatal("state clone shared publication time storage")
	}
	projected := projectDashboardState(DashboardHost{}, state)
	projected.HostUpdate.Latest.Metadata = "changed"
	if summary.Latest.Metadata == "changed" {
		t.Fatal("dashboard shared source summary storage")
	}
}

func TestInstallationKindsUseActualPathAndBuild(t *testing.T) {
	build := release.Build{Version: "v0.1.0"}
	if updateinstall.InstallationKind("/usr/bin/mesh", build, true) != "package" || updateinstall.InstallationKind("", build, false) != "client" {
		t.Fatal("package or client counted as managed")
	}
	path := filepath.Join(t.TempDir(), "mesh")
	if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if updateinstall.InstallationKind(path, build, true) != "managed" {
		t.Fatal("regular daemon installation not recognized")
	}
	build.Modified = true
	if updateinstall.InstallationKind("/usr/bin/mesh", build, true) != "development" {
		t.Fatal("modified installation counted as managed")
	}
}
