package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/updateinstall"
	"github.com/shaul/mesh/internal/updatenotice"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAutomaticCanonicalSlotRead(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fleet := testFleet(t, 1)
	run, err := store.Start(fleet.Members[0].ID, fleet, testManifest())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(store.path(run.ID)); err != nil {
		t.Fatal(err)
	}
	run.Origin = &AutomaticOrigin{run.ID, 1, fleet.Digest()}
	record := map[string]any{"schema": 1, "policy": map[string]any{"id": run.ID, "revision": 1, "ownerId": run.Coordinator, "coordinatorId": run.Coordinator, "epoch": 1, "fleet": fleet, "membership": fleet.Digest(), "enabled": false}, "current": run, "attempt": 1}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(store.directory), "automatic.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(run.ID); err != nil {
		t.Fatalf("canonical automatic slot is not readable: %v", err)
	}
	runs, err := store.List()
	if err != nil || len(runs) != 1 {
		t.Fatalf("automatic slot missing from run list: %d %v", len(runs), err)
	}
}

type automaticRemote struct {
	*retryRemote
	consent map[string]RecipientConsent
}

func (r *automaticRemote) Call(ctx context.Context, host Host, action string, input, output any) error {
	if action != "automatic.consent" {
		return r.retryRemote.Call(ctx, host, action, input, output)
	}
	if r.offline[host.ID] {
		return errors.New("offline recipient")
	}
	data, err := json.Marshal(r.consent[host.ID])
	if err != nil {
		return fmt.Errorf("encode automatic fixture response: %w", err)
	}
	if err := json.Unmarshal(data, output); err != nil {
		return fmt.Errorf("decode automatic fixture response: %w", err)
	}
	return nil
}
func automaticFixture(t *testing.T, count int) (*Coordinator, *automaticRemote, VerifiedActor) {
	t.Helper()
	fleet := testFleet(t, count)
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{ID: fleet.Members[count-1].ID, Store: store, SavedFleet: func() (Fleet, error) { return fleet, nil }, Trusted: func(string) bool { return true }, Download: func(context.Context, release.Manifest, release.Platform, string) (string, error) {
		return "/cached/mesh", nil
	}}
	actor := VerifiedActor{id: c.ID, local: true}
	command := AutomaticCommand{ID: mustAutomaticID(t), Action: "enable"}
	status, err := c.ChangeAutomatic(context.Background(), actor, command)
	if err != nil {
		t.Fatal(err)
	}
	remote := &automaticRemote{retryRemote: &retryRemote{fakeRemote: newRemote(fleet)}, consent: map[string]RecipientConsent{}}
	for _, host := range fleet.Members {
		remote.consent[host.ID] = RecipientConsent{1, 1, host.ID, c.ID, fleet.Digest(), status.Record.Policy.Epoch, true}
	}
	c.Remote = remote
	c.CheckedRelease = func(context.Context) (*updatenotice.CheckedRelease, error) {
		m := testManifest()
		return &updatenotice.CheckedRelease{Manifest: m, Digest: m.Digest(), CheckedAt: time.Now()}, nil
	}
	if err := c.refreshAutomatic(context.Background()); err != nil {
		t.Fatal(err)
	}
	return c, remote, actor
}
func mustAutomaticID(t *testing.T) string {
	t.Helper()
	id, err := newAutomaticID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
func currentAutomatic(t *testing.T, c *Coordinator) AutomaticRecord {
	t.Helper()
	status, err := c.Store.ReadAutomatic(context.Background())
	if err != nil || status.Record == nil {
		t.Fatalf("status: %+v %v", status, err)
	}
	return *status.Record
}
func decideAutomatic(t *testing.T, c *Coordinator, actor VerifiedActor, action string) AutomaticCommand {
	t.Helper()
	r := currentAutomatic(t, c)
	command := AutomaticCommand{ID: mustAutomaticID(t), ExpectedRevision: r.Policy.Revision, Action: action}
	if r.Current != nil {
		command.RunID = r.Current.ID
	}
	if _, err := c.ChangeAutomatic(context.Background(), actor, command); err != nil {
		t.Fatal(err)
	}
	return command
}
func TestAutomaticCancellationAndFailureAreSticky(t *testing.T) {
	for _, mode := range []string{"before grant", "after grant", "settled cancellation", "failed canary"} {
		t.Run(mode, func(t *testing.T) { testAutomaticSticky(t, mode) })
	}
}
func testAutomaticSticky(t *testing.T, mode string) {
	c, remote, actor := automaticFixture(t, 3)
	if mode != "before grant" {
		if err := c.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if mode == "failed canary" {
		info := remote.info[remote.grants[0]]
		info.Installation.Phase = updateinstall.RolledBack
		info.Installation.Error = "failed canary"
		remote.info[remote.grants[0]] = info
		if err := c.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
	} else {
		decideAutomatic(t, c, actor, "cancel")
	}
	if mode == "settled cancellation" {
		settleAutomaticFixture(t, c, remote)
	}
	original := currentAutomatic(t, c)
	stages, grants := remote.stages, len(remote.grants)
	for range 2 {
		reopened, err := OpenStore(c.Store.stateDir())
		if err != nil {
			t.Fatal(err)
		}
		c.Store = reopened
		checked := testManifest()
		checked.Version = "v0.3.0"
		if _, err := c.Store.AcceptCheckedRelease(context.Background(), updatenotice.CheckedRelease{Manifest: checked, Digest: checked.Digest(), CheckedAt: time.Now()}, c.automaticScope()); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Store.Start(c.ID, original.Policy.Fleet, original.Current.Release); err != nil {
			t.Fatal(err)
		}
		decideAutomatic(t, c, actor, "disable")
		latest := currentAutomatic(t, c)
		_, err = c.ChangeAutomatic(context.Background(), actor, AutomaticCommand{ID: mustAutomaticID(t), Action: "enable", ExpectedRevision: latest.Policy.Revision})
		if err == nil {
			t.Fatal("ordinary enable cleared stop")
		}
	}
	final := currentAutomatic(t, c)
	if final.Stop == nil || original.Stop == nil || final.Stop.ID != original.Stop.ID || final.Current.ID != original.Current.ID || final.Attempt != original.Attempt || remote.stages != stages || len(remote.grants) != grants {
		t.Fatalf("stop bypassed: %+v", final)
	}
	if _, err := c.Store.Retry(final.Current.ID); err == nil {
		t.Fatal("manual retry bypassed policy owner")
	}
	if _, err := c.Store.Change(final.Current.ID, func(r *Run) error { r.Stopped = false; return nil }); err == nil {
		t.Fatal("progress cleared sticky stop")
	}
}
func settleAutomaticFixture(t *testing.T, c *Coordinator, remote *automaticRemote) {
	t.Helper()
	for _, id := range remote.grants {
		remote.complete(id)
	}
	if err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestAutomaticDisabledContinuationSupersededAndReplayCannotRetryNewFailure(t *testing.T) {
	c, remote, actor := automaticFixture(t, 2)
	if err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := remote.grants[0]
	info := remote.info[first]
	info.Installation.Phase = updateinstall.RolledBack
	remote.info[first] = info
	if err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	decideAutomatic(t, c, actor, "disable")
	retry := decideAutomatic(t, c, actor, "retry")
	r := currentAutomatic(t, c)
	if !activeContinuation(r) || r.Policy.Enabled || r.Attempt != 2 {
		t.Fatalf("missing continuation: %+v", r)
	}
	reopened, err := OpenStore(c.Store.stateDir())
	if err != nil {
		t.Fatal(err)
	}
	c.Store = reopened
	if _, err := c.ChangeAutomatic(context.Background(), actor, retry); err != nil {
		t.Fatalf("lost-reply replay: %v", err)
	}
	if err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(remote.tokens) != 1 || remote.tokens[0] != 1 {
		t.Fatalf("retry token changed: %v", remote.tokens)
	}
	info = remote.info[first]
	info.Installation.Phase = updateinstall.RolledBack
	remote.info[first] = info
	if err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	failed := currentAutomatic(t, c)
	if _, err := c.ChangeAutomatic(context.Background(), actor, retry); err == nil {
		t.Fatal("old retry reauthorized second failure")
	}
	if failed.Stop.Kind == "recovering" || failed.Stop.ID == r.Stop.ID {
		t.Fatal("second failure did not create a fresh stop")
	}
	decideAutomatic(t, c, actor, "retry")
	decideAutomatic(t, c, actor, "disable")
	final := currentAutomatic(t, c)
	if activeContinuation(final) || final.Continuation.State != "superseded" {
		t.Fatal("disable retained continuation")
	}
	_, issued, err := c.reserveAutomatic(context.Background(), final.Current.ID, 0, "stage")
	if err != nil || issued {
		t.Fatalf("disable allowed new stage: %v %v", issued, err)
	}
}
func TestAutomaticCanaryNeedsItsOwnCommittedInstallation(t *testing.T) {
	c, remote, _ := automaticFixture(t, 4)
	r := currentAutomatic(t, c)
	current := r.Current.Targets[0].Host.ID
	info := remote.info[current]
	info.Health.Build.Digest = strings.Repeat("c", 64)
	info.Health.Build.Version = "v0.2.0"
	remote.info[current] = info
	if err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(remote.grants) != 1 || remote.grants[0] == current {
		t.Fatalf("already current unlocked grants: %v", remote.grants)
	}
	r = currentAutomatic(t, c)
	if r.Canary == nil || r.Canary.State == "verified" {
		t.Fatalf("false canary proof: %+v", r.Canary)
	}
	remote.complete(remote.grants[0])
	if err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	r = currentAutomatic(t, c)
	if r.Canary.State != "verified" {
		t.Fatalf("committed own canary not verified: %+v", r.Canary)
	}
}
func TestAutomaticAcknowledgementRetiresHistoryWithoutRetryAuthority(t *testing.T) {
	c, remote, actor := automaticFixture(t, 2)
	decideAutomatic(t, c, actor, "cancel")
	settleAutomaticFixture(t, c, remote)
	old := currentAutomatic(t, c).Current.ID
	decideAutomatic(t, c, actor, "acknowledge-stop")
	r := currentAutomatic(t, c)
	if r.Current != nil || r.Stop != nil || r.Policy.Enabled {
		t.Fatalf("acknowledgement did not retire disabled slot: %+v", r)
	}
	if _, err := c.Store.Read(old); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Store.Retry(old); err == nil {
		t.Fatal("history regained execution authority")
	}
	decideAutomatic(t, c, actor, "enable")
}
func TestAutomaticManualOverlapAndOwnerCAS(t *testing.T) {
	c, _, actor := automaticFixture(t, 2)
	r := currentAutomatic(t, c)
	if _, err := c.Store.Start(c.ID, r.Policy.Fleet, testManifest()); err != nil {
		t.Fatal(err)
	}
	changed := testManifest()
	changed.Version = "v0.3.0"
	if _, err := c.Store.Start(c.ID, r.Policy.Fleet, changed); err == nil {
		t.Fatal("overlapping manual approval accepted")
	}
	other, _ := testIdentity(t)
	if _, err := c.ChangeAutomatic(context.Background(), VerifiedActor{id: other}, AutomaticCommand{ID: mustAutomaticID(t), ExpectedRevision: r.Policy.Revision, Action: "disable"}); err == nil {
		t.Fatal("another administrator took policy ownership")
	}
	cmd := decideAutomatic(t, c, actor, "disable")
	if _, err := c.ChangeAutomatic(context.Background(), actor, cmd); err != nil {
		t.Fatal(err)
	}
	cmd.ID = mustAutomaticID(t)
	if _, err := c.ChangeAutomatic(context.Background(), actor, cmd); err == nil {
		t.Fatal("different stale command won CAS")
	}
}

func TestAutomaticDisabledHealthyRetryCanReserveWholePinnedRun(t *testing.T) {
	c, _, actor := automaticFixture(t, 2)
	decideAutomatic(t, c, actor, "disable")
	decideAutomatic(t, c, actor, "retry")
	r := currentAutomatic(t, c)
	_, issued, err := c.reserveAutomatic(context.Background(), r.Current.ID, 0, "stage")
	if err != nil || !issued {
		t.Fatalf("explicit healthy disabled continuation was lost: issued=%v error=%v", issued, err)
	}
}
func TestAutomaticWithdrawnOwnerStillObservesPreviouslyAcceptedWork(t *testing.T) {
	c, remote, _ := automaticFixture(t, 2)
	if err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	canary := remote.grants[0]
	remote.complete(canary)
	c.Trusted = func(string) bool { return false }
	_ = c.Step(context.Background())
	r := currentAutomatic(t, c)
	for _, target := range r.Current.Targets {
		if target.Host.ID == canary && target.State != Updated {
			t.Fatal("withdrawal prevented observation of already accepted work")
		}
	}
	if r.Stop == nil || r.Stop.Kind != "authority_withdrawn" || len(remote.grants) != 1 {
		t.Fatalf("withdrawal did not freeze authority: %+v", r)
	}
}

func TestAutomaticCancelledFailedCanaryCanBeAcknowledgedAfterSettlement(t *testing.T) {
	c, remote, actor := automaticFixture(t, 2)
	if err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	info := remote.info[remote.grants[0]]
	info.Installation.Phase = updateinstall.RolledBack
	remote.info[remote.grants[0]] = info
	if err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	decideAutomatic(t, c, actor, "cancel")
	if err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	decideAutomatic(t, c, actor, "acknowledge-stop")
}
func TestAutomaticCancelledJoinRemainsObservedUntilExternalOwnerSettles(t *testing.T) {
	c, remote, actor := automaticFixture(t, 2)
	r := currentAutomatic(t, c)
	host := r.Current.Targets[0].Host.ID
	info := remote.info[host]
	info.Installation = &updateinstall.Status{Phase: updateinstall.Staged, Request: updateinstall.Request{ID: mustAutomaticID(t), TargetID: host, Generation: 1, Manifest: testManifest()}}
	remote.info[host] = info
	if err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(remote.grants) != 0 {
		t.Fatal("joined installation provided canary proof")
	}
	decideAutomatic(t, c, actor, "cancel")
	if err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	remote.complete(host)
	if err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	r = currentAutomatic(t, c)
	if !automaticSettled(*r.Current) {
		t.Fatal("settled external installation remained unresolved")
	}
	decideAutomatic(t, c, actor, "acknowledge-stop")
}

func TestAutomaticAuthorityPreservesLegacyRelaysAndBindsNewActions(t *testing.T) {
	serverID, serverKey := testIdentity(t)
	actorID, actorKey := testIdentity(t)
	relayID, relayKey := testIdentity(t)
	state := t.TempDir()
	if err := Trust(t.Context(), state, actorID, false); err != nil {
		t.Fatal(err)
	}
	authority := &Authority{StateDir: state, ID: serverID, Key: serverKey, Handle: func(_ context.Context, actor VerifiedActor, _ string, _ json.RawMessage) (any, error) {
		return actor.ID(), nil
	}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = transport.ServeWithOptions(w, r, transport.ServeOptions{Auth: &transport.Authentication{Key: serverKey, Authorize: func(id string) bool { return id == relayID }}}, func(ctx context.Context, conn transport.Conn) error {
			return serveAutomaticRelay(ctx, conn, authority)

		})
	}))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := transport.DialOnce(ctx, server.URL, transport.DialOptions{Auth: &transport.Authentication{Key: relayKey, ExpectedIdentity: serverID}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	client := Client{ID: actorID, Key: actorKey}
	host := Host{ID: serverID, Endpoint: "ws" + strings.TrimPrefix(server.URL, "http")}
	for _, action := range []string{"info", "automatic.status", "configuration.status", "configuration.observe"} {
		request := Message{Actor: actorID, Target: serverID, Action: "challenge", Nonce: mustAutomaticID(t)}
		challenge, err := client.exchange(conn, host, request)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(challenge.Data, &request.Nonce); err != nil {
			t.Fatal(err)
		}
		request.Action = action
		response, err := client.exchange(conn, host, request)
		if action == "info" {
			assertAutomaticRelaySigner(t, response, err, actorID)
			continue
		}
		var denied *RemoteError
		code := ProblemCode("automatic.conflict")
		if configurationAction(action) {
			code = "configuration.denied"
		}
		if !errors.As(err, &denied) || denied.Code != code {
			t.Fatalf("new %s boundary accepted relay: %v", action, err)
		}
	}
}
func TestAutomaticPublicationBarrierBlocksDispatchAndSettlesReplay(t *testing.T) {
	c, remote, actor := automaticFixture(t, 2)
	r := currentAutomatic(t, c)
	original := syncOperationDirectory
	t.Cleanup(func() { syncOperationDirectory = original })
	injected := errors.New("automatic directory barrier")
	syncOperationDirectory = func(path string) error {
		if path == filepath.Dir(c.Store.directory) {
			return injected
		}
		return original(path)
	}
	cmd := AutomaticCommand{ID: mustAutomaticID(t), Action: "disable", ExpectedRevision: r.Policy.Revision}
	if _, err := c.ChangeAutomatic(context.Background(), actor, cmd); !errors.Is(err, injected) {
		t.Fatalf("authority publication not faulted: %v", err)
	}
	if err := c.Step(context.Background()); !errors.Is(err, injected) {
		t.Fatalf("unsettled authority used: %v", err)
	}
	if remote.stages != 0 || len(remote.grants) != 0 {
		t.Fatal("unconfirmed authority dispatched work")
	}
	syncOperationDirectory = original
	status, err := c.ChangeAutomatic(context.Background(), actor, cmd)
	if err != nil || status.Record.Policy.Revision != r.Policy.Revision+1 || status.Record.Policy.Enabled {
		t.Fatalf("publication replay changed decision: %+v %v", status, err)
	}
}

func TestAutomaticNoCurrentStopAcknowledgementAndOwnershipRecovery(t *testing.T) {
	for _, kind := range []string{"membership", "owner"} {
		t.Run(kind, func(t *testing.T) { testAutomaticNoCurrentStop(t, kind) })
	}
}
func testAutomaticNoCurrentStop(t *testing.T, kind string) {
	fleet := testFleet(t, 1)
	coordinator := fleet.Members[0].ID
	owner, _ := testIdentity(t)
	ownerTrusted := true
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{ID: coordinator, Store: store, SavedFleet: func() (Fleet, error) { return fleet, nil }, Trusted: func(id string) bool { return id == coordinator || id == owner && ownerTrusted }}
	actor := VerifiedActor{id: owner}
	if _, err := c.ChangeAutomatic(context.Background(), actor, AutomaticCommand{ID: mustAutomaticID(t), Action: automaticEnable}); err != nil {
		t.Fatal(err)
	}
	original := currentAutomatic(t, c)
	if kind == "membership" {
		fleet.Revision++
	} else {
		ownerTrusted = false
	}
	if err := c.checkAutomaticScope(context.Background()); !stoppedAutomaticError(err) {
		t.Fatalf("pre-run authority change did not stop: %v", err)
	}
	reopened, err := OpenStore(c.Store.stateDir())
	if err != nil {
		t.Fatal(err)
	}
	c.Store = reopened
	stopped := currentAutomatic(t, c)
	if stopped.Current != nil || stopped.Stop == nil {
		t.Fatal("pre-run stop fabricated or lost a run")
	}
	local := VerifiedActor{id: coordinator, local: true}
	decideAutomatic(t, c, local, "disable")
	r := currentAutomatic(t, c)
	ack := AutomaticCommand{ID: mustAutomaticID(t), Action: "acknowledge-stop", RunID: r.Policy.ID, ExpectedRevision: r.Policy.Revision}
	if _, err := c.ChangeAutomatic(context.Background(), VerifiedActor{id: coordinator}, ack); err == nil {
		t.Fatal("remote coordinator silently recovered another owner")
	}
	if kind == "owner" {
		if _, err := c.ChangeAutomatic(context.Background(), actor, ack); err == nil {
			t.Fatal("withdrawn remote owner acknowledged stop")
		}
	}
	status, err := c.ChangeAutomatic(context.Background(), local, ack)
	if err != nil {
		t.Fatal(err)
	}
	if status.Record.Policy.Enabled || status.Record.Stop != nil || status.Record.Current != nil {
		t.Fatalf("acknowledged pending policy is active: %+v", status)
	}
	reopened, err = OpenStore(c.Store.stateDir())
	if err != nil {
		t.Fatal(err)
	}
	c.Store = reopened
	if _, err := c.ChangeAutomatic(context.Background(), local, ack); err != nil {
		t.Fatalf("ack replay: %v", err)
	}
	if err := c.refreshAutomatic(context.Background()); err != nil {
		t.Fatal(err)
	}
	inactive := currentAutomatic(t, c)
	if inactive.Stop != nil || inactive.Policy.Revision != r.Policy.Revision+1 {
		t.Fatal("polling recreated acknowledged authority")
	}
	next := AutomaticCommand{ID: mustAutomaticID(t), Action: automaticEnable, TakeOwnership: true, ExpectedRevision: inactive.Policy.Revision}
	enabled, err := c.ChangeAutomatic(context.Background(), local, next)
	if err != nil {
		t.Fatal(err)
	}
	if enabled.Record.Policy.OwnerID != coordinator || enabled.Record.Policy.Epoch != original.Policy.Epoch+1 || len(enabled.Record.Enrollment) != 0 {
		t.Fatal("local recovery reused old ownership or consent")
	}
	if _, err := c.ChangeAutomatic(context.Background(), local, ack); err == nil {
		t.Fatal("stale ack cleared a new enrollment")
	}
}

func TestAutomaticResponseReplacesWholeSnapshot(t *testing.T) {
	old := AutomaticStatus{Problem: "old stop", Record: &AutomaticRecord{Stop: &AutomaticStop{Kind: "failed"}, Current: &Run{Targets: []Target{{Grant: true, Problem: "old failure"}}}}}
	if err := decodeAutomaticOutput([]byte(`{"record":{"schema":1,"policy":{},"attempt":0},"revision":3,"state":"disabled"}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.Problem != "" || old.Record.Current != nil || old.Record.Stop != nil {
		t.Fatalf("omitted fields retained stale authority: %+v", old)
	}
}

func serveAutomaticRelay(ctx context.Context, conn transport.Conn, authority *Authority) error {
	for range 8 {
		if err := exchangeDownloadPlan(ctx, conn, authority); err != nil {
			return err
		}
	}
	return nil
}
func assertAutomaticRelaySigner(t *testing.T, response Message, err error, expected string) {
	t.Helper()
	var id string
	if err != nil || json.Unmarshal(response.Data, &id) != nil || id != expected {
		t.Fatalf("legacy signer lost: %s %v", id, err)
	}
}

func TestAutomaticAcknowledgedRestoredMembershipRequiresFreshConsent(t *testing.T) {
	fleet := testFleet(t, 1)
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{ID: fleet.Members[0].ID, Store: store, SavedFleet: func() (Fleet, error) { return fleet, nil }, Trusted: func(string) bool { return true }}
	actor := VerifiedActor{id: c.ID, local: true}
	if _, err := c.ChangeAutomatic(context.Background(), actor, AutomaticCommand{ID: mustAutomaticID(t), Action: automaticEnable}); err != nil {
		t.Fatal(err)
	}
	fleet.Revision++
	_ = c.checkAutomaticScope(context.Background())
	fleet.Revision--
	decideAutomatic(t, c, actor, "disable")
	r := currentAutomatic(t, c)
	if _, err := c.ChangeAutomatic(context.Background(), actor, AutomaticCommand{ID: mustAutomaticID(t), Action: "acknowledge-stop", RunID: r.Policy.ID, ExpectedRevision: r.Policy.Revision}); err != nil {
		t.Fatal(err)
	}
	decideAutomatic(t, c, actor, automaticEnable)
	if currentAutomatic(t, c).Policy.Epoch != 2 {
		t.Fatal("restored membership reused the withdrawn enrollment epoch")
	}
}

func TestAutomaticPolicyAcknowledgementSerializesWithAdmissionAndReplay(t *testing.T) {
	fleet := testFleet(t, 1)
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{ID: fleet.Members[0].ID, Store: store, SavedFleet: func() (Fleet, error) { return fleet, nil }, Trusted: func(string) bool { return true }}
	actor := VerifiedActor{id: c.ID, local: true}
	if _, err := c.ChangeAutomatic(context.Background(), actor, AutomaticCommand{ID: mustAutomaticID(t), Action: automaticEnable}); err != nil {
		t.Fatal(err)
	}
	fleet.Revision++
	_ = c.checkAutomaticScope(context.Background())
	fleet.Revision--
	decideAutomatic(t, c, actor, "disable")
	r := currentAutomatic(t, c)
	ack := AutomaticCommand{ID: mustAutomaticID(t), Action: "acknowledge-stop", RunID: r.Policy.ID, ExpectedRevision: r.Policy.Revision}
	start := make(chan struct{})
	results := make(chan error, 3)
	go func() { <-start; _, err := c.ChangeAutomatic(context.Background(), actor, ack); results <- err }()
	go func() { <-start; _, err := c.ChangeAutomatic(context.Background(), actor, ack); results <- err }()
	go func() {
		<-start
		manifest := testManifest()
		_, err := store.AcceptCheckedRelease(context.Background(), updatenotice.CheckedRelease{Manifest: manifest, Digest: manifest.Digest(), CheckedAt: time.Now()}, c.automaticScope())
		results <- err
	}()
	close(start)
	for range 3 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	final := currentAutomatic(t, c)
	if final.Current != nil || final.Policy.Enabled || final.Stop != nil || final.Policy.Revision != r.Policy.Revision+1 {
		t.Fatalf("admission or replay escaped acknowledgement CAS: %+v", final)
	}
	if final.LastAcknowledgement == nil || final.LastAcknowledgement.Stop.ID != r.Stop.ID || final.LastAcknowledgement.ActorID != actor.ID() || final.LastAcknowledgement.ResultRevision != final.Policy.Revision {
		t.Fatal("acknowledgement lost its exact stop receipt")
	}
}

type automaticCallFunc func(context.Context, Host, string, any, any) error

func (f automaticCallFunc) Call(ctx context.Context, host Host, action string, input, output any) error {
	return f(ctx, host, action, input, output)
}
func TestAutomaticWithdrawnOwnerObservesBeyondUnacceptedGrant(t *testing.T) {
	c, remote, _ := automaticFixture(t, 4)
	if err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	remote.complete(remote.grants[0])
	run := currentAutomatic(t, c).Current
	waiting := run.Targets[1].Host.ID
	c.Remote = automaticCallFunc(func(ctx context.Context, host Host, action string, input, output any) error {
		if host.ID == waiting && action == "grant" {
			return errors.New("grant was not delivered")
		}
		return remote.Call(ctx, host, action, input, output)
	})
	if err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	completed := remote.grants[len(remote.grants)-1]
	remote.complete(completed)
	if _, err := c.Store.Change(run.ID, func(r *Run) error {
		for i := range r.Targets {
			r.Targets[i].RetryAt = time.Time{}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	c.Trusted = func(string) bool { return false }
	_ = c.Step(context.Background())
	r := currentAutomatic(t, c)
	for _, target := range r.Current.Targets {
		if target.Host.ID == completed && target.State != Updated {
			t.Fatal("unaccepted earlier grant blocked observation of accepted later work")
		}
	}
}

func TestAutomaticAbsenceNeverConsumesChecksOrCreatesWork(t *testing.T) {
	fleet := testFleet(t, 1)
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := Trust(t.Context(), store.stateDir(), fleet.Members[0].ID, false); err != nil {
		t.Fatal(err)
	}
	calls := 0
	c := &Coordinator{ID: fleet.Members[0].ID, Store: store, SavedFleet: func() (Fleet, error) { calls++; return fleet, nil }, CheckedRelease: func(context.Context) (*updatenotice.CheckedRelease, error) { calls++; return nil, nil }, Remote: automaticCallFunc(func(context.Context, Host, string, any, any) error {
		calls++
		return errors.New("unexpected remote call")
	})}
	for range 3 {
		if err := c.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := store.List()
	if err != nil || len(runs) != 0 || calls != 0 {
		t.Fatalf("absence scheduled automatic work: runs=%d calls=%d error=%v", len(runs), calls, err)
	}
}

type automaticDispatchBarrier struct {
	remote  Caller
	action  string
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *automaticDispatchBarrier) Call(ctx context.Context, host Host, action string, input, output any) error {
	if action == b.action {
		b.once.Do(func() { close(b.entered) })
		select {
		case <-b.release:
		case <-ctx.Done():
			return fmt.Errorf("dispatch barrier: %w", ctx.Err())
		}
	}
	if err := b.remote.Call(ctx, host, action, input, output); err != nil {
		return fmt.Errorf("dispatch fixture: %w", err)
	}
	return nil
}
func TestAutomaticDisableAndCancelFenceFutureIntentionsWithoutRetractingIssuedWork(t *testing.T) {
	for _, action := range []string{"disable", "cancel"} {
		t.Run(action, func(t *testing.T) { testAutomaticDispatchBoundary(t, action) })
	}
}
func testAutomaticDispatchBoundary(t *testing.T, decision string) {
	c, remote, actor := automaticFixture(t, 3)
	barrier := &automaticDispatchBarrier{remote: c.Remote, action: "stage", entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() {
		select {
		case <-barrier.release:
		default:
			close(barrier.release)
		}
	})
	c.Remote = barrier
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Step(ctx) }()
	select {
	case <-barrier.entered:
	case <-ctx.Done():
		t.Fatal("stage did not reserve an intention")
	}
	decideAutomatic(t, c, actor, decision)
	close(barrier.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	r := currentAutomatic(t, c)
	if remote.stages != 1 || len(remote.grants) != 0 || r.Policy.Enabled {
		t.Fatalf("decision changed dispatch boundary: stages=%d grants=%v policy=%+v", remote.stages, remote.grants, r.Policy)
	}
	if decision == "cancel" && (r.Stop == nil || r.Stop.Kind != "cancelled") {
		t.Fatal("in-flight staging erased cancellation")
	}
}
func TestAutomaticInitialOwnerCASHasOneWinner(t *testing.T) {
	fleet := testFleet(t, 1)
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{ID: fleet.Members[0].ID, Store: store, SavedFleet: func() (Fleet, error) { return fleet, nil }, Trusted: func(string) bool { return true }}
	first, _ := testIdentity(t)
	second, _ := testIdentity(t)
	results := make(chan error, 2)
	start := make(chan struct{})
	a, b := AutomaticCommand{ID: mustAutomaticID(t), Action: automaticEnable}, AutomaticCommand{ID: mustAutomaticID(t), Action: automaticEnable}
	go func() {
		<-start
		_, err := c.ChangeAutomatic(context.Background(), VerifiedActor{id: first}, a)
		results <- err
	}()
	go func() {
		<-start
		_, err := c.ChangeAutomatic(context.Background(), VerifiedActor{id: second}, b)
		results <- err
	}()
	close(start)
	successes := 0
	for range 2 {
		if err := <-results; err == nil {
			successes++
		}
	}
	if successes != 1 || currentAutomatic(t, c).Policy.Revision != 1 {
		t.Fatal("concurrent initial owners did not serialize")
	}
}

func TestAutomaticActiveRunDoesNotDecodeLaterCheckedRelease(t *testing.T) {
	c, remote, _ := automaticFixture(t, 2)
	c.lastAutomaticCheck = time.Time{}
	c.CheckedRelease = func(context.Context) (*updatenotice.CheckedRelease, error) {
		return nil, errors.New("later checked cache is damaged")
	}
	_ = c.Step(context.Background())
	if len(remote.grants) != 1 {
		t.Fatal("later cache damage prevented already pinned run execution")
	}
}

func TestAutomaticManualApprovalFirstBlocksAdmissionWithoutReadingFeed(t *testing.T) {
	c, remote, manual := testCoordinator(t, 2)
	c.SavedFleet = func() (Fleet, error) { return manual.Fleet, nil }
	c.Trusted = func(string) bool { return true }
	actor := VerifiedActor{id: c.ID, local: true}
	status, err := c.ChangeAutomatic(context.Background(), actor, AutomaticCommand{ID: mustAutomaticID(t), Action: automaticEnable})
	if err != nil {
		t.Fatal(err)
	}
	caller := &automaticRemote{retryRemote: &retryRemote{fakeRemote: remote}, consent: map[string]RecipientConsent{}}
	for _, host := range manual.Fleet.Members {
		caller.consent[host.ID] = RecipientConsent{1, 1, host.ID, c.ID, manual.Membership, status.Record.Policy.Epoch, true}
	}
	c.Remote = caller
	c.CheckedRelease = func(context.Context) (*updatenotice.CheckedRelease, error) {
		t.Fatal("manual overlap read the release feed cache")
		return nil, nil
	}
	if err := c.refreshAutomatic(context.Background()); err != nil {
		t.Fatal(err)
	}
	if currentAutomatic(t, c).Current != nil {
		t.Fatal("manual-first overlap created a second operation")
	}
}

func TestAutomaticDevelopmentBuildRequiresManualReview(t *testing.T) {
	c, remote, _ := automaticFixture(t, 1)
	host := currentAutomatic(t, c).Current.Targets[0].Host.ID
	info := remote.info[host]
	info.Health.Build.Version = "dev"
	info.Health.Build.Modified = true
	remote.info[host] = info
	if err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(remote.grants) != 0 || currentAutomatic(t, c).Stop == nil {
		t.Fatal("automatic development build entered the installer")
	}
}
