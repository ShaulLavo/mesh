package release

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func rollbackFixture(t *testing.T) (RollbackProof, []byte, Transition, Compatibility) {
	t.Helper()
	transition := Transition{Platform: Platform{OS: "linux", Arch: "amd64"}, FromDigest: strings.Repeat("a", 64), ToDigest: strings.Repeat("b", 64)}
	compatibility := Compatibility{StateReadMin: 14, StateReadMax: 16, StateWrite: 16, WorkerMin: 1, WorkerMax: 1, WorkerWrite: 1, JournalVersion: 1}
	receipt := TransitionReceipt{Schema: 1, Platform: transition.Platform, FromDigest: transition.FromDigest, ToDigest: transition.ToDigest,
		StateReadMin: 14, StateReadMax: 16, StateWrite: 16, WorkerMin: 1, WorkerMax: 1, WorkerWrite: 1, JournalVersion: 1,
		CandidateOpenedRetainedState: true, SessionsPreserved: true, RecoveryRecordsPreserved: true}
	forward, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	transition.Proof = rollbackDigest(forward)
	proof := RollbackProof{Schema: 1, Platform: transition.Platform, FromDigest: transition.FromDigest, ToDigest: transition.ToDigest,
		Forward: transition.Proof, Workers: []string{transition.FromDigest}}
	for state := 14; state <= 16; state++ {
		proof.Boundaries = append(proof.Boundaries, RollbackBoundary{state, true, true, true, true})
	}
	return proof, forward, transition, compatibility
}

func rollbackDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func TestVerifyRollbackProof(t *testing.T) {
	proof, forward, transition, compatibility := rollbackFixture(t)
	data, err := json.Marshal(proof)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := VerifyRollbackProof(data, rollbackDigest(data), forward, transition, compatibility)
	if err != nil || len(verified.Boundaries) != 3 {
		t.Fatalf("complete rollback proof: %+v, %v", verified, err)
	}
	if _, err := VerifyRollbackProof(append(data, ' '), rollbackDigest(data), forward, transition, compatibility); err == nil {
		t.Fatal("accepted changed content-addressed rollback bytes")
	}
	if _, err := VerifyRollbackProof(data, rollbackDigest(data), append(forward, ' '), transition, compatibility); err == nil {
		t.Fatal("accepted changed forward receipt bytes")
	}
}

func TestVerifyRollbackProofRejectsIncompleteOrUnrelatedEvidence(t *testing.T) {
	cases := map[string]func(*RollbackProof){
		"schema":                  func(p *RollbackProof) { p.Schema = 2 },
		"missing initial":         func(p *RollbackProof) { p.Boundaries = p.Boundaries[1:] },
		"missing middle":          func(p *RollbackProof) { p.Boundaries = append(p.Boundaries[:1], p.Boundaries[2:]...) },
		"missing final":           func(p *RollbackProof) { p.Boundaries = p.Boundaries[:2] },
		"duplicate boundary":      func(p *RollbackProof) { p.Boundaries[1] = p.Boundaries[0] },
		"outside range":           func(p *RollbackProof) { p.Boundaries[0].State = 13 },
		"candidate did not write": func(p *RollbackProof) { p.Boundaries[1].CandidateWrote = false },
		"source did not start":    func(p *RollbackProof) { p.Boundaries[1].SourceStarted = false },
		"no session IO":           func(p *RollbackProof) { p.Boundaries[1].SessionIO = false },
		"changed recovery":        func(p *RollbackProof) { p.Boundaries[1].RecoveryPreserved = false },
		"forward identity":        func(p *RollbackProof) { p.Forward = strings.Repeat("c", 64) },
		"platform":                func(p *RollbackProof) { p.Platform.Arch = "arm64" },
		"source":                  func(p *RollbackProof) { p.FromDigest = strings.Repeat("c", 64) },
		"target":                  func(p *RollbackProof) { p.ToDigest = strings.Repeat("c", 64) },
		"no retained worker":      func(p *RollbackProof) { p.Workers = nil },
		"wrong retained worker":   func(p *RollbackProof) { p.Workers[0] = strings.Repeat("c", 64) },
		"duplicate worker":        func(p *RollbackProof) { p.Workers = append(p.Workers, p.Workers[0]) },
		"invalid worker":          func(p *RollbackProof) { p.Workers = append(p.Workers, "invalid") },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) { checkInvalidRollback(t, mutate) })
	}
}

func checkInvalidRollback(t *testing.T, mutate func(*RollbackProof)) {
	t.Helper()
	proof, forward, transition, compatibility := rollbackFixture(t)
	mutate(&proof)
	data, err := json.Marshal(proof)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyRollbackProof(data, rollbackDigest(data), forward, transition, compatibility); err == nil {
		t.Fatal("accepted incomplete or unrelated rollback evidence")
	}
}

func TestVerifyRollbackProofStrictJSON(t *testing.T) {
	proof, forward, transition, compatibility := rollbackFixture(t)
	data, err := json.Marshal(proof)
	if err != nil {
		t.Fatal(err)
	}
	for _, contents := range [][]byte{append(data, []byte(" {}")...), append([]byte(`{"unknown":true,`), data[1:]...)} {
		if _, err := VerifyRollbackProof(contents, rollbackDigest(contents), forward, transition, compatibility); err == nil {
			t.Fatal("accepted trailing or unknown rollback data")
		}
	}
}
