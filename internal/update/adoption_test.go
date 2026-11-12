package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/updatebootstrap"
	"github.com/shaul/mesh/internal/updateinstall"
)

type adoptionFixture struct {
	base       *hostFixture
	supervisor *AdoptionSupervisor
	start      AdoptionStart
	id         string
	controls   []string
	helperPID  int
	promotions int
	refuse     string
	peers      map[string]HostAdoption
	peerError  error
	files      []string
}

func newAdoptionFixture(t *testing.T) *adoptionFixture {
	t.Helper()
	f := &adoptionFixture{base: newHostFixture(t), helperPID: os.Getpid(), peers: make(map[string]HostAdoption)}
	b := f.base
	host, _, err := identity.LoadOrCreate(b.store.stateDir())
	if err != nil {
		t.Fatal(err)
	}
	f.id = host.ID
	b.settings.Probe = f.probe
	b.settings.Service = f
	b.engine, err = updateinstall.New(b.settings)
	if err != nil {
		t.Fatal(err)
	}
	request := updateinstall.Request{ID: "previous", TargetID: f.id, Generation: 1, Current: b.current, Manifest: b.manifests["v0.1.2"]}
	if _, err := b.engine.Stage(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if _, err := b.engine.Grant(t.Context(), request.ID, request.Generation); err != nil {
		t.Fatal(err)
	}
	if status, err := b.engine.Run(t.Context()); err != nil || status.Phase != updateinstall.Committed {
		t.Fatalf("previous installation: %+v %v", status, err)
	}
	if _, err := b.store.SetHostPolicy(t.Context(), 1, HostPolicy{Mode: HostManual}); err != nil {
		t.Fatal(err)
	}
	side := filepath.Join(t.TempDir(), "side-mesh")
	if err := os.WriteFile(side, b.files["v0.1.3/image"], 0600); err != nil {
		t.Fatal(err)
	}
	f.files = []string{filepath.Join(b.store.stateDir(), "authorized_keys"), filepath.Join(b.store.stateDir(), "updates", "administrators.json")}
	f.start = AdoptionStart{ID: strings.Repeat("a", 32), Version: "v0.1.3", Expected: f.id, Policy: HostPolicy{Mode: HostAutomatic}, Files: f.files}
	f.supervisor = &AdoptionSupervisor{Store: b.store, Documents: b.documents, Side: side, Build: b.builds["v0.1.3"], Inspect: f.inspect, Caller: f,
		Installation: f.installation,
		Service:      func(context.Context, updateinstall.Settings) (string, error) { return strings.Repeat("d", 64), nil },
		Now:          func() time.Time { return b.now }, Promote: f.promote, Helper: f.helper, Engine: func(Adoption) (*updateinstall.Engine, error) { return b.engine, nil },
		Evidence: func(_ context.Context, peer Host) (HostAdoption, error) { return f.peers[peer.ID], f.peerError }}
	return f
}

func (f *adoptionFixture) installation(ctx context.Context, stateDir string) (updatebootstrap.Installation, error) {
	observed, err := f.inspect(ctx, stateDir)
	return updatebootstrap.Installation{Observation: observed, Settings: f.base.engine.Settings(), Journal: true}, err
}

func (f *adoptionFixture) probe(ctx context.Context) (updateinstall.Health, error) {
	health, err := f.base.probe(ctx)
	health.HostID, health.BootID = f.id, "fixture-boot"
	return health, err
}

func (f *adoptionFixture) inspect(ctx context.Context, _ string) (updatebootstrap.Observation, error) {
	health, err := f.probe(ctx)
	return updatebootstrap.Observation{Executable: f.base.settings.Executable, Health: health}, err
}

func (f *adoptionFixture) promote(ctx context.Context, operation Adoption, pinned AdoptionRelease) (updateinstall.HelperInstallation, error) {
	policy, err := f.base.store.HostPolicy(ctx)
	if err != nil || !reflect.DeepEqual(policy, operation.Policy) {
		return updateinstall.HelperInstallation{}, errors.New("policy missing before helper promotion")
	}
	f.promotions++
	return updateinstall.HelperInstallation{Executable: f.supervisor.Side, Digest: f.supervisor.Build.Digest, ServicePath: "/fixture/helper.service"}, nil
}

func (f *adoptionFixture) helper(context.Context, Adoption) (int, error) { return f.helperPID, nil }

func (f *adoptionFixture) Stop(ctx context.Context) error { return f.base.Stop(ctx) }

func (f *adoptionFixture) Start(ctx context.Context) error {
	image, err := os.ReadFile(f.base.settings.Executable)
	if err != nil {
		return fmt.Errorf("read candidate at first service start: %w", err)
	}
	if discoveryDigest(image) == f.base.builds["v0.1.3"].Digest {
		policy, err := f.base.store.HostPolicy(ctx)
		if err != nil || policy.Mode != HostAutomatic {
			return errors.New("candidate first service start preceded automatic policy seeding")
		}
	}
	return f.base.Start(ctx)
}

func (f *adoptionFixture) Call(ctx context.Context, host Host, action string, input, output any) error {
	if !IsLocal(host) || host.ID != f.id {
		return errors.New("adoption attempted remote update control")
	}
	f.controls = append(f.controls, action)
	if action == f.refuse {
		return errors.New("administrator refused local control")
	}
	switch action {
	case "info":
		health, err := f.probe(ctx)
		*output.(*Info) = Info{Health: health}
		return err
	case "stage":
		status, err := f.base.engine.Stage(ctx, input.(updateinstall.Request))
		*output.(*updateinstall.Status) = status
		return hostError(err)
	case "grant":
		op := input.(Operation)
		status, err := f.base.engine.Grant(ctx, op.ID, op.Generation)
		*output.(*updateinstall.Status) = status
		return hostError(err)
	default:
		return fmt.Errorf("unexpected adoption action %s", action)
	}
}

func (f *adoptionFixture) state(t *testing.T) (HostAdoption, Adoption) {
	t.Helper()
	envelope, operation, err := f.base.store.AdoptionStatus(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return envelope, operation
}

func (f *adoptionFixture) resume(t *testing.T) {
	t.Helper()
	// Reconstruct the supervisor and store between every phase, as a new helper
	// process does. No in-memory progress can make a restart test pass.
	restarted := *f.supervisor
	var err error
	restarted.Store, err = OpenStore(f.base.store.stateDir())
	if err != nil {
		t.Fatal(err)
	}
	f.supervisor = &restarted
	if err := restarted.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestAdoptionResumesAndConfirmsPinnedCommit(t *testing.T) {
	f := newAdoptionFixture(t)
	if err := f.supervisor.Start(t.Context(), f.start); err != nil {
		t.Fatal(err)
	}
	envelope, operation := f.state(t)
	if envelope.Confirmed || operation.Phase != AdoptionWaiting || f.promotions != 1 || len(f.controls) != 0 {
		t.Fatalf("initial adoption: %+v controls %v", operation, f.controls)
	}
	f.resume(t)
	f.resume(t)
	envelope, operation = f.state(t)
	if envelope.Confirmed || operation.Phase != AdoptionConfirming {
		t.Fatal("activation released the confirmation fence")
	}
	f.base.roundTrip()
	f.resume(t)
	f.resume(t)
	envelope, operation = f.state(t)
	if !envelope.Confirmed || operation.Phase != AdoptionComplete || envelope.Adopted.Metadata != operation.Pin || envelope.Adopted.Origin != "adoption" {
		t.Fatalf("confirmation missing pinned milestone: %+v", envelope)
	}
	entries, more, err := f.base.store.HostLedger(t.Context(), 0, 64)
	if err != nil || more || len(entries) != 1 || entries[0].Origin != "adoption" {
		t.Fatalf("duplicate or wrong commit: %+v %v", entries, err)
	}
	if !reflect.DeepEqual(f.controls, []string{"info", "stage", "grant"}) {
		t.Fatalf("controls: %v", f.controls)
	}
}

func TestAdoptionRejectsChangedProofOrImageBeforeControls(t *testing.T) {
	for _, kind := range []string{"signature", "manifest", "side", "rollback", "worker", "source"} {
		t.Run(kind, func(t *testing.T) { testAdoptionMismatch(t, kind) })
	}
}

func testAdoptionMismatch(t *testing.T, kind string) {
	f := newAdoptionFixture(t)
	switch kind {
	case "signature":
		f.base.files["v0.1.3/mesh-update.sig.json"] = []byte(`{}`)
	case "manifest":
		f.base.files["v0.1.3/"+release.ManifestAssetName] = append(f.base.files["v0.1.3/"+release.ManifestAssetName], ' ')
	case "side":
		f.supervisor.Build.Digest = strings.Repeat("f", 64)
	case "rollback":
		removeAdoptionRollbackProofs(f)
	case "worker":
		build := *f.base.workers[0].Build
		build.Digest = strings.Repeat("e", 64)
		f.base.workers[0].Build = &build
	case "source":
		f.start.Expected = updateTestID(t)
	}
	if err := f.supervisor.Start(t.Context(), f.start); err == nil {
		t.Fatal("unverified input accepted")
	}
	if len(f.controls) != 0 || f.promotions != 0 {
		t.Fatal("unverified input reached update control or helper promotion")
	}
	if _, err := os.Stat(filepath.Join(f.base.store.stateDir(), "updates", "host-adoption.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unverified input left an adoption record")
	}
}

func removeAdoptionRollbackProofs(f *adoptionFixture) {
	for name := range f.base.files {
		if strings.HasSuffix(name, ".rollback.json") {
			f.base.files[name] = []byte(`{}`)
		}
	}
}

func updateTestID(t *testing.T) string {
	t.Helper()
	host, _, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return host.ID
}

func TestAdoptionPeersAndNoticeHoldLastHost(t *testing.T) {
	f := newAdoptionFixture(t)
	peer := Host{ID: updateTestID(t), Endpoint: "wss://fixture.invalid/mesh", Platform: release.CurrentPlatform()}
	f.start.Require = []Host{peer}
	marker := filepath.Join(t.TempDir(), "notice")
	f.start.Notice = &AdoptionNotice{Command: []string{"touch", marker}, Lead: 30 * time.Minute}
	if err := f.supervisor.Start(t.Context(), f.start); err != nil {
		t.Fatal(err)
	}
	if err := f.supervisor.Resume(t.Context()); err == nil {
		t.Fatal("missing peer milestones allowed progress")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) || len(f.controls) != 0 {
		t.Fatal("notice or activation ran before predecessor evidence")
	}
	_, operation := f.state(t)
	f.peers[peer.ID] = completeAdoptionEvidence(operation.ID, operation.Pin, f.base.now)
	f.base.now = f.base.now.Add(time.Minute)
	f.resume(t)
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("notice was not executed")
	}
	f.resume(t)
	if len(f.controls) != 0 {
		t.Fatal("activation ran before notice lead time")
	}
	f.base.now = f.base.now.Add(30 * time.Minute)
	f.peerError = errors.New("peer refused evidence")
	if err := f.supervisor.Resume(t.Context()); err == nil {
		t.Fatal("lead time waived the predecessor recheck")
	}
	if len(f.controls) != 0 {
		t.Fatal("refusal reached activation")
	}
	f.peerError = nil
	f.base.now = f.base.now.Add(2 * time.Minute)
	f.resume(t)
	f.resume(t)
	if !reflect.DeepEqual(f.controls, []string{"info", "stage", "grant"}) {
		t.Fatalf("activation did not run after notice and recheck: %v", f.controls)
	}
}

func completeAdoptionEvidence(operation, pin string, now time.Time) HostAdoption {
	adopted := &LedgerEntry{Request: operation, Generation: 1, Version: "v0.1.3", Metadata: pin, Binary: strings.Repeat("a", 64), Origin: "adoption", CommittedAt: now, Workers: 1, WorkersPreserved: 1}
	later := &LedgerEntry{Request: "unattended", Generation: 2, Version: "v0.1.4", Metadata: strings.Repeat("c", 64), Binary: strings.Repeat("b", 64), Origin: "host", CommittedAt: now.Add(time.Minute), Workers: 1, WorkersPreserved: 1}
	return HostAdoption{Schema: 1, Operation: operation, Pin: pin, Confirmed: true, Adopted: adopted, Unattended: later}
}

func TestAdoptionRefusalHasNoSessionFallback(t *testing.T) {
	f := newAdoptionFixture(t)
	if err := f.supervisor.Start(t.Context(), f.start); err != nil {
		t.Fatal(err)
	}
	f.resume(t)
	f.refuse = "stage"
	if err := f.supervisor.Resume(t.Context()); err == nil {
		t.Fatal("refusal accepted")
	}
	if !reflect.DeepEqual(f.controls, []string{"info", "stage"}) {
		t.Fatalf("unexpected fallback controls: %v", f.controls)
	}
	_, operation := f.state(t)
	if operation.Phase != AdoptionActivating || !strings.Contains(operation.Problem, "refused") {
		t.Fatal("refusal did not remain durable at the pinned phase")
	}
}

func TestAdoptionChangedPreservedStateKeepsConfirmationFence(t *testing.T) {
	for _, kind := range []string{"policy", "identity", "grants"} {
		t.Run(kind, func(t *testing.T) { testChangedAdoptionState(t, kind) })
	}
}

func testChangedAdoptionState(t *testing.T, kind string) {
	f := newAdoptionFixture(t)
	if err := f.supervisor.Start(t.Context(), f.start); err != nil {
		t.Fatal(err)
	}
	f.resume(t)
	f.resume(t)
	switch kind {
	case "policy":
		policy, err := f.base.store.HostPolicy(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.base.store.SetHostPolicy(t.Context(), policy.Revision, HostPolicy{Mode: HostManual}); err != nil {
			t.Fatal(err)
		}
	case "identity":
		if err := os.Remove(filepath.Join(f.base.store.stateDir(), "identity.key")); err != nil {
			t.Fatal(err)
		}
		if _, _, err := identity.LoadOrCreate(f.base.store.stateDir()); err != nil {
			t.Fatal(err)
		}
	case "grants":
		if err := os.WriteFile(f.files[0], []byte("changed"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.supervisor.Resume(t.Context()); err == nil {
		t.Fatal("changed preserved state was confirmed")
	}
	envelope, _ := f.state(t)
	if envelope.Confirmed || envelope.Adopted != nil {
		t.Fatal("changed preserved state released the updater")
	}
}

func TestAdoptionOrderRequiresAllOtherHostsOnlyForInitialLastHost(t *testing.T) {
	var hosts []Host
	for range 4 {
		hosts = append(hosts, Host{ID: updateTestID(t), Endpoint: "wss://fixture.invalid/mesh", Platform: release.CurrentPlatform()})
	}
	canaries := []string{hosts[0].ID, hosts[1].ID}
	order, err := AdoptionOrder(Fleet{Version: 1, Name: "fixture", Revision: 1, Members: hosts}, canaries, hosts[3].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(order[hosts[0].ID]) != 0 || len(order[hosts[1].ID]) != 0 || len(order[hosts[2].ID]) != 2 || len(order[hosts[3].ID]) != 3 {
		t.Fatalf("wrong adoption order: %+v", order)
	}
}

func TestAdoptionCrashBeforePolicyPublicationKeepsFence(t *testing.T) {
	f := newAdoptionFixture(t)
	pinned, err := ReadAdoptionRelease(t.Context(), f.base.documents, f.start.Version)
	if err != nil {
		t.Fatal(err)
	}
	operation, revision, err := f.supervisor.prepare(t.Context(), f.start, pinned)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.base.store.beginAdoption(t.Context(), operation, revision); err != nil {
		t.Fatal(err)
	}
	policy, err := f.base.store.HostPolicy(t.Context())
	if err != nil || policy.Mode != HostManual {
		t.Fatal("policy was published before the durable adoption hold")
	}
	reason, err := f.base.store.hostYield()
	if err != nil || reason != "adoption awaiting confirmation" {
		t.Fatalf("recorded operation did not fence the host updater: %s %v", reason, err)
	}
	f.resume(t)
	_, actual := f.state(t)
	policy, err = f.base.store.HostPolicy(t.Context())
	if err != nil || !reflect.DeepEqual(policy, actual.Policy) || actual.Phase != AdoptionWaiting {
		t.Fatal("restarted helper did not publish the recorded policy before promotion")
	}
}

func TestAdoptionResumesStagedAndGrantedTransactions(t *testing.T) {
	for _, phase := range []updateinstall.Phase{updateinstall.Staged, updateinstall.Granted} {
		t.Run(string(phase), func(t *testing.T) { testAdoptionTransactionResume(t, phase) })
	}
}

func testAdoptionTransactionResume(t *testing.T, phase updateinstall.Phase) {
	f := newAdoptionFixture(t)
	if err := f.supervisor.Start(t.Context(), f.start); err != nil {
		t.Fatal(err)
	}
	f.resume(t)
	_, operation := f.state(t)
	if _, err := f.base.engine.Stage(t.Context(), operation.Request); err != nil {
		t.Fatal(err)
	}
	if phase == updateinstall.Granted {
		if _, err := f.base.engine.Grant(t.Context(), operation.ID, operation.Request.Generation); err != nil {
			t.Fatal(err)
		}
	}
	f.resume(t)
	f.resume(t)
	envelope, _ := f.state(t)
	if !envelope.Confirmed {
		t.Fatal("resumed transaction did not confirm")
	}
	if phase == updateinstall.Granted && len(f.controls) != 0 || phase == updateinstall.Staged && !reflect.DeepEqual(f.controls, []string{"grant"}) {
		t.Fatalf("restart repeated accepted update controls: %v", f.controls)
	}
	f.base.roundTrip()
}

func TestAdoptionNoticeFailureDoesNotDeliverOrActivate(t *testing.T) {
	f := newAdoptionFixture(t)
	f.start.Notice = &AdoptionNotice{Command: []string{"false"}, Lead: time.Minute}
	if err := f.supervisor.Start(t.Context(), f.start); err != nil {
		t.Fatal(err)
	}
	if err := f.supervisor.Resume(t.Context()); err == nil {
		t.Fatal("failed notice was accepted")
	}
	_, operation := f.state(t)
	if operation.Phase != AdoptionWaiting || !operation.Notice.DeliveredAt.IsZero() || len(f.controls) != 0 {
		t.Fatal("failed notice released activation")
	}
}

func TestAdoptionRollbackStopsAndRecordsExactFailedEdge(t *testing.T) {
	f := newAdoptionFixture(t)
	if err := f.supervisor.Start(t.Context(), f.start); err != nil {
		t.Fatal(err)
	}
	f.resume(t)
	f.base.fail = f.start.Version
	if err := f.supervisor.Resume(t.Context()); err == nil {
		t.Fatal("failed candidate was accepted")
	}
	envelope, operation := f.state(t)
	if envelope.Confirmed || operation.Phase != AdoptionRolledBack {
		t.Fatalf("rollback was not a terminal adoption hold: %+v", operation)
	}
	edge := Edge{operation.Request.Current.Digest, f.supervisor.Build.Digest, release.CurrentPlatform()}
	failed, err := f.base.store.failedHostEdge(edge)
	if err != nil || !failed {
		t.Fatal("rollback did not quarantine the exact failed edge")
	}
	before := len(f.controls)
	f.resume(t)
	if len(f.controls) != before {
		t.Fatal("rolled-back adoption retried its controls")
	}
	if err := canReplaceAdoption(operation, operation.Version); err == nil {
		t.Fatal("failed adoption accepted its same pin again")
	}
	f.base.roundTrip()
}

func TestAdoptionMilestonesRequireExactOperationAndAllWorkers(t *testing.T) {
	pin, operation := strings.Repeat("a", 64), strings.Repeat("b", 32)
	evidence := completeAdoptionEvidence(operation, pin, time.Now().UTC())
	if err := AdoptionMilestones(evidence, operation, pin); err != nil {
		t.Fatal(err)
	}
	if err := AdoptionMilestones(evidence, strings.Repeat("c", 32), pin); err == nil {
		t.Fatal("another operation's milestones were accepted")
	}
	if err := AdoptionMilestones(evidence, operation, strings.Repeat("d", 64)); err == nil {
		t.Fatal("another pin's milestones were accepted")
	}
	evidence.Unattended.WorkersPreserved = 0
	if err := AdoptionMilestones(evidence, operation, pin); err == nil {
		t.Fatal("lost worker released a descendant")
	}
}
