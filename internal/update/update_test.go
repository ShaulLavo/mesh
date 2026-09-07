package update

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
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
	"github.com/shaul/mesh/internal/updateinstall"
)

func testIdentity(t *testing.T) (string, ed25519.PrivateKey) {
	t.Helper()
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(public), key
}

func testManifest() release.Manifest {
	manifest := release.Manifest{Schema: 1, Version: "v0.2.0", Commit: strings.Repeat("a", 40), Compatibility: release.Compatibility{StateReadMin: 5, StateReadMax: 7, StateWrite: 7, WorkerMin: 1, WorkerMax: 1, WorkerWrite: 1, JournalVersion: 1}}
	for _, platform := range []release.Platform{{OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"}, {OS: "darwin", Arch: "arm64"}} {
		manifest.Artifacts = append(manifest.Artifacts, release.Artifact{Platform: platform, Archive: fmt.Sprintf("mesh_%s_%s.tar.gz", platform.OS, platform.Arch), SHA256: strings.Repeat("b", 64), BinarySHA256: strings.Repeat("c", 64)})
		manifest.Compatibility.Transitions = append(manifest.Compatibility.Transitions, release.Transition{Platform: platform, FromDigest: strings.Repeat("d", 64), ToDigest: strings.Repeat("c", 64), Proof: strings.Repeat("e", 64)})
	}
	return manifest
}

func testFleet(t *testing.T, count int) Fleet {
	t.Helper()
	fleet := Fleet{Version: 1, Name: "test", Revision: 1}
	for index := range count {
		id, _ := testIdentity(t)
		fleet.Members = append(fleet.Members, Host{ID: id, Alias: fmt.Sprintf("host-%d", index), Endpoint: fmt.Sprintf("ws://host-%d:7337/mesh", index), Platform: release.Platform{OS: "linux", Arch: "amd64"}})
	}
	return fleet
}

func TestFleetOrdersDependenciesAndCoordinatorLast(t *testing.T) {
	fleet := testFleet(t, 3)
	fleet.Members[0].DependsOn = []string{fleet.Members[1].ID}
	ordered, err := fleet.Order(fleet.Members[2].ID)
	if err != nil {
		t.Fatal(err)
	}
	for index := range ordered {
		if ordered[index].ID != fleet.Members[index].ID {
			t.Fatalf("bad dependency order: %#v", ordered)
		}
	}
	fleet.Members[1].DependsOn = []string{fleet.Members[0].ID}
	if err = fleet.Validate(); err == nil {
		t.Fatal("accepted dependency cycle")
	}
	fleet.Members[1].DependsOn = []string{"absent"}
	if err = fleet.Validate(); err == nil {
		t.Fatal("accepted absent dependency")
	}
}

func TestStoreReopensDeduplicatesAndPinsApproval(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	fleet := testFleet(t, 2)
	manifest := testManifest()
	first, err := store.Start(fleet.Members[0].ID, fleet, manifest)
	if err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Start(fleet.Members[0].ID, fleet, manifest)
	if err != nil || second.ID != first.ID {
		t.Fatalf("retry created another operation: %s %v", second.ID, err)
	}
	if _, err = store.Read("../administrators"); err == nil {
		t.Fatal("accepted path traversal")
	}
	contents, err := os.ReadFile(store.path(first.ID))
	if err != nil {
		t.Fatal(err)
	}
	contents = []byte(strings.Replace(string(contents), manifest.Version, "v0.3.0", 1))
	if err = os.WriteFile(store.path(first.ID), contents, 0600); err != nil { //nolint:gosec // intentionally corrupt this test operation in its isolated store
		t.Fatal(err)
	}
	if _, err = store.Read(first.ID); err == nil {
		t.Fatal("accepted changed release approval")
	}
}

func TestAuthorityRejectsReplayWrongIdentityAndUnauthorizedActors(t *testing.T) {
	id, key := testIdentity(t)
	actor, actorKey := testIdentity(t)
	called := 0
	authority := Authority{StateDir: t.TempDir(), ID: id, Key: key, Handle: func(context.Context, string, json.RawMessage) (any, error) { called++; return "ok", nil }}
	request := Message{Action: "challenge", Actor: actor, Target: id, Nonce: "request-nonce"}
	request.Sign(actorKey)
	if _, err := callAuthority(&authority, request); err == nil {
		t.Fatal("unauthorized actor got challenge")
	}
	if err := Trust(authority.StateDir, actor, true); err != nil {
		t.Fatal(err)
	}
	response, err := callAuthority(&authority, request)
	if err != nil {
		t.Fatal(err)
	}
	if err = response.Verify(id); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(response.Data, &request.Nonce); err != nil {
		t.Fatal(err)
	}
	request.Action = "grant"
	request.Sign(actorKey)
	response, err = callAuthority(&authority, request)
	if err != nil || response.Problem != "" || called != 1 {
		t.Fatalf("authorized request failed: %#v %v", response, err)
	}
	response, err = callAuthority(&authority, request)
	if err != nil || response.Problem == "" || called != 1 {
		t.Fatalf("replayed grant executed: %#v %v", response, err)
	}
	request.Target = actor
	request.Sign(actorKey)
	if _, err = callAuthority(&authority, request); err == nil {
		t.Fatal("accepted another target's signature")
	}
	if err = Trust(authority.StateDir, id, true); err == nil {
		t.Fatal("bootstrap replaced existing administrator policy")
	}
}

func callAuthority(authority *Authority, message Message) (Message, error) {
	data, err := json.Marshal(message)
	if err != nil {
		return Message{}, err
	}
	control, _, err := authority.HandleControl(context.Background(), protocol.Control{Type: ControlType, RequestID: message.Nonce, Update: data})
	if err != nil {
		return Message{}, err
	}
	var response Message
	err = json.Unmarshal(control.Update, &response)
	return response, err
}

type fakeRemote struct {
	info    map[string]Info
	offline map[string]bool
	grants  []string
	stages  int
}

func newRemote(fleet Fleet) *fakeRemote {
	remote := &fakeRemote{info: make(map[string]Info), offline: make(map[string]bool)}
	for _, host := range fleet.Members {
		remote.info[host.ID] = Info{Health: updateinstall.Health{HostID: host.ID, Build: release.Build{Version: "v0.1.0", Commit: strings.Repeat("a", 40), Digest: strings.Repeat("d", 64), Platform: host.Platform, StateVersion: 7, WorkerProtocol: 1, UpdateProtocol: 1}}}
	}
	return remote
}

func (f *fakeRemote) Call(_ context.Context, host Host, action string, input, output any) error {
	if f.offline[host.ID] {
		return errors.New("unreachable")
	}
	info := f.info[host.ID]
	var result any
	switch action {
	case "info":
		result = info
	case "stage":
		request := input.(updateinstall.Request)
		info.Installation = &updateinstall.Status{Schema: 1, Phase: updateinstall.Staged, Request: request}
		result = info.Installation
		f.stages++
	case "grant":
		info.Installation.Phase = updateinstall.Granted
		result = info.Installation
		f.grants = append(f.grants, host.ID)
	case "install-cancel":
		if info.Installation.Phase != updateinstall.Staged {
			return &RemoteError{Problem: "already granted"}
		}
		info.Installation.Phase = updateinstall.Cancelled
		result = info.Installation
	default:
		return fmt.Errorf("unexpected call %s", action)
	}
	f.info[host.ID] = info
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, output)
}

