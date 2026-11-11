package update

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/updateinstall"
)

func localTargetInput(t *testing.T, f *hostFixture, version string) HostTargetSubmission {
	t.Helper()
	policy, err := f.store.SetHostPolicy(t.Context(), 1, HostPolicy{Mode: HostManual})
	if err != nil {
		t.Fatal(err)
	}
	return HostTargetSubmission{Version: version, CurrentDigest: f.current.Digest, PolicyRevision: policy.Revision}
}

func TestHostTargetPlanIsReadOnlyAndSubmissionIsReserved(t *testing.T) {
	f := newHostFixture(t)
	input := localTargetInput(t, f, "v0.1.3")
	plan, err := f.updater.PlanLocalTarget(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Target.Version != input.Version || plan.Next.Version != "v0.1.2" || !hostDigest(plan.Next.Forward) || !hostDigest(plan.Next.Rollback) {
		t.Fatalf("verified plan: %+v", plan)
	}
	if f.helperCalls != 0 || f.stops != 0 || f.starts != 0 {
		t.Fatal("preview changed helper or service")
	}
	for _, file := range []string{"host-target.json", "host-update.json", "host-ledger.json"} {
		if _, err := os.Stat(filepath.Join(f.store.stateDir(), "updates", file)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("preview wrote %s: %v", file, err)
		}
	}
	status, err := f.updater.SubmitLocalTarget(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if status.Revision != 1 || status.Phase != HostTargetReserved || status.Request == "" || status.Generation != 1 || status.Next == nil || status.Target != plan.Target || *status.Next != plan.Next {
		t.Fatalf("submission did not reserve verified work: %+v", status)
	}
	if _, err := f.engine.Read(); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("submission claimed an issued installation before scheduler acceptance")
	}
	state, err := f.store.HostState(t.Context())
	if err != nil || state.Owned == nil || state.Owned.Request != status.Request || state.Owned.IntentRevision != status.Revision {
		t.Fatalf("missing durable ownership: %+v %v", state, err)
	}
	replayed, err := f.updater.SubmitLocalTarget(t.Context(), input)
	if err != nil || replayed.Request != status.Request || replayed.Generation != status.Generation || replayed.PendingSince != status.PendingSince {
		t.Fatalf("submission replay drifted: %+v %v", replayed, err)
	}
}

func TestHostTargetManualTwoHopsRestartAndNoLatestDrift(t *testing.T) {
	f := newHostFixture(t)
	input := localTargetInput(t, f, "v0.1.3")
	status, err := f.updater.SubmitLocalTarget(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	f.addRelease(4, []uint64{3})
	f.head(4)
	f.tick()
	if got := f.installation(); got.Phase != updateinstall.Granted || got.Request.ID != status.Request || got.Request.Manifest.Version != "v0.1.2" {
		t.Fatalf("first manual hop: %+v", got)
	}
	f.finish()
	f.roundTrip()
	f.updater = f.newUpdater()
	f.tick()
	if got := f.installation(); got.Phase != updateinstall.Granted || got.Request.Manifest.Version != input.Version {
		t.Fatalf("pinned final manual hop: %+v", got)
	}
	f.finish()
	f.tick()
	f.roundTrip()
	final, err := f.store.HostTarget(t.Context())
	if err != nil || final.Phase != HostTargetCommitted || final.Target != status.Target || final.PendingSince != status.PendingSince {
		t.Fatalf("final target: %+v %v", final, err)
	}
	policy, err := f.store.HostPolicy(t.Context())
	if err != nil || policy.Mode != HostManual || policy.Revision != input.PolicyRevision {
		t.Fatalf("manual approval toggled policy: %+v %v", policy, err)
	}
	f.tick()
	if got := f.installation(); got.Request.Manifest.Version != input.Version {
		t.Fatal("manual target drifted to latest after completion")
	}
	entries, _, err := f.store.HostLedger(t.Context(), 0, 64)
	if err != nil || len(entries) != 2 || entries[0].Origin != string(HostManual) || entries[0].Metadata == "" {
		t.Fatalf("manual pinned evidence: %+v %v", entries, err)
	}
}

func TestHostTargetCancellationAndMissingIntentFence(t *testing.T) {
	for _, kind := range []string{"cancel", "policy", "missing intent", "missing ownership"} {
		t.Run(kind, func(t *testing.T) { testHostTargetFence(t, kind) })
	}
}

func testHostTargetFence(t *testing.T, kind string) {
	f := newHostFixture(t)
	input := localTargetInput(t, f, "v0.1.2")
	status, err := f.updater.SubmitLocalTarget(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	state, err := f.store.HostState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	switch kind {
	case "cancel":
		if _, err := f.store.CancelLocalTarget(t.Context(), status.Revision); err != nil {
			t.Fatal(err)
		}
	case "policy":
		if _, err := f.store.SetHostPolicy(t.Context(), input.PolicyRevision, HostPolicy{Mode: HostManual}); err != nil {
			t.Fatal(err)
		}
	case "missing intent":
		if err := os.Remove(filepath.Join(f.store.stateDir(), "updates", "host-target.json")); err != nil {
			t.Fatal(err)
		}
	case "missing ownership":
		if err := os.Remove(filepath.Join(f.store.stateDir(), "updates", "host-update.json")); err != nil {
			t.Fatal(err)
		}
	}
	engine := f.engine.WithHostOrigin().WithAcceptance(f.store.AuthorizeHostAcceptance(nil))
	if _, err := engine.Stage(t.Context(), state.Owned.Installation); err == nil {
		t.Fatal("local intent fell through to ordinary manual authority")
	}
	if _, err := f.engine.Read(); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("withdrawn local intent accepted installation")
	}
}

func TestHostTargetCancelDuringDownloadAndIssuedWorkReconcile(t *testing.T) {
	for _, issued := range []bool{false, true} {
		t.Run(map[bool]string{false: "download", true: "granted"}[issued], func(t *testing.T) { testTargetCancel(t, issued) })
	}
}

func testTargetCancel(t *testing.T, issued bool) {
	f := newHostFixture(t)
	status, err := f.updater.SubmitLocalTarget(t.Context(), localTargetInput(t, f, "v0.1.2"))
	if err != nil {
		t.Fatal(err)
	}
	if issued {
		f.tick()
		if _, err := f.store.CancelLocalTarget(t.Context(), status.Revision); err != nil {
			t.Fatal(err)
		}
		f.finish()
		f.tick()
		final, err := f.store.HostTarget(t.Context())
		if err != nil || final.Phase != HostTargetCancelled {
			t.Fatalf("issued cancellation: %+v %v", final, err)
		}
		entries, _, err := f.store.HostLedger(t.Context(), 0, 64)
		if err != nil || len(entries) != 1 {
			t.Fatalf("issued result lost: %+v %v", entries, err)
		}
		return
	}
	client := *f.documents.Client.HTTPClient
	blocked := &blockedHostArchive{base: client.Transport, entered: make(chan struct{}), resume: make(chan struct{})}
	release := sync.OnceFunc(func() { close(blocked.resume) })
	t.Cleanup(release)
	client.Transport = blocked
	f.settings.Client.HTTPClient = &client
	f.engine, err = updateinstall.New(f.settings)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- f.updater.Tick(t.Context()) }()
	<-blocked.entered
	if _, err := f.store.CancelLocalTarget(t.Context(), status.Revision); err != nil {
		t.Fatal(err)
	}
	release()
	if err := <-done; err == nil {
		t.Fatal("cancelled download published staged commit")
	}
	if got := f.installation(); got.Phase != updateinstall.Accepted {
		t.Fatalf("cancel crossed stage fence: %s", got.Phase)
	}
	f.tick()
	if got := f.installation(); got.Phase != updateinstall.Cancelled {
		t.Fatalf("ungranted work not retired: %s", got.Phase)
	}
}

func TestHostTargetRejectsStaleInputsAndBadEvidence(t *testing.T) {
	for _, kind := range []string{"digest", "policy", "intent", "signature", "rollback", "worker", "helper"} {
		t.Run(kind, func(t *testing.T) { testTargetReject(t, kind) })
	}
}

func TestHostTargetKeepsQuarantineUntilExactRetryCommits(t *testing.T) {
	f := newHostFixture(t)
	f.head(2)
	f.tick()
	f.fail = "v0.1.2"
	if status, err := f.engine.Run(t.Context()); err == nil || status.Phase != updateinstall.RolledBack {
		t.Fatalf("expected verified rollback: %s %v", status.Phase, err)
	}
	f.tick()
	input := localTargetInput(t, f, "v0.1.2")
	edge := Edge{From: f.current.Digest, To: f.builds["v0.1.2"].Digest, Platform: f.current.Platform}
	if _, err := f.updater.SubmitLocalTarget(t.Context(), input); err == nil {
		t.Fatal("local submission silently cleared quarantine")
	}
	input.RetryEdge = &edge
	if _, err := f.updater.SubmitLocalTarget(t.Context(), input); err != nil {
		t.Fatal(err)
	}
	if failed, err := f.store.failedHostEdge(edge); err != nil || !failed {
		t.Fatalf("reservation erased quarantine before success: %v %v", failed, err)
	}
	f.fail = ""
	f.tick()
	f.finish()
	f.tick()
	if failed, err := f.store.failedHostEdge(edge); err != nil || failed {
		t.Fatalf("committed retry did not settle old-source quarantine: %v %v", failed, err)
	}
}

func TestHostTargetReservationRecoveryBeforeJournalAndMissingOwnership(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(map[bool]string{false: "before journal", true: "accepted"}[accepted], func(t *testing.T) { testTargetRecovery(t, accepted) })
	}
}

