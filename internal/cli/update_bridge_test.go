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
	"testing"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/update"
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
