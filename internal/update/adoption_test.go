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
		Service: func(context.Context, updateinstall.Settings) (string, error) { return strings.Repeat("d", 64), nil },
		Now:     func() time.Time { return b.now }, Promote: f.promote, Helper: f.helper, Engine: func(Adoption) (*updateinstall.Engine, error) { return b.engine, nil },
		Evidence: func(_ context.Context, peer Host) (HostAdoption, error) { return f.peers[peer.ID], f.peerError }}
	return f
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
