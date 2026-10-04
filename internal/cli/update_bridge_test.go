package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/updatebootstrap"
	"github.com/shaul/mesh/internal/updateinstall"
)

func publishedBridgeFixture(t *testing.T) (Dependencies, update.Host) {
	t.Helper()
	_, local := setupUpdateCLI(t)
	root := filepath.Join("..", "release", "testdata", "bridge")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/history" {
			_, _ = w.Write([]byte(`[{"tag_name":"v0.1.159"},{"tag_name":"v0.1.151"}]`))
			return
		}
		name := filepath.Base(r.URL.Path)
		if name == release.ManifestAssetName {
			name = filepath.Base(filepath.Dir(r.URL.Path)) + ".json"
		}
		data, err := os.ReadFile(filepath.Join(root, name)) //nolint:gosec // fixed checked-in public metadata fixtures
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data) //nolint:gosec // serves immutable JSON fixtures over a test-only origin
	}))
	t.Cleanup(server.Close)
	caller := updateCallFunc(func(_ context.Context, host update.Host, action string, _, output any) error {
		if action != "info" {
			t.Fatalf("bridge guidance sent mutation %s", action)
		}
		build := release.Build{Version: "v0.1.149", Digest: "ff0bb62b4ae33b0844eeca825a248c2b14fe616cff06e043a252821a1af92cbf",
			Platform: release.Platform{OS: "darwin", Arch: "arm64"}, StateVersion: 10, WorkerProtocol: 1, UpdateProtocol: 1}
		*output.(*update.Info) = update.Info{AcceptsIdentityFleet: true, Health: updateinstall.Health{HostID: host.ID, Build: build}}
		return nil
	})
	return Dependencies{UpdateRelease: release.Client{BaseURL: server.URL, HistoryAPI: server.URL + "/history", HTTPClient: server.Client()}, UpdateCaller: caller,
		UpdateBridgePreflight: func(context.Context, string, update.Target, release.Manifest) error { return nil }}, local
}

func TestUpdatePublishedBridgeGuidance(t *testing.T) {
	dependencies, _ := publishedBridgeFixture(t)
	text, _, err := executeCommand(t, dependencies, "update", "--local", "--version", "v0.1.159", "--check")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Published Mac bridge fixture, readiness provider is a fixture:\n%s", text)
	want := "Update in steps: first run mesh update --local --version v0.1.151 on this Mac."
	if !strings.Contains(text, want) {
		t.Fatalf("verified next-hop command missing; want %q", want)
	}
	if strings.Contains(text, "--local --check") || strings.Contains(text, "ff0bb62") {
		t.Fatal("bridge summary includes another action or technical identifiers")
	}
}

func TestUpdateBridgeInteractiveStillRequiresSeparateStep(t *testing.T) {
	dependencies, _ := publishedBridgeFixture(t)
	text, err := interactiveUpdateFlow(t, dependencies, "y\n", "update", "--local", "--version", "v0.1.159")
	if err == nil || strings.Contains(text, "[y/N]") || !strings.Contains(text, "Update in steps: first run mesh update --local --version v0.1.151 on this Mac.") {
		t.Fatalf("interactive direct approval = %s, %v", text, err)
	}
}

func TestUpdatePublishedBridgeStillBlocksDirectApproval(t *testing.T) {
	dependencies, local := publishedBridgeFixture(t)
	text, _, err := executeCommand(t, dependencies, "update", "--local", "--version", "v0.1.159", "--yes", "--json")
	if err == nil {
		t.Fatal("bridge suggestion authorized the untested direct update")
	}
	var preview updatePreview
	if err := json.Unmarshal([]byte(text), &preview); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(preview.ApprovalProblem, "--version v0.1.151") || !strings.Contains(preview.Targets[0].Problem, "no tested darwin/arm64 transition") {
		t.Fatal("suggestion or original admission cause lost")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(strings.TrimPrefix(local.Endpoint, "unix://")), "updates", "runs")); !errors.Is(err, os.ErrNotExist) { //nolint:gosec // fixture state directory
		t.Fatalf("guidance created an operation: %v", err)
	}
}

