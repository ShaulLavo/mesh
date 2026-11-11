package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/shaul/mesh/internal/release"
)

func verifyProofDirectory(directory string) error {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return fmt.Errorf("boundary: proof directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	paths, err := filepath.Glob(filepath.Join(directory, "rollback", "*.rollback.json"))
	if err != nil {
		return fmt.Errorf("boundary: rollback assets: %w", err)
	}
	if len(paths) == 0 {
		return errors.New("boundary: no rollback assets")
	}
	for _, path := range paths {
		if err := verifyRollbackAsset(root, filepath.Base(path)); err != nil {
			return err
		}
	}
	return nil
}

func verifyRollbackAsset(root *os.Root, name string) error {
	data, err := root.ReadFile(filepath.Join("rollback", name))
	if err != nil {
		return fmt.Errorf("boundary: read rollback asset: %w", err)
	}
	var proof release.RollbackProof
	if err := json.Unmarshal(data, &proof); err != nil {
		return fmt.Errorf("boundary: rollback identity: %w", err)
	}
	forward, err := root.ReadFile(proof.Forward + ".json")
	if err != nil {
		return fmt.Errorf("boundary: read forward asset: %w", err)
	}
	var receipt release.TransitionReceipt
	if err := json.Unmarshal(forward, &receipt); err != nil {
		return fmt.Errorf("boundary: forward identity: %w", err)
	}
	transition := release.Transition{Platform: receipt.Platform, FromDigest: receipt.FromDigest, ToDigest: receipt.ToDigest, Proof: proof.Forward}
	compatibility := release.Compatibility{StateReadMin: receipt.StateReadMin, StateReadMax: receipt.StateReadMax, StateWrite: receipt.StateWrite,
		WorkerMin: receipt.WorkerMin, WorkerMax: receipt.WorkerMax, WorkerWrite: receipt.WorkerWrite, JournalVersion: receipt.JournalVersion}
	if _, err := release.VerifyRollbackProof(data, strings.TrimSuffix(name, ".rollback.json"), forward, transition, compatibility); err != nil {
		return fmt.Errorf("boundary: verify rollback asset %s: %w", name, err)
	}
	return nil
}
