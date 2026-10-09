package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func historicalBridgeFixture(t *testing.T) (map[string][]byte, Build, Manifest) {
	t.Helper()
	assets := make(map[string][]byte)
	for _, name := range []string{"v0.1.151.json", "v0.1.159.json", "8dda36b83d5f097a5297f2788762a6d443006b3c13a53755ae65f8f222e082e6.json", "54d13c1d74cb6235cfbeef2bbe89cc270a97c2e62b0075588c24908bdd46cb59.json"} {
		data, err := os.ReadFile(filepath.Join("testdata", "bridge", name)) //nolint:gosec // checked-in public release fixtures
		if err != nil {
			t.Fatal(err)
		}
		assets[name] = data
	}
	target, err := decodeManifest(assets["v0.1.159.json"])
	if err != nil {
		t.Fatal(err)
	}
	build := Build{Version: "v0.1.149", Digest: "ff0bb62b4ae33b0844eeca825a248c2b14fe616cff06e043a252821a1af92cbf",
		Platform: Platform{OS: "darwin", Arch: "arm64"}, StateVersion: 10, WorkerProtocol: 1, UpdateProtocol: 1}
	return assets, build, target
}

func bridgeOrigin(t *testing.T, assets map[string][]byte, history string) (Client, *atomic.Int32) {
	t.Helper()
	requests := new(atomic.Int32)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/history" {
			_, _ = w.Write([]byte(history))
			return
		}
		name := filepath.Base(r.URL.Path)
		if name == ManifestAssetName {
			name = filepath.Base(filepath.Dir(r.URL.Path)) + ".json"
		}
		data, ok := assets[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	}))
	t.Cleanup(server.Close)
	return Client{BaseURL: server.URL, HistoryAPI: server.URL + "/history", HTTPClient: server.Client()}, requests
}

func TestBridgePublishedHistoryAndExactReceipts(t *testing.T) {
	assets, build, target := historicalBridgeFixture(t)
	client, requests := bridgeOrigin(t, assets, `[{"tag_name":"v0.1.159"},{"tag_name":"v0.1.151"}]`)
	hop, err := client.Bridge(t.Context(), build, target)
	if err != nil || hop.Version != "v0.1.151" || requests.Load() != 4 {
		t.Fatalf("published bridge = %s, requests %d, %v", hop.Version, requests.Load(), err)
	}
	if target.Allows(build) == nil {
		t.Fatal("discovery changed direct admission")
	}
}

func TestBridgeRejectsUnverifiedEvidence(t *testing.T) {
	for _, scenario := range []string{"missing first receipt", "altered second receipt", "wrong platform", "wrong source digest", "unknown version", "modified build", "state outside exact receipt", "tag mismatch", "unsupported journal", "draft", "prerelease"} {
		t.Run(scenario, func(t *testing.T) {
			assets, build, target := historicalBridgeFixture(t)
			history := `[{"tag_name":"v0.1.151"}]`
			mutateBridgeEvidence(t, scenario, assets, &build, &target, &history)
			client, _ := bridgeOrigin(t, assets, history)
			if hop, err := client.Bridge(t.Context(), build, target); err == nil {
				t.Fatalf("%s yielded %s", scenario, hop.Version)
			}
		})
	}
}

func mutateBridgeEvidence(t *testing.T, scenario string, assets map[string][]byte, build *Build, target *Manifest, history *string) {
	t.Helper()
	switch scenario {
	case "missing first receipt":
		delete(assets, "8dda36b83d5f097a5297f2788762a6d443006b3c13a53755ae65f8f222e082e6.json")
	case "altered second receipt":
		assets["54d13c1d74cb6235cfbeef2bbe89cc270a97c2e62b0075588c24908bdd46cb59.json"] = []byte(`{}`)
	case "wrong platform":
		build.Platform = Platform{OS: "linux", Arch: "arm64"}
	case "wrong source digest":
		build.Digest = strings.Repeat("b", 64)
	case "unknown version":
		build.Version = "development"
	case "modified build":
		build.Modified = true
	case "state outside exact receipt":
		build.StateVersion = 9
		target.Compatibility.StateReadMin = 9
		hop, err := decodeManifest(assets["v0.1.151.json"])
		if err != nil {
			t.Fatal(err)
		}
		hop.Compatibility.StateReadMin = 9
		assets["v0.1.151.json"], _ = json.Marshal(hop)
	case "tag mismatch":
		hop, err := decodeManifest(assets["v0.1.151.json"])
		if err != nil {
			t.Fatal(err)
		}
		hop.Version = "v0.1.152"
		assets["v0.1.151.json"], _ = json.Marshal(hop)
	case "unsupported journal":
		target.Compatibility.JournalVersion = 2
	case "draft":
		*history = `[{"tag_name":"v0.1.151","draft":true}]`
	case "prerelease":
		*history = `[{"tag_name":"v0.1.151","prerelease":true}]`
	}
}

