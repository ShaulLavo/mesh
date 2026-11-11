package release

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// RollbackProof is separate from the schema 1 receipt read by shipped clients.
// Its asset name is the SHA-256 of its bytes followed by .rollback.json.
type RollbackProof struct {
	Schema     int                `json:"schema"`
	Platform   Platform           `json:"platform"`
	FromDigest string             `json:"fromDigest"`
	ToDigest   string             `json:"toDigest"`
	Forward    string             `json:"forward"`
	Boundaries []RollbackBoundary `json:"boundaries"`
	Workers    []string           `json:"workers"`
}

type RollbackBoundary struct {
	State             int  `json:"state"`
	CandidateWrote    bool `json:"candidateWrote"`
	SourceStarted     bool `json:"sourceStarted"`
	SessionIO         bool `json:"sessionIO"`
	RecoveryPreserved bool `json:"recoveryPreserved"`
}

// VerifyRollbackProof binds complete rollback evidence to a verified forward receipt.
func VerifyRollbackProof(data []byte, digest string, forward []byte, transition Transition, compatibility Compatibility) (RollbackProof, error) {
	actual := sha256.Sum256(data)
	if !validDigest(digest) || hex.EncodeToString(actual[:]) != digest {
		return RollbackProof{}, errors.New("release: rollback proof SHA-256 differs from its reference")
	}
	receipt, err := VerifyTransitionReceipt(forward, transition, compatibility)
	if err != nil {
		return RollbackProof{}, fmt.Errorf("release: rollback forward receipt: %w", err)
	}
	var proof RollbackProof
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&proof); err != nil {
		return proof, fmt.Errorf("release: decode rollback proof: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return proof, errors.New("release: trailing rollback proof data")
	}
	if proof.Schema != 1 || proof.Platform != receipt.Platform || proof.FromDigest != receipt.FromDigest || proof.ToDigest != receipt.ToDigest || proof.Forward != transition.Proof {
		return proof, errors.New("release: rollback proof identity differs from its forward receipt")
	}
	if err := verifyRollbackBoundaries(proof.Boundaries, receipt.StateReadMin, receipt.StateWrite); err != nil {
		return proof, err
	}
	if err := verifyRollbackWorkers(proof.Workers, receipt.FromDigest); err != nil {
		return proof, err
	}
	return proof, nil
}

func verifyRollbackBoundaries(boundaries []RollbackBoundary, first, last int) error {
	if first <= 0 || last < first || len(boundaries)-1 != last-first {
		return errors.New("release: rollback proof does not cover the complete migration range")
	}
	seen := make(map[int]bool, len(boundaries))
	for _, boundary := range boundaries {
		if boundary.State < first || boundary.State > last || seen[boundary.State] {
			return errors.New("release: rollback proof has a duplicate or unexpected migration boundary")
		}
		if !boundary.CandidateWrote || !boundary.SourceStarted || !boundary.SessionIO || !boundary.RecoveryPreserved {
			return fmt.Errorf("release: rollback boundary %d did not pass every check", boundary.State)
		}
		seen[boundary.State] = true
	}
	return nil
}

func verifyRollbackWorkers(workers []string, source string) error {
	seen := make(map[string]bool, len(workers))
	for _, worker := range workers {
		if !validDigest(worker) || seen[worker] {
			return errors.New("release: rollback proof has an invalid or duplicate worker executable")
		}
		seen[worker] = true
	}
	if !seen[source] {
		return errors.New("release: rollback proof did not retain the source worker executable")
	}
	return nil
}
