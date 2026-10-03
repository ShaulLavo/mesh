package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/updateinstall"
)

func TestHelperRecoveryRequiresBoundJournalBeforeWork(t *testing.T) {
	_, _, err := executeCommand(t, Dependencies{}, "update-helper", "recover", "--state-dir", filepath.Join(t.TempDir(), "state"))
	if err == nil || !strings.Contains(err.Error(), "recovery requires an exact journal tuple") {
		t.Fatalf("supported local recovery boundary = %v; want exact journal tuple rejection", err)
	}
}

func TestHelperRecoveryApprovalProbeIsReadOnlyForAbsentStore(t *testing.T) {
	root := t.TempDir()
	status := updateinstall.Status{Settings: updateinstall.Settings{StateDir: root}}
	if err := checkHelperRecoveryApprovals(t.Context(), status); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "updates")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("approval probe created an absent update store")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := checkHelperRecoveryApprovals(ctx, status); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled approval probe = %v", err)
	}
}

func TestHelperRecoveryCompletedRetryMetadataIsInert(t *testing.T) {
	root, local := setupUpdateCLI(t)
	_, second := setupUpdateCLI(t)
	_, third := setupUpdateCLI(t)
	second.Alias, third.Alias = "second", "third"
	store, err := update.OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.Start(local.ID, scopedUpdateFleet("completed-retry-fixture", []update.Host{local, second, third}), updateTestManifest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Change(run.ID, func(run *update.Run) error {
		for index := range run.Targets {
			run.Targets[index].State = update.Updated
			run.Targets[index].RetryPending = run.Targets[index].Host.ID == local.ID
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := helperRecoveryRunBytes(t, root, run.ID)
	status := updateinstall.Status{Settings: updateinstall.Settings{StateDir: root}, Request: updateinstall.Request{TargetID: local.ID}}
	if err = checkHelperRecoveryApprovals(t.Context(), status); err != nil {
		t.Fatalf("completed all-three-updated run with inert retry metadata blocks helper recovery: %v", err)
	}
	after := helperRecoveryRunBytes(t, root, run.ID)
	if string(after) != string(before) {
		t.Fatal("helper recovery changed completed retry metadata")
	}
}

func TestHelperRecoveryRejectsPendingApprovals(t *testing.T) {
	for _, target := range []update.Target{
		{State: update.Granted}, {State: update.Staged}, {State: update.Pending}, {State: update.Offline},
		{State: update.Failed, RetryPending: true}, {State: update.Failed, BootstrapRetry: true}, {State: "unknown"},
		{State: update.Cancelled, RetryPending: true}, {State: update.Cancelled, BootstrapRetry: true},
		{State: update.Updated, RetryPending: true}, {State: update.Newer, RetryPending: true},
		{State: update.Updated, BootstrapRetry: true}, {State: update.Newer, BootstrapRetry: true},
		{State: update.Updated, Grant: true}, {State: update.Newer, Grant: true},
		{State: update.Failed, Grant: true}, {State: update.Cancelled, Grant: true},
		{State: update.Failed}, {State: update.Cancelled}, {State: update.Updated}, {State: update.Newer},
	} {
		t.Run(fmt.Sprintf("%s/retry-%t/bootstrap-%t/grant-%t", target.State, target.RetryPending, target.BootstrapRetry, target.Grant), func(t *testing.T) {
			root, host := setupUpdateCLI(t)
			target.Host = host
			status := updateinstall.Status{Settings: updateinstall.Settings{StateDir: root}, Request: updateinstall.Request{TargetID: host.ID}}
			store, err := update.OpenStore(root)
			if err != nil {
				t.Fatal(err)
			}
			run, err := store.Start(host.ID, scopedUpdateFleet("recovery-fixture", []update.Host{host}), updateTestManifest())
			if err != nil {
				t.Fatal(err)
			}
			if _, err = store.Change(run.ID, func(run *update.Run) error { run.Targets[0] = target; return nil }); err != nil {
				t.Fatal(err)
			}
			err = checkHelperRecoveryApprovals(t.Context(), status)
			terminal := target.State == update.Failed || target.State == update.Cancelled || target.State == update.Updated || target.State == update.Newer
			verified := target.State == update.Updated || target.State == update.Newer
			allowed := terminal && (verified || !target.RetryPending) && !target.BootstrapRetry && !target.Grant
			if allowed != (err == nil) {
				t.Fatalf("approval boundary allowed=%v, error=%v", allowed, err)
			}
		})
	}
}

func TestHelperRecoveryRejectsCoordinatorSetup(t *testing.T) {
	root, host := setupUpdateCLI(t)
	store, err := update.OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.Start(host.ID, scopedUpdateFleet("recovery-setup-fixture", []update.Host{host}), updateTestManifest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Change(run.ID, func(run *update.Run) error {
		run.Targets[0].State = update.Updated
		run.CoordinatorSetup = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := helperRecoveryRunBytes(t, root, run.ID)
	status := updateinstall.Status{Settings: updateinstall.Settings{StateDir: root}, Request: updateinstall.Request{TargetID: host.ID}}
	if err = checkHelperRecoveryApprovals(t.Context(), status); err == nil {
		t.Fatal("completed target with pending coordinator setup was accepted")
	}
	after := helperRecoveryRunBytes(t, root, run.ID)
	if string(after) != string(before) {
		t.Fatal("helper recovery changed pending coordinator setup")
	}
}

func helperRecoveryRunBytes(t *testing.T, stateDir, runID string) []byte {
	t.Helper()
	root, err := os.OpenRoot(filepath.Join(stateDir, "updates", "runs"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile(runID + ".json")
	if err != nil {
		t.Fatal(err)
	}
	return data
}
