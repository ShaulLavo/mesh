package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/updateinstall"
)

type updateCallFunc func(context.Context, update.Host, string, any, any) error

func (f updateCallFunc) Call(ctx context.Context, host update.Host, action string, input, output any) error {
	return f(ctx, host, action, input, output)
}

func updateTestManifest() release.Manifest {
	manifest := release.Manifest{Schema: 1, Version: "v0.2.0", Commit: strings.Repeat("a", 40), Compatibility: release.Compatibility{StateReadMin: 1, StateReadMax: 1, StateWrite: 1, WorkerMin: 1, WorkerMax: 1, WorkerWrite: 1, JournalVersion: 1}}
	for _, platform := range []release.Platform{{OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"}, {OS: "darwin", Arch: "arm64"}} {
		manifest.Artifacts = append(manifest.Artifacts, release.Artifact{Platform: platform, Archive: "mesh_" + platform.OS + "_" + platform.Arch + ".tar.gz", SHA256: strings.Repeat("b", 64), BinarySHA256: strings.Repeat("c", 64)})
	}
	return manifest
}

func updateTestRelease(t *testing.T) (release.Client, *atomic.Int32) {
	t.Helper()
	requests := new(atomic.Int32)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_ = json.NewEncoder(w).Encode(updateTestManifest())
	}))
	t.Cleanup(server.Close)
	return release.Client{BaseURL: server.URL, HTTPClient: server.Client()}, requests
}

