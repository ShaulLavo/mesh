package release

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func historicalBridgeFixture(t *testing.T) (map[string][]byte, Build) {
	t.Helper()
	assets := make(map[string][]byte)
	for _, name := range []string{"v0.1.151.json", "v0.1.159.json", "8dda36b83d5f097a5297f2788762a6d443006b3c13a53755ae65f8f222e082e6.json", "54d13c1d74cb6235cfbeef2bbe89cc270a97c2e62b0075588c24908bdd46cb59.json"} {
		data, err := os.ReadFile(filepath.Join("testdata", "bridge", name)) //nolint:gosec // checked-in public release fixtures
		if err != nil {
			t.Fatal(err)
		}
		assets[name] = data
	}
	build := Build{Version: "v0.1.149", Digest: "ff0bb62b4ae33b0844eeca825a248c2b14fe616cff06e043a252821a1af92cbf",
		Platform: Platform{OS: "darwin", Arch: "arm64"}, StateVersion: 10, WorkerProtocol: 1, UpdateProtocol: 1}
	return assets, build
}

func TestVerifyTransitionReceiptRejectsContentAddressedFalseClaims(t *testing.T) {
	for _, field := range []string{"schema", "platform", "fromDigest", "toDigest", "stateReadMin", "stateReadMax", "stateWrite", "workerMin", "workerMax", "workerWrite", "journalVersion", "candidateOpenedRetainedState", "sessionsPreserved", "recoveryRecordsPreserved", "unknownField"} {
		t.Run(field, func(t *testing.T) {
			assets, build := historicalBridgeFixture(t)
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
