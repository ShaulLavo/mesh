package updateinstall

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/updategate"
)

func TestFailedRollbackSettlesActivationGate(t *testing.T) {
	f := newFixture(t)
	f.stage()
	f.manager.failCandidate, f.manager.failRollback = true, true
	if _, err := f.engine.Grant(context.Background(), f.request.ID, 1); err != nil {
		t.Fatal(err)
	}
	status, err := f.engine.Run(context.Background())
	if err == nil || status.Phase != RollbackFailed {
		t.Fatalf("rollback = %s %v", status.Phase, err)
	}
	durable, readErr := f.engine.Read()
	if readErr != nil || durable.Phase != RollbackFailed || durable.Error != status.Error {
		t.Fatalf("terminal journal = %+v %v", durable, readErr)
	}
	if err := updategate.Check(f.engine.cfg.StateDir); err != nil {
		t.Fatalf("failed rollback left gate held: %v", err)
	}
	f.roundTrip()
}

func terminalFixture(t *testing.T, phase Phase) (*fixture, Status) {
	t.Helper()
	f := newFixture(t)
	status := f.stage()
	status.Phase, status.Error = phase, "persisted terminal failure"
	if err := f.engine.save(&status); err != nil {
		t.Fatal(err)
	}
	if err := updategate.Set(f.engine.cfg.StateDir, f.request.ID); err != nil {
		t.Fatal(err)
	}
	return f, status
}

func TestTerminalRunAndCancelSettleOnlyMatchingGate(t *testing.T) {
	for _, phase := range []Phase{Committed, RolledBack, RollbackFailed, Failed, Cancelled} {
		for _, action := range []string{"run", "cancel"} {
			t.Run(string(phase)+"/"+action, func(t *testing.T) {
				f, previous := terminalFixture(t, phase)
				reopened, err := New(f.engine.cfg)
				if err != nil {
					t.Fatal(err)
				}
				var status Status
				if action == "cancel" {
					status, err = reopened.Cancel(context.Background(), f.request.ID, 1)
				} else {
					status, err = reopened.Run(context.Background())
				}
				if action == "cancel" && err != nil {
					t.Fatalf("terminal cancellation: %v", err)
				}
				if status.Phase != previous.Phase || status.Error != previous.Error {
					t.Fatalf("settlement changed terminal receipt: %+v", status)
				}
				if err := updategate.Check(f.engine.cfg.StateDir); err != nil {
					t.Fatalf("terminal %s retained gate: %v", action, err)
				}
				if f.manager.stops != 0 {
					t.Fatal("terminal settlement stopped daemon")
				}
				f.roundTrip()
			})
		}
	}
}

func TestNextStageReclaimsFailedRollbackGate(t *testing.T) {
	f, _ := terminalFixture(t, RollbackFailed)
	request := f.request
	request.ID, request.Generation = "operation-2", 2
	status, err := f.engine.Stage(context.Background(), request)
	if err != nil || status.Phase != Staged || status.Request.ID != request.ID {
		t.Fatalf("next stage = %s %v", status.Phase, err)
	}
	if err := updategate.Check(f.engine.cfg.StateDir); err != nil {
		t.Fatalf("previous gate remains: %v", err)
	}
}

func TestTerminalSettlementFencesOwnerAndGeneration(t *testing.T) {
	for _, action := range []string{"run", "cancel", "stage"} {
		t.Run(action, func(t *testing.T) {
			f, _ := terminalFixture(t, RollbackFailed)
			if err := updategate.Set(f.engine.cfg.StateDir, "another-operation"); err != nil {
				t.Fatal(err)
			}
			var err error
			switch action {
			case "run":
				_, err = f.engine.Run(context.Background())
			case "cancel":
				_, err = f.engine.Cancel(context.Background(), f.request.ID, 1)
			case "stage":
				request := f.request
				request.ID, request.Generation = "operation-2", 2
				_, err = f.engine.Stage(context.Background(), request)
			}
			if err == nil {
				t.Fatal("foreign owner was ignored")
			}
			assertGateOwner(t, f, "another-operation")
		})
	}
	f, _ := terminalFixture(t, Failed)
	if _, err := f.engine.Cancel(context.Background(), f.request.ID, 2); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("stale cancel = %v", err)
	}
	assertGateOwner(t, f, f.request.ID)
}

func TestCancelCannotSettleActiveOrRetriedIncarnation(t *testing.T) {
	for _, phase := range []Phase{Granted, Activating, Validating, RollingBack} {
		t.Run(string(phase), func(t *testing.T) {
			f, _ := terminalFixture(t, phase)
			if _, err := f.engine.Cancel(context.Background(), f.request.ID, 1); !errors.Is(err, ErrAlreadyGranted) {
				t.Fatalf("active cancellation = %v", err)
			}
			assertGateOwner(t, f, f.request.ID)
		})
	}
	f, _ := terminalFixture(t, Failed)
	if _, err := f.engine.Retry(context.Background(), f.request.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.Grant(context.Background(), f.request.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.Cancel(context.Background(), f.request.ID, 1); !errors.Is(err, ErrAlreadyGranted) {
		t.Fatalf("retry incarnation cancelled: %v", err)
	}
	assertGateOwner(t, f, f.request.ID)
}

func TestTerminalCancelWaitsForInstallationLock(t *testing.T) {
	f, _ := terminalFixture(t, RollbackFailed)
	lock, err := lockInstallation(context.Background(), f.engine.cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock(lock)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := f.engine.Cancel(ctx, f.request.ID, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("locked cancellation = %v", err)
	}
	assertGateOwner(t, f, f.request.ID)
}

func assertGateOwner(t *testing.T, f *fixture, owner string) {
	t.Helper()
	data, err := os.ReadFile(updategate.Path(f.engine.cfg.StateDir))
	if err != nil || string(data) != owner+"\n" {
		t.Fatalf("gate owner = %q %v", data, err)
	}
}

func TestStageCannotBorrowAnotherOperationsTerminalReceipt(t *testing.T) {
	f, _ := terminalFixture(t, Failed)
	request := f.request
	request.ID = "operation-2"
	if _, err := f.engine.Stage(context.Background(), request); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("another operation borrowed terminal receipt: %v", err)
	}
	assertGateOwner(t, f, f.request.ID)
}
