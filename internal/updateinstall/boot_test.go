package updateinstall

import (
	"context"
	"errors"
	"testing"

	"github.com/shaul/mesh/internal/updategate"
)

func TestGrantedActivationWaitsForDaemonStartup(t *testing.T) {
	f := newFixture(t)
	f.stage()
	if _, err := f.engine.Grant(context.Background(), f.request.ID, 1); err != nil {
		t.Fatal(err)
	}
	attempts := 0
	f.engine.cfg.Probe = func(ctx context.Context) (Health, error) {
		attempts++
		if attempts <= 2 {
			return Health{}, errors.New("daemon socket is not ready")
		}
		return f.manager.probe(ctx)
	}
	result, err := f.engine.Run(context.Background())
	if err != nil || result.Phase != Committed {
		t.Fatalf("daemon startup race stranded an approved update: %s %v", result.Phase, err)
	}
	f.roundTrip()
}

func TestUnavailableDaemonRetainsGrantForAutonomousRetry(t *testing.T) {
	f := newFixture(t)
	f.stage()
	if _, err := f.engine.Grant(context.Background(), f.request.ID, 1); err != nil {
		t.Fatal(err)
	}
	f.engine.cfg.Probe = func(context.Context) (Health, error) {
		return Health{}, errors.New("daemon socket is not ready")
	}
	result, err := f.engine.Run(context.Background())
	if err == nil || result.Phase != Granted || f.manager.stops != 0 {
		t.Fatalf("transient daemon startup failure consumed approval: %s %v", result.Phase, err)
	}
	if err := updategate.Check(f.engine.cfg.StateDir); err != nil {
		t.Fatalf("original daemon remained gated before activation: %v", err)
	}
	f.engine.cfg.Probe = f.manager.probe
	result, err = f.engine.Run(context.Background())
	if err != nil || result.Phase != Committed {
		t.Fatalf("persisted grant did not resume without another approval: %s %v", result.Phase, err)
	}
	f.roundTrip()
}

func TestRetryPreservesRebootInterruptionEvidence(t *testing.T) {
	f := newFixture(t)
	status := f.stage()
	status.Phase, status.Original.BootID = Failed, "previous"
	if err := f.engine.save(&status); err != nil {
		t.Fatal(err)
	}
	f.engine.cfg.Probe = func(ctx context.Context) (Health, error) {
		health, err := f.manager.probe(ctx)
		health.BootID, health.Workers = "current", nil
		return health, err
	}
	result, err := f.engine.RetryWithToken(context.Background(), f.request.ID, 1, 1)
	if err != nil || len(result.Original.InterruptedWorkers) != 1 {
		t.Fatalf("retry lost reboot interruption evidence: %+v %v", result.Original, err)
	}
}

func TestVerifiedBootChangeResumesActivationAndRollback(t *testing.T) {
	for _, test := range []struct {
		name     string
		phase    Phase
		rollback bool
	}{{"before activation", Granted, false}, {"activation", Activating, false}, {"rollback", Activating, true}} {
		t.Run(test.name, func(t *testing.T) { testBootChange(t, test.phase, test.rollback) })
	}
}

func testBootChange(t *testing.T, phase Phase, rollback bool) {
	f := newFixture(t)
	status := f.stage()
	status.Phase = phase
	status.Original.BootID = "previous-kernel-boot"
	if err := f.engine.save(&status); err != nil {
		t.Fatal(err)
	}
	f.manager.failCandidate = rollback
	f.engine.cfg.Probe = func(ctx context.Context) (Health, error) {
		health, err := f.manager.probe(ctx)
		health.BootID = "current-kernel-boot"
		health.Workers = nil
		return health, err
	}
	result, err := f.engine.Run(context.Background())
	want := Committed
	if rollback {
		want = RolledBack
	}
	if result.Phase != want || (!rollback && err != nil) {
		t.Fatalf("resume after boot change = %s %v", result.Phase, err)
	}
	if result.Verified == nil || len(result.Verified.Workers) != 0 || len(result.Verified.InterruptedWorkers) != 1 {
		t.Fatalf("reboot lost interruption evidence: %+v", result.Verified)
	}
	if result.Verified.InterruptedWorkers[0].ID != status.Original.Workers[0].ID {
		t.Fatal("reboot receipt did not identify the interrupted session")
	}
	if err := updategate.Check(f.engine.cfg.StateDir); err != nil {
		t.Fatalf("verified daemon remains gated after reboot: %v", err)
	}
}

func TestUnknownOrUnchangedBootCannotExcuseLostWorkers(t *testing.T) {
	f := newFixture(t)
	status := f.stage()
	for _, boot := range [][2]string{{"", ""}, {"", "current"}, {"previous", ""}, {"same", "same"}} {
		status.Original.BootID = boot[0]
		health := Health{HostID: f.request.TargetID, BootID: boot[1], Build: f.request.Current}
		if err := verifyHealth(health, f.request.Current, status); err == nil {
			t.Fatalf("missing worker accepted for boot IDs %q -> %q", boot[0], boot[1])
		}
	}
}

func TestBootChangeStillRequiresExecutingImageAndCompatibleDatabase(t *testing.T) {
	f := newFixture(t)
	status := f.stage()
	status.Original.BootID = "previous"
	health := Health{HostID: f.request.TargetID, BootID: "current", Build: f.request.Current}
	health.Build.Digest = "wrong-executable"
	if err := verifyHealth(health, f.request.Current, status); err == nil {
		t.Fatal("boot change accepted the wrong executable")
	}
	health.Build = f.request.Current
	health.Build.StateVersion++
	if err := verifyHealth(health, f.request.Current, status); err == nil {
		t.Fatal("boot change accepted an unsupported database")
	}
	status.Request.Manifest.Compatibility.StateReadMin = 5
	expected := f.request.Current
	expected.Digest = "candidate-executable"
	health.Build = expected
	health.Build.StateVersion = 5
	if err := verifyHealth(health, expected, status); err == nil {
		t.Fatal("candidate accepted an unfinished database migration")
	}
}