func (f *fakeRemote) complete(id string) {
	info := f.info[id]
	info.Installation.Phase = updateinstall.Committed
	info.Health.Build.Digest = strings.Repeat("c", 64)
	info.Health.Build.Version = "v0.2.0"
	f.info[id] = info
}

func testCoordinator(t *testing.T, count int) (*Coordinator, *fakeRemote, Run) {
	t.Helper()
	fleet := testFleet(t, count)
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.Start(fleet.Members[count-1].ID, fleet, testManifest())
	if err != nil {
		t.Fatal(err)
	}
	remote := newRemote(fleet)
	coordinator := &Coordinator{ID: run.Coordinator, Store: store, Remote: remote, CacheDir: filepath.Join(t.TempDir(), "cache"), Download: func(context.Context, release.Manifest, release.Platform, string) (string, error) {
		return "/cached/mesh", nil
	}}
	return coordinator, remote, run
}

func TestCoordinatorCanaryTwoGrantLimitAndRestart(t *testing.T) {
	coordinator, remote, run := testCoordinator(t, 5)
	ctx := context.Background()
	if err := coordinator.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if len(remote.grants) != 1 || remote.stages != 5 {
		t.Fatalf("expected one canary and all targets staged: grants=%d stages=%d", len(remote.grants), remote.stages)
	}
	remote.complete(remote.grants[0])
	restarted := &Coordinator{ID: coordinator.ID, Store: coordinator.Store, Remote: coordinator.Remote, Release: coordinator.Release, CacheDir: coordinator.CacheDir, Download: coordinator.Download}
	if err := restarted.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if len(remote.grants) != 3 {
		t.Fatalf("expected two further grants, got %d", len(remote.grants))
	}
	if err := restarted.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if len(remote.grants) != 3 {
		t.Fatal("grant limit lost after restart")
	}
	remote.complete(remote.grants[1])
	remote.complete(remote.grants[2])
	if err := restarted.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if remote.grants[len(remote.grants)-1] == run.Coordinator {
		t.Fatal("coordinator activated while another target was unverified")
	}
	remote.complete(remote.grants[3])
	if err := restarted.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if remote.grants[len(remote.grants)-1] != run.Coordinator {
		t.Fatal("coordinator did not activate last")
	}
	remote.complete(run.Coordinator)
	if err := restarted.Step(ctx); err != nil {
		t.Fatal(err)
	}
	final, err := coordinator.Store.Read(run.ID)
	if err != nil || !final.Done() || final.ExitCode() != 0 {
		t.Fatalf("operation not complete: %#v %v", final, err)
	}
}

