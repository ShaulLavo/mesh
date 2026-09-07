package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/updateinstall"
)

func localApprovalFixture(t *testing.T) (*updateinstall.Engine, updateinstall.Status) {
	t.Helper()
	stateDir, local := setupUpdateCLI(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	settings := updateinstall.Settings{StateDir: stateDir, Executable: executable, CacheDir: filepath.Join(stateDir, "cache"), ClientOnly: true}
	status := updateinstall.Status{Schema: 1, Phase: updateinstall.Staged, Settings: settings, Request: updateinstall.Request{ID: strings.Repeat("a", 32), TargetID: local.ID, Generation: 7, Manifest: updateTestManifest(), Current: release.Current()}}
	writeApprovalJournal(t, status)
	engine, err := updateinstall.New(settings.Config())
	if err != nil {
		t.Fatal(err)
	}
	return engine, status
}

func writeApprovalJournal(t *testing.T, status updateinstall.Status) {
	t.Helper()
	if !filepath.IsAbs(status.Settings.StateDir) {
		t.Fatal("test installation journal requires an absolute isolated state directory")
	}
	path := filepath.Join(status.Settings.StateDir, "update", "installation.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestClientOnlyApprovalSurvivesInitiatingProcessAndCannotOverrideCancellation(t *testing.T) {
	engine, status := localApprovalFixture(t)
	if err := saveClientOnlyApproval(status.Settings.StateDir, status.Request); err != nil {
		t.Fatal(err)
	}
	granted, err := grantApprovedClientOnlyUpdate(context.Background(), engine, status)
	if err != nil || granted.Phase != updateinstall.Granted {
		t.Fatalf("persisted approval did not authorize staged update: %s, %v", granted.Phase, err)
	}
	status.Phase = updateinstall.Cancelled
	writeApprovalJournal(t, status)
	stale := status
	stale.Phase = updateinstall.Staged
	if _, err := grantApprovedClientOnlyUpdate(context.Background(), engine, stale); !errors.Is(err, updateinstall.ErrStaleGeneration) {
		t.Fatalf("stale helper approval overrode durable cancellation: %v", err)
	}
}

func TestClientOnlyApprovalCannotAuthorizeAnotherReleaseOrGeneration(t *testing.T) {
	engine, status := localApprovalFixture(t)
	wrong := status.Request
	wrong.Generation++
	if err := saveClientOnlyApproval(status.Settings.StateDir, wrong); err != nil {
		t.Fatal(err)
	}
	if _, err := grantApprovedClientOnlyUpdate(context.Background(), engine, status); err == nil {
		t.Fatal("another generation's approval activated this request")
	}
	current, err := updateinstall.Read(status.Settings.StateDir)
	if err != nil || current.Phase != updateinstall.Staged {
		t.Fatalf("mismatched approval changed installation: %s, %v", current.Phase, err)
	}
}

func TestUnfinishedFleetNoticeUsesDurableStateWithoutReleaseLookup(t *testing.T) {
	stateDir, local := setupUpdateCLI(t)
	store, err := update.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Start(local.ID, scopedUpdateFleet("saved", []update.Host{local}), updateTestManifest()); err != nil {
		t.Fatal(err)
	}
	if text := pendingUpdateNotice(stateDir); !strings.Contains(text, "unfinished") {
		t.Fatalf("pending operation disappeared: %q", text)
	}
}
