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

type TransitionReceipt struct {
	Schema                       int      `json:"schema"`
	Platform                     Platform `json:"platform"`
	FromDigest                   string   `json:"fromDigest"`
	ToDigest                     string   `json:"toDigest"`
	StateReadMin                 int      `json:"stateReadMin"`
	StateReadMax                 int      `json:"stateReadMax"`
	StateWrite                   int      `json:"stateWrite"`
	WorkerMin                    int      `json:"workerMin"`
	WorkerMax                    int      `json:"workerMax"`
	WorkerWrite                  int      `json:"workerWrite"`
	JournalVersion               int      `json:"journalVersion"`
	CandidateOpenedRetainedState bool     `json:"candidateOpenedRetainedState"`
	SessionsPreserved            bool     `json:"sessionsPreserved"`
	RecoveryRecordsPreserved     bool     `json:"recoveryRecordsPreserved"`
}

// VerifyTransitionReceipt applies the publication checks to content-addressed evidence.
func VerifyTransitionReceipt(data []byte, transition Transition, compatibility Compatibility) (TransitionReceipt, error) {
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != transition.Proof {
		return TransitionReceipt{}, errors.New("release: transition receipt SHA-256 differs from its manifest")
	}
	var proof TransitionReceipt
	if err := readReceiptJSON(data, &proof); err != nil {
		return proof, err
	}
	if proof.Schema != 1 || proof.Platform != transition.Platform || proof.FromDigest != transition.FromDigest || proof.ToDigest != transition.ToDigest {
		return proof, errors.New("release: transition receipt identity differs from its manifest")
	}
	if proof.StateReadMin <= 0 || proof.StateReadMin > compatibility.StateReadMax || proof.StateReadMax != compatibility.StateReadMax || proof.StateWrite != compatibility.StateWrite {
		return proof, errors.New("release: transition receipt state evidence differs from declared compatibility")
	}
	if proof.WorkerMin <= 0 || proof.WorkerMin > compatibility.WorkerMax || proof.WorkerMax != compatibility.WorkerMax || proof.WorkerWrite != compatibility.WorkerWrite || proof.JournalVersion != compatibility.JournalVersion {
		return proof, errors.New("release: transition receipt protocol evidence differs from declared compatibility")
	}
	if !proof.CandidateOpenedRetainedState || !proof.SessionsPreserved || !proof.RecoveryRecordsPreserved {
		return proof, errors.New("release: transition receipt did not pass every forward transition check")
	}
	return proof, nil
}

func readReceiptJSON(data []byte, proof *TransitionReceipt) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(proof); err != nil {
		return fmt.Errorf("release: decode transition receipt: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("release: trailing transition receipt data")
	}
	return nil
}
