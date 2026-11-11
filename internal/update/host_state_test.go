package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/updateinstall"
)

func TestHostQuarantinePersistsAcrossRestartAndTwelveTargets(t *testing.T) {
	f := newHostFixture(t)
	f.head(2)
	f.tick()
	f.fail = "v0.1.2"
	status, err := f.engine.Run(t.Context())
	if err == nil || status.Phase != updateinstall.RolledBack {
		t.Fatalf("rollback: %s %v", status.Phase, err)
	}
	f.roundTrip()
	f.updater = f.newUpdater()
	f.tick()
	edge := Edge{f.current.Digest, f.builds["v0.1.2"].Digest, release.CurrentPlatform()}
	if failed, err := f.store.failedHostEdge(edge); err != nil || !failed {
		t.Fatalf("quarantine not durable: %v %v", failed, err)
	}
	for target := uint64(4); target <= 15; target++ {
		assertHostQuarantinedTarget(t, f, target)
	}
	f.addRelease(16, []uint64{1})
	f.head(16)
	f.now = f.now.Add(time.Hour)
	f.updater = f.newUpdater()
	f.tick()
	if got := f.installation(); got.Request.Manifest.Version != "v0.1.16" || got.Phase != updateinstall.Granted {
		t.Fatalf("verified alternative did not proceed: %+v", got)
	}
	f.finish()
	f.roundTrip()
}

func assertHostQuarantinedTarget(t *testing.T, f *hostFixture, target uint64) {
	t.Helper()
	f.addRelease(target, []uint64{2})
	f.head(target)
	f.now = f.now.Add(time.Hour)
	f.updater = f.newUpdater()
	for range 3 {
		f.tick()
	}
	if status := f.installation(); status.Phase != updateinstall.RolledBack {
		t.Fatalf("target %d retried failed edge: %s", target, status.Phase)
	}
}

func TestHostRollbackFailureRequiresAttentionWithoutRestartLoop(t *testing.T) {
	f := newHostFixture(t)
	f.tick()
	f.fail, f.failRollback = "v0.1.2", true
	status, err := f.engine.Run(t.Context())
	if err == nil || status.Phase != updateinstall.RollbackFailed {
		t.Fatalf("rollback failure: %s %v", status.Phase, err)
	}
	stops, starts := f.stops, f.starts
	for range 3 {
		if err := f.updater.Tick(t.Context()); err == nil {
			t.Fatal("rollback failure not reported")
		}
	}
	if f.stops != stops || f.starts != starts {
		t.Fatal("host updater restarted failed rollback")
	}
}

func TestHostLedgerCrashAfterAppendBeforeOwnershipClear(t *testing.T) {
	f := newHostFixture(t)
	f.tick()
	f.finish()
	state, err := f.store.HostState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	status := f.installation()
	unlock, err := f.store.lockContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	err = f.updater.recordCommit(state.Owned, status)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.SetHostPolicy(t.Context(), 1, HostPolicy{Mode: HostManual}); err != nil {
		t.Fatal(err)
	}
	f.updater = f.newUpdater()
	f.tick()
	f.tick()
	entries, more, err := f.store.HostLedger(t.Context(), 0, 64)
	if err != nil || more || len(entries) != 1 {
		t.Fatalf("duplicate ledger after crash: %+v %v %v", entries, more, err)
	}
}