func TestCoordinatorFailureStopsNewGrantsAndCancelReconcilesIssued(t *testing.T) {
	coordinator, remote, run := testCoordinator(t, 4)
	ctx := context.Background()
	if err := coordinator.Step(ctx); err != nil {
		t.Fatal(err)
	}
	canary := remote.grants[0]
	info := remote.info[canary]
	info.Installation.Phase = updateinstall.RolledBack
	info.Installation.Error = "candidate failed health"
	remote.info[canary] = info
	if err := coordinator.Step(ctx); err != nil {
		t.Fatal(err)
	}
	failed, err := coordinator.Store.Read(run.ID)
	if err != nil || !failed.Stopped || failed.ExitCode() != 1 || len(remote.grants) != 1 {
		t.Fatalf("failure did not stop rollout: %#v %v", failed, err)
	}
	if _, err = coordinator.Store.Cancel(run.ID); err != nil {
		t.Fatal(err)
	}
	if err = coordinator.Step(ctx); err != nil {
		t.Fatal(err)
	}
	for id, info := range remote.info {
		if id != canary && info.Installation.Phase != updateinstall.Cancelled {
			t.Fatalf("staged target %s was not cancelled", id)
		}
	}
}

func TestCoordinatorOfflineHostRemainsPendingAndRetryUsesPinnedRelease(t *testing.T) {
	coordinator, remote, run := testCoordinator(t, 3)
	id := run.Targets[0].Host.ID
	remote.offline[id] = true
	if err := coordinator.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	pending, err := coordinator.Store.Read(run.ID)
	if err != nil || pending.Targets[0].State != Offline || pending.ExitCode() != 2 {
		t.Fatalf("offline host not pending: %#v %v", pending, err)
	}
	remote.offline[id] = false
	if _, err = coordinator.Store.Retry(run.ID); err != nil {
		t.Fatal(err)
	}
	if err = coordinator.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if remote.info[id].Installation.Request.Manifest.Digest() != run.ReleaseDigest {
		t.Fatal("retry changed pinned release")
	}
	if time.Since(pending.CreatedAt) > time.Minute {
		t.Fatal("unexpected operation timestamp")
	}
}