func testTargetRecovery(t *testing.T, accepted bool) {
	f := newHostFixture(t)
	status, err := f.updater.SubmitLocalTarget(t.Context(), localTargetInput(t, f, "v0.1.2"))
	if err != nil {
		t.Fatal(err)
	}
	if accepted {
		state, err := f.store.HostState(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		engine := f.engine.WithHostOrigin().WithAcceptance(f.store.AuthorizeHostAcceptance(nil))
		if _, err := engine.Stage(t.Context(), state.Owned.Installation); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(f.store.stateDir(), "updates", "host-update.json")); err != nil {
		t.Fatal(err)
	}
	f.updater = f.newUpdater()
	if accepted {
		if err := f.updater.Tick(t.Context()); err == nil {
			t.Fatal("missing accepted ownership was reconstructed as manual approval")
		}
		if got := f.installation(); got.Phase != updateinstall.Staged {
			t.Fatal("missing ownership granted an accepted journal")
		}
		return
	}
	f.tick()
	state, err := f.store.HostState(t.Context())
	if err != nil || state.Owned == nil || state.Owned.Request != status.Request {
		t.Fatalf("pre-journal recovery lost exact reservation: %+v %v", state, err)
	}
	f.tick()
	if got := f.installation(); got.Request.ID != status.Request || got.Phase != updateinstall.Granted {
		t.Fatalf("recovered target request: %+v", got)
	}
}

func TestHostTargetWorksWithMissingDefaultManualPolicy(t *testing.T) {
	f := newHostFixture(t)
	if err := os.Remove(filepath.Join(f.store.stateDir(), "updates", "host-policy.json")); err != nil {
		t.Fatal(err)
	}
	status, err := f.updater.SubmitLocalTarget(t.Context(), HostTargetSubmission{Version: "v0.1.2", CurrentDigest: f.current.Digest})
	if err != nil {
		t.Fatal(err)
	}
	f.tick()
	if got := f.installation(); got.Request.ID != status.Request || got.Phase != updateinstall.Granted {
		t.Fatal("default manual policy could not execute its explicit target")
	}
	policy, err := f.store.HostPolicy(t.Context())
	if err != nil || policy.Revision != 0 || policy.Mode != HostManual {
		t.Fatal("local submission wrote a default auto policy")
	}
}

func TestHostTargetPreviewDoesNotAcknowledgeAndOrderFencesManualStage(t *testing.T) {
	f := newHostFixture(t)
	order, peer, _, _, now := orderFixture(t)
	order.Store = f.store
	peer.commit(t, 2, now.Add(-3*time.Hour))
	setPeerMetadata(t, peer, discoveryDigest(f.files["v0.1.2/mesh-update.json"]))
	f.updater.Order = order
	policy, err := f.store.SetHostPolicy(t.Context(), 1, HostPolicy{Mode: HostManual, After: []string{peer.host.ID}})
	if err != nil {
		t.Fatal(err)
	}
	input := HostTargetSubmission{Version: "v0.1.3", CurrentDigest: f.current.Digest, PolicyRevision: policy.Revision}
	plan, err := f.updater.PlanLocalTarget(t.Context(), input)
	if err != nil || plan.Next.Version != "v0.1.2" || plan.Target.Version != input.Version {
		t.Fatalf("ordered preview: %+v %v", plan, err)
	}
	dependents, err := peer.store.HostDependents(t.Context())
	if err != nil || len(dependents) != 0 {
		t.Fatalf("preview mutated predecessor ledger retention: %+v %v", dependents, err)
	}
	if f.helperCalls != 0 {
		t.Fatal("ordered preview invoked helper")
	}
	if _, err := f.updater.SubmitLocalTarget(t.Context(), input); err != nil {
		t.Fatal(err)
	}
	dependents, err = peer.store.HostDependents(t.Context())
	if err != nil || len(dependents) != 1 {
		t.Fatalf("real submission did not acknowledge predecessor: %+v %v", dependents, err)
	}
	peer.state.Store(1)
	f.tick()
	if _, err := f.engine.Read(); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("manual intent bypassed refreshed predecessor refusal")
	}
	state, err := f.store.HostState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	proof := hostHopProof{Workers: []string{f.current.Digest}}
	engine := f.engine.WithHostOrigin().WithAcceptance(f.updater.acceptWorkers(proof))
	if _, err := engine.Stage(t.Context(), state.Owned.Installation); err == nil {
		t.Fatal("manual intent bypassed acceptance-time order fence")
	}
}

func testTargetReject(t *testing.T, kind string) {
	f := newHostFixture(t)
	input := localTargetInput(t, f, "v0.1.2")
	switch kind {
	case "digest":
		input.CurrentDigest = discoveryDigest([]byte("stale"))
	case "policy":
		input.PolicyRevision++
	case "intent":
		input.ExpectedRevision++
	case "signature":
		delete(f.files, "v0.1.2/mesh-update.sig.json")
	case "rollback":
		removeHostBoundary(t, f, "v0.1.2")
	case "worker":
		f.workers[0].Build = nil
	case "helper":
		f.helperErr = errors.New("helper is not running")
	}
	if _, err := f.updater.SubmitLocalTarget(t.Context(), input); err == nil {
		t.Fatal("unverified local target was approved")
	}
	if _, err := f.engine.Read(); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("invalid submission issued installation work")
	}
	status, err := f.store.HostTarget(context.Background())
	if err != nil || status.Revision != 0 {
		t.Fatalf("invalid submission reserved intent: %+v %v", status, err)
	}
}