func TestBridgeHistoryIsBoundedAndNeverInfersTags(t *testing.T) {
	assets, build, target := historicalBridgeFixture(t)
	history := "[" + strings.Repeat(`{"tag_name":"v0.1.150"},`, bridgeReleaseLimit) + `{"tag_name":"v0.1.151"}]`
	client, requests := bridgeOrigin(t, assets, history)
	if _, err := client.Bridge(t.Context(), build, target); err == nil || requests.Load() != 2 {
		t.Fatalf("searched outside the first history page or repeated tags: requests %d, %v", requests.Load(), err)
	}
	client, requests = bridgeOrigin(t, assets, `[]`)
	if _, err := client.Bridge(t.Context(), build, target); err == nil || requests.Load() != 1 {
		t.Fatalf("inferred tags without history: requests %d, %v", requests.Load(), err)
	}
}

func TestBridgeHonorsCancellation(t *testing.T) {
	_, build, target := historicalBridgeFixture(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	client := Client{BaseURL: server.URL, HistoryAPI: server.URL + "/history", HTTPClient: server.Client()}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := client.Bridge(ctx, build, target); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("bridge exceeded caller budget: %v", err)
	}
}

func TestBridgeUsesDiscoveredManifestVersion(t *testing.T) {
	assets, build, target := historicalBridgeFixture(t)
	hop, err := decodeManifest(assets["v0.1.151.json"])
	if err != nil {
		t.Fatal(err)
	}
	hop.Version = "v0.1.153"
	assets["v0.1.153.json"], err = json.Marshal(hop)
	if err != nil {
		t.Fatal(err)
	}
	delete(assets, "v0.1.151.json")
	client, _ := bridgeOrigin(t, assets, `[{"tag_name":"v0.1.153"}]`)
	got, err := client.Bridge(t.Context(), build, target)
	if err != nil || got.Version != "v0.1.153" {
		t.Fatalf("discovered manifest version = %s, %v", got.Version, err)
	}
}

func TestVerifyTransitionReceiptRejectsContentAddressedFalseClaims(t *testing.T) {
	for _, field := range []string{"schema", "platform", "fromDigest", "toDigest", "stateReadMin", "stateReadMax", "stateWrite", "workerMin", "workerMax", "workerWrite", "journalVersion", "candidateOpenedRetainedState", "sessionsPreserved", "recoveryRecordsPreserved", "unknownField"} {
		t.Run(field, func(t *testing.T) {
			assets, build, _ := historicalBridgeFixture(t)
			hop, err := decodeManifest(assets["v0.1.151.json"])
			if err != nil {
				t.Fatal(err)
			}
			var transition Transition
			for _, candidate := range hop.Compatibility.Transitions {
				if candidate.Platform == build.Platform && candidate.FromDigest == build.Digest {
					transition = candidate
				}
			}
			var claims map[string]any
			if err := json.Unmarshal(assets[transition.Proof+".json"], &claims); err != nil {
				t.Fatal(err)
			}
			claims[field] = invalidReceiptClaim(claims[field])
			data, err := json.Marshal(claims)
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(data)
			transition.Proof = hex.EncodeToString(digest[:])
			if _, err := VerifyTransitionReceipt(data, transition, hop.Compatibility); err == nil {
				t.Fatalf("receipt with invalid %s accepted despite a matching hash", field)
			}
		})
	}
}

func invalidReceiptClaim(value any) any {
	switch claim := value.(type) {
	case float64:
		return claim + 1
	case string:
		return strings.Repeat("b", 64)
	case map[string]any:
		return map[string]any{"os": "linux", "arch": "arm64"}
	default:
		return false
	}
}
