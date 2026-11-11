package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/release"
)

func TestProofDirectoryByteBounds(t *testing.T) {
	for _, oversized := range []string{"", "rollback", "forward"} {
		t.Run(oversized, func(t *testing.T) { checkProofDirectoryBound(t, oversized) })
	}
}

func checkProofDirectoryBound(t *testing.T, oversized string) {
	t.Helper()
	directory := t.TempDir()
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err = root.Mkdir("rollback", 0o700); err != nil {
		t.Fatal(err)
	}
	receipt := release.TransitionReceipt{Schema: 1, Platform: release.Platform{OS: "linux", Arch: "amd64"},
		FromDigest: strings.Repeat("a", 64), ToDigest: strings.Repeat("b", 64), StateReadMin: 1, StateReadMax: 1, StateWrite: 1,
		WorkerMin: 1, WorkerMax: 1, WorkerWrite: 1, JournalVersion: 1,
		CandidateOpenedRetainedState: true, SessionsPreserved: true, RecoveryRecordsPreserved: true}
	forward := paddedProof(t, receipt, oversized == "forward")
	forwardDigest := proofDigest(forward)
	if err = root.WriteFile(forwardDigest+".json", forward, 0o600); err != nil {
		t.Fatal(err)
	}
	proof := release.RollbackProof{Schema: 1, Platform: receipt.Platform, FromDigest: receipt.FromDigest, ToDigest: receipt.ToDigest,
		Forward: forwardDigest, Workers: []string{receipt.FromDigest},
		Boundaries: []release.RollbackBoundary{{State: 1, CandidateWrote: true, SourceStarted: true, SessionIO: true, RecoveryPreserved: true}}}
	data := paddedProof(t, proof, oversized == "rollback")
	if err = root.WriteFile(filepath.Join("rollback", proofDigest(data)+".rollback.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	err = verifyProofDirectory(directory)
	if oversized == "" && err != nil {
		t.Fatalf("maximum-sized proof files rejected: %v", err)
	}
	if oversized != "" && (err == nil || !strings.Contains(err.Error(), "exceeds")) {
		t.Fatalf("oversized %s file accepted or parsed: %v", oversized, err)
	}
}

func paddedProof(t *testing.T, value any, oversized bool) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	size := release.MaxTransitionProofBytes
	if oversized {
		size++
	}
	return append(data, []byte(strings.Repeat(" ", size-len(data)))...)
}

func proofDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