func TestUpdateBridgeReadinessAndRecoveryTakePriority(t *testing.T) {
	for _, scenario := range []string{"helper recovery", "service prerequisite", "rollback failed", "modified build", "worker protocol"} {
		t.Run(scenario, func(t *testing.T) {
			dependencies, _ := publishedBridgeFixture(t)
			preflights := 0
			dependencies.UpdateBridgePreflight = func(context.Context, string, update.Target, release.Manifest) error {
				preflights++
				return errors.New("fixture " + scenario + " requires owner review")
			}
			caller := dependencies.UpdateCaller
			dependencies.UpdateCaller = updateCallFunc(func(ctx context.Context, host update.Host, action string, input, output any) error {
				if err := caller.Call(ctx, host, action, input, output); err != nil {
					return fmt.Errorf("inspect fixture bridge source: %w", err)
				}
				info := output.(*update.Info)
				if scenario == "rollback failed" {
					info.Installation = &updateinstall.Status{Phase: updateinstall.RollbackFailed}
				}
				if scenario == "modified build" {
					info.Health.Build.Modified = true
				}
				if scenario == "worker protocol" {
					info.Health.Workers = []updateinstall.Worker{{Protocol: 0}}
				}
				return nil
			})
			text, _, err := executeCommand(t, dependencies, "update", "--local", "--version", "v0.1.159", "--check", "--details")
			if err != nil || strings.Contains(text, "Update in steps") {
				t.Fatalf("unsafe bridge suggestion: %s, %v", text, err)
			}
			if scenario == "rollback failed" && (!strings.Contains(text, "review recovery") || preflights != 0) {
				t.Fatal("bridge discovery superseded recovery")
			}
			if (scenario == "modified build" || scenario == "worker protocol") && preflights != 0 {
				t.Fatal("inadmissible source reached helper preflight")
			}
		})
	}
}

func TestUpdateBridgeRemoteRequiresDestinationReview(t *testing.T) {
	dependencies, _ := publishedBridgeFixture(t)
	remote := namedDestinationPeer(t)
	if err := saveNamedTestHost(t, remote.host); err != nil {
		t.Fatal(err)
	}
	dependencies.UpdateBridgePreflight = func(context.Context, string, update.Target, release.Manifest) error {
		t.Fatal("remote guidance borrowed local helper readiness")
		return nil
	}
	text, _, err := executeCommand(t, dependencies, "update", "--host", remote.host.ID, "--version", "v0.1.159", "--check")
	if err != nil || strings.Contains(text, "Update in steps") || !strings.Contains(text, "Run mesh update --local --check on ") {
		t.Fatalf("remote destination action = %s, %v", text, err)
	}
}

func TestUpdateBridgeDetailsContainVerifiedEvidence(t *testing.T) {
	dependencies, _ := publishedBridgeFixture(t)
	text, _, err := executeCommand(t, dependencies, "update", "--local", "--version", "v0.1.159", "--check", "--details")
	if err != nil || !strings.Contains(text, "Verified next hop: Mesh v0.1.151") || !strings.Contains(text, "no tested darwin/arm64 transition") {
		t.Fatalf("bridge details missing: %s, %v", text, err)
	}
}

func TestUpdateBridgeArchiveProofDoesNotSupplyHelperReadiness(t *testing.T) {
	dependencies, local := publishedBridgeFixture(t)
	var info update.Info
	if err := dependencies.UpdateCaller.Call(t.Context(), local, "info", nil, &info); err != nil {
		t.Fatal(err)
	}
	dependencies.UpdateBridgePreflight = nil
	dependencies.UpdateInspect = func(context.Context, string) (updatebootstrap.Observation, error) {
		return updatebootstrap.Observation{Executable: filepath.Join(t.TempDir(), "mesh"), Health: info.Health}, nil
	}
	text, _, err := executeCommand(t, dependencies, "update", "--local", "--version", "v0.1.159", "--check")
	if err != nil || strings.Contains(text, "Update in steps") || !strings.Contains(text, "could not establish a tested runnable update path") || !strings.Contains(text, "--local --check") {
		t.Fatalf("archive-only readiness guidance = %s, %v", text, err)
	}
}

func TestUpdateBridgeBootstrapKeepsItsOwner(t *testing.T) {
	for _, clientOnly := range []bool{true, false} {
		dependencies, local := publishedBridgeFixture(t)
		var info update.Info
		if err := dependencies.UpdateCaller.Call(t.Context(), local, "info", nil, &info); err != nil {
			t.Fatal(err)
		}
		manifest, err := dependencies.UpdateRelease.Manifest(t.Context(), "v0.1.159")
		if err != nil {
			t.Fatal(err)
		}
		dependencies.UpdateBridgePreflight = func(context.Context, string, update.Target, release.Manifest) error {
			t.Fatal("bootstrap borrowed readiness from a different installation owner")
			return nil
		}
		preview := prepareUpdateApproval(updatePreview{Release: manifest, ClientOnly: clientOnly, CoordinatorBootstrap: !clientOnly,
			Targets: []update.Target{{Host: local, State: update.Pending, Build: &info.Health.Build}}})
		a := application{dependencies: dependencies}
		preview = a.reviewUpdateBridges(t.Context(), updateEnvironment{local: local}, preview)
		if preview.ApprovalProblem == "" || strings.Contains(preview.ApprovalProblem, "Update in steps") {
			t.Fatal("bootstrap transition was authorized through bridge discovery")
		}
	}
}