func TestHostLedgerRecordsManualRetryOfEarlierFinalPhase(t *testing.T) {
	f := newHostFixture(t)
	if _, err := f.store.SetHostPolicy(t.Context(), 1, HostPolicy{Mode: HostManual}); err != nil {
		t.Fatal(err)
	}
	request := updateinstall.Request{ID: "manual", TargetID: "host", Generation: 1, Manifest: f.manifests["v0.1.2"], Current: f.current}
	if _, err := f.engine.Stage(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	failHostBeforeActivation(t, f, f.engine, OwnedRequest{Request: request.ID, Generation: request.Generation})
	f.tick()
	if _, err := f.engine.Retry(t.Context(), request.ID, request.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.Grant(t.Context(), request.ID, request.Generation); err != nil {
		t.Fatal(err)
	}
	f.finish()
	f.tick()
	entries, _, err := f.store.HostLedger(t.Context(), 0, 64)
	if err != nil || len(entries) != 1 || entries[0].Origin != "manual" {
		t.Fatalf("manual retry commit missing: %+v %v", entries, err)
	}
}

func TestHostLedgerUsesBoundedEntryFilesAndPages(t *testing.T) {
	f := newHostFixture(t)
	for seq := uint64(1); seq <= 150; seq++ {
		entry := LedgerEntry{Request: fmt.Sprintf("%032d", seq), Generation: seq, Binary: f.current.Digest, Version: f.current.Version, Origin: "manual", Metadata: strings.Repeat("a", 64), CommittedAt: f.now}
		if err := f.store.appendLedger(entry); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.store.appendLedger(LedgerEntry{Request: fmt.Sprintf("%032d", 1), Generation: 1}); err != nil {
		t.Fatal(err)
	}
	entries, more, err := f.store.HostLedger(t.Context(), 64, 64)
	if err != nil || !more || len(entries) != 64 || entries[0].Seq != 65 || entries[63].Seq != 128 {
		t.Fatalf("page: %+v %v %v", entries, more, err)
	}
	entries, more, err = f.store.HostLedger(t.Context(), 128, 64)
	if err != nil || more || len(entries) != 22 || entries[21].Seq != 150 {
		t.Fatalf("last page: %+v %v %v", entries, more, err)
	}
	if _, _, err := f.store.HostLedger(t.Context(), 0, 65); err == nil {
		t.Fatal("unbounded evidence page accepted")
	}
}

func TestHostAdoptionConfirmationAndLegacyConsentYield(t *testing.T) {
	for _, kind := range []string{"confirmation", "consent"} {
		t.Run(kind, func(t *testing.T) { testHostOtherAuthority(t, kind) })
	}
}

func testHostOtherAuthority(t *testing.T, kind string) {
	f := newHostFixture(t)
	if kind == "confirmation" {
		adoption := HostAdoption{Schema: 1, Operation: strings.Repeat("a", 32), Pin: discoveryDigest(f.files[f.current.Version+"/"+release.UpdateMetadataAssetName])}
		if err := f.store.writeAuthority("host-adoption.json", adoption); err != nil {
			t.Fatal(err)
		}
	} else {
		consent := RecipientConsent{Schema: 1, Revision: 1, LocalHostID: strings.Repeat("A", 43), CoordinatorID: strings.Repeat("A", 43), Membership: strings.Repeat("a", 64), Epoch: 1, Enabled: true}
		if err := f.store.writeAuthority("automatic-consent.json", consent); err != nil {
			t.Fatal(err)
		}
	}
	f.tick()
	if f.helperCalls != 0 {
		t.Fatal("held updater changed helper")
	}
	if _, err := f.engine.Read(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("held updater staged: %v", err)
	}
}

func TestHostAdoptionMilestoneSurvivesOrdinaryLedger(t *testing.T) {
	f := newHostFixture(t)
	pin := discoveryDigest(f.files[f.current.Version+"/"+release.UpdateMetadataAssetName])
	adoption := HostAdoption{Schema: 1, Operation: strings.Repeat("a", 32), Pin: pin, Confirmed: true,
		Adopted: &LedgerEntry{Request: strings.Repeat("b", 32), Generation: 1, Origin: "adoption", Metadata: pin, Binary: f.current.Digest, Version: f.current.Version, CommittedAt: f.now.Add(-time.Hour)},
		Details: json.RawMessage(`{"futurePhase":{"retained":"opaque"}}`)}
	if err := f.store.writeAuthority("host-adoption.json", adoption); err != nil {
		t.Fatal(err)
	}
	f.tick()
	f.finish()
	if _, err := f.store.SetHostPolicy(t.Context(), 1, HostPolicy{Mode: HostManual}); err != nil {
		t.Fatal(err)
	}
	f.tick()
	var retained HostAdoption
	if err := f.store.readAuthority("host-adoption.json", &retained); err != nil {
		t.Fatal(err)
	}
	if retained.Operation != adoption.Operation || retained.Pin != adoption.Pin || retained.Unattended == nil || retained.Unattended.Binary != f.current.Digest {
		t.Fatalf("milestone lost: %+v", retained)
	}
	if string(retained.Details) != string(adoption.Details) {
		t.Fatal("unattended reconciliation changed Unit 10 operation details")
	}
}

func TestHostAdoptionPinBindsMetadataAcrossPlatforms(t *testing.T) {
	pin := strings.Repeat("a", 64)
	adoption := HostAdoption{Schema: 1, Operation: strings.Repeat("a", 32), Pin: pin, Confirmed: true,
		Adopted: &LedgerEntry{Request: "adoption", Generation: 1, Metadata: pin, Binary: strings.Repeat("b", 64), Origin: "adoption", Version: "v0.1.1", CommittedAt: time.Now().UTC()},
		Details: json.RawMessage(`{"future":{"phase":"preserved"}}`)}
	if err := adoption.validate(); err != nil {
		t.Fatal(err)
	}
	adoption.Adopted.Binary = strings.Repeat("c", 64)
	if err := adoption.validate(); err != nil {
		t.Fatal("platform-specific binary changed the common activation pin", err)
	}
	adoption.Pin = adoption.Adopted.Binary
	if err := adoption.validate(); err == nil {
		t.Fatal("binary digest was accepted as an activation metadata pin")
	}
	adoption.Pin = pin
	adoption.Details = json.RawMessage(`{"oversized":"` + strings.Repeat("x", 64<<10) + `"}`)
	if err := adoption.validate(); err == nil {
		t.Fatal("unbounded Unit 10 details accepted")
	}
}

func TestHostAdoptionConfirmationFencesRealGrant(t *testing.T) {
	f := newHostFixture(t)
	owned := reserveHostWithoutStage(t, f)
	pin := discoveryDigest(f.files[f.current.Version+"/"+release.UpdateMetadataAssetName])
	adoption := HostAdoption{Schema: 1, Operation: strings.Repeat("a", 32), Pin: pin, Confirmed: true,
		Adopted: &LedgerEntry{Request: "adoption", Generation: 1, Metadata: pin, Binary: f.current.Digest, Origin: "adoption", Version: f.current.Version, CommittedAt: f.now.Add(-time.Hour)}}
	if err := f.store.writeAuthority("host-adoption.json", adoption); err != nil {
		t.Fatal(err)
	}
	engine := f.engine.WithHostOrigin().WithAcceptance(f.store.AuthorizeHostAcceptance(nil))
	if _, err := engine.Stage(t.Context(), owned.Installation); err != nil {
		t.Fatal(err)
	}
	adoption.Confirmed = false
	unlock, err := f.store.lockContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	err = f.store.writeAuthority("host-adoption.json", adoption)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Grant(t.Context(), owned.Request, owned.Generation); err == nil {
		t.Fatal("grant crossed unfinished adoption confirmation")
	}
	if got := f.installation(); got.Phase != updateinstall.Staged {
		t.Fatalf("confirmation fence changed journal: %s", got.Phase)
	}
}

func TestHostInvalidHelperMountAndInstalledImageBackOff(t *testing.T) {
	for _, kind := range []string{"helper", "mount", "installed image", "development", "modified", "malformed policy"} {
		t.Run(kind, func(t *testing.T) { testHostPreflight(t, kind) })
	}
}

func testHostPreflight(t *testing.T, kind string) {
	f := newHostFixture(t)
	switch kind {
	case "helper":
		f.helperErr = errors.New("helper process is stopped")
	case "mount":
		f.settings.RequiredMount = filepath.Dir(f.settings.CacheDir)
		var err error
		f.engine, err = updateinstall.New(f.settings)
		if err != nil {
			t.Fatal(err)
		}
	case "installed image":
		if err := os.WriteFile(f.settings.Executable, []byte("modified"), 0600); err != nil {
			t.Fatal(err)
		}
	case "development":
		f.current.Version = "dev"
	case "modified":
		f.current.Modified = true
	case "malformed policy":
		if err := f.store.writeAuthority("host-policy.json", map[string]any{"schema": 1, "revision": 1, "mode": "invalid"}); err != nil {
			t.Fatal(err)
		}
	}
	err := f.updater.Tick(t.Context())
	if err == nil && kind != "development" && kind != "modified" {
		t.Fatal("invalid prerequisite was accepted")
	}
	if _, err := f.engine.Read(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid prerequisite staged: %v", err)
	}
	state, stateErr := f.store.HostState(context.Background())
	if stateErr != nil {
		t.Fatal(stateErr)
	}
	if f.stops != 0 || f.starts != 0 {
		t.Fatal("preflight failure restarted daemon")
	}
	if err != nil && state.Phase != HostAttention && !state.NextAttempt.After(f.now) {
		t.Fatalf("failure lacks backoff: %+v", state)
	}
}