func setupUpdateCLI(t *testing.T) (string, update.Host) {
	t.Helper()
	stateDir := t.TempDir()
	t.Setenv("MESH_STATE_DIR", stateDir)
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	host, _, err := identity.LoadOrCreate(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	return stateDir, update.LocalHost(stateDir, host.ID)
}

func TestUpdateNoninteractiveApprovalAndScopeAreRequiredBeforeNetwork(t *testing.T) {
	setupUpdateCLI(t)
	client, requests := updateTestRelease(t)
	for _, args := range [][]string{{"update"}, {"update", "--yes"}, {"update", "--local", "--host", "remote", "--yes"}, {"update", "--local", "--version", "../../bad", "--yes"}} {
		_, _, err := executeCommand(t, Dependencies{UpdateRelease: client}, args...)
		if err == nil {
			t.Fatalf("unsafe or ambiguous arguments accepted: %v", args)
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid arguments contacted release server %d times", requests.Load())
	}
}

func TestUpdateCheckReturnsJSONWithoutMutationOrBackgroundNotice(t *testing.T) {
	stateDir, _ := setupUpdateCLI(t)
	client, requests := updateTestRelease(t)
	caller := updateCallFunc(func(_ context.Context, host update.Host, action string, input, output any) error {
		if action != "info" {
			t.Errorf("--check sent mutation %s", action)
			return errors.New("unexpected mutation")
		}
		*output.(*update.Info) = update.Info{Health: updateinstall.Health{HostID: host.ID, Build: release.Build{Version: "v0.1.0", Platform: release.CurrentPlatform()}}}
		return nil
	})
	stdout, stderr, err := executeCommand(t, Dependencies{UpdateRelease: client, UpdateCaller: caller}, "update", "--local", "--check", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var preview updatePreview
	if err := json.Unmarshal([]byte(stdout), &preview); err != nil {
		t.Fatalf("invalid JSON %q: %v", stdout, err)
	}
	if stderr != "" || preview.Release.Version != "v0.2.0" || len(preview.Targets) != 1 || requests.Load() != 1 {
		t.Fatalf("check polluted output or repeated network: stderr=%q, preview=%+v, requests=%d", stderr, preview, requests.Load())
	}
	if _, err := os.Stat(filepath.Join(stateDir, "update")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("check created installation state: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "update-notice", "notice.json")); err != nil {
		t.Fatalf("explicit check did not update availability cache: %v", err)
	}
}

func TestUpdateKeepsOfflineMembersAndSubmitsOnePinnedPlan(t *testing.T) {
	_, local := setupUpdateCLI(t)
	remoteIdentity, _, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	remote := update.Host{ID: remoteIdentity.ID, Alias: "laptop", Endpoint: "ws://laptop.invalid/mesh"}
	fleet := scopedUpdateFleet("test", []update.Host{local, remote})
	file := filepath.Join(t.TempDir(), "fleet.json")
	if err := update.SaveFleet(file, fleet); err != nil {
		t.Fatal(err)
	}
	client, _ := updateTestRelease(t)
	var plans atomic.Int32
	caller := updateCallFunc(func(_ context.Context, host update.Host, action string, input, output any) error {
		if action == "info" {
			if host.ID == remote.ID {
				return context.DeadlineExceeded
			}
			*output.(*update.Info) = update.Info{Health: updateinstall.Health{HostID: host.ID, Build: release.Build{Version: "v0.1.0"}}}
			return nil
		}
		if action != "plan" {
			return errors.New("unexpected updater action")
		}
		plans.Add(1)
		plan := input.(update.Plan)
		if plan.Fleet.Digest() != fleet.Digest() || plan.Manifest.Digest() != updateTestManifest().Digest() {
			return errors.New("plan lost approved membership or release")
		}
		*output.(*update.Run) = update.Run{ID: strings.Repeat("a", 32), Fleet: plan.Fleet, Release: plan.Manifest, Targets: []update.Target{{Host: local, State: update.Updated}, {Host: remote, State: update.Offline}}}
		return nil
	})
	stdout, stderr, err := executeCommand(t, Dependencies{UpdateRelease: client, UpdateCaller: caller}, "update", "--fleet", file, "--yes", "--json")
	if code, ok := StatusCode(err); !ok || code != 2 {
		t.Fatalf("pending fleet status = %v", err)
	}
	var run update.Run
	if err := json.Unmarshal([]byte(stdout), &run); err != nil {
		t.Fatal(err)
	}
	if len(run.Targets) != 2 || run.Targets[1].State != update.Offline || plans.Load() != 1 || stderr != "" {
		t.Fatalf("offline host omitted or output polluted: %+v, plans %d, stderr %q", run, plans.Load(), stderr)
	}
}

func TestUpdatePreviewSeparatesLegacyBootstrapFromRefusedAuthority(t *testing.T) {
	if !legacyUpdateUnavailable(&update.RemoteError{Problem: `daemon: unknown control "update.control"`}) {
		t.Fatal("legacy daemon was not recognized")
	}
	for _, err := range []error{&update.RemoteError{Problem: "update administrator is not authorized"}, &update.RemoteError{Problem: "invalid update signature"}, errors.New("unknown control update.control")} {
		if legacyUpdateUnavailable(err) {
			t.Fatalf("authentication or local error permits bootstrap: %v", err)
		}
	}
}

func TestVersionJSONAndHiddenHelperValidationAreQuiet(t *testing.T) {
	setupUpdateCLI(t)
	stdout, stderr, err := executeCommand(t, Dependencies{}, "version", "--json")
	var build release.Build
	if err != nil || json.Unmarshal([]byte(stdout), &build) != nil || stderr != "" || build.Platform != release.CurrentPlatform() {
		t.Fatalf("version result %q, %q, %v", stdout, stderr, err)
	}
	_, _, err = executeCommand(t, Dependencies{}, "update-helper", "--state-dir", "relative", "--check-journal")
	if err == nil {
		t.Fatal("relative helper state accepted")
	}
}

func TestWorkerUpdateReportDoesNotClaimOldSessionsWereUpgraded(t *testing.T) {
	workers := []updateinstall.Worker{{ID: "old", Build: &release.Build{Version: "v0.1.0"}}, {ID: "unknown"}, {ID: "current", Build: &release.Build{Version: "v0.2.0"}}}
	text := workerUpdateSummary(workers, updateTestManifest())
	for _, want := range []string{"3 running sessions preserved", "1 use older workers", "1 worker versions unknown"} {
		if !strings.Contains(text, want) {
			t.Fatalf("worker summary lacks %q: %s", want, text)
		}
	}
}

func TestPickerUpdateCannotBeCombinedWithSessionMutation(t *testing.T) {
	if err := validatePickerSelection(PickerSelection{ReviewUpdate: true, SessionID: "BVMX"}); err == nil {
		t.Fatal("ambiguous update selection accepted")
	}
	if err := validatePickerSelection(PickerSelection{ReviewUpdate: true}); err != nil {
		t.Fatal(err)
	}
}
