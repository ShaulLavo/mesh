package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/updateinstall"
)

type clientOnlyApproval struct {
	ID             string `json:"id"`
	TargetID       string `json:"targetId"`
	Generation     uint64 `json:"generation"`
	ManifestDigest string `json:"manifestDigest"`
}

func clientOnlyApprovalPath(stateDir, id string) string {
	return filepath.Join(stateDir, "update", "local-approvals", id+".json")
}

func saveClientOnlyApproval(stateDir string, request updateinstall.Request) error {
	if !update.ValidRunID(request.ID) {
		return errors.New("invalid local update approval ID")
	}
	path := clientOnlyApprovalPath(stateDir, request.ID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(clientOnlyApproval{ID: request.ID, TargetID: request.TargetID, Generation: request.Generation, ManifestDigest: request.Manifest.Digest()})
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".approval-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name()) //nolint:errcheck // remove an unpublished temporary approval
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close() //nolint:errcheck // Sync reports persistence failures
	return directory.Sync()
}

func grantApprovedClientOnlyUpdate(ctx context.Context, engine *updateinstall.Engine, status updateinstall.Status) (updateinstall.Status, error) {
	if !status.Settings.ClientOnly || status.Phase != updateinstall.Staged {
		return status, nil
	}
	if !update.ValidRunID(status.Request.ID) {
		return status, errors.New("invalid local update approval ID")
	}
	file, err := os.Open(clientOnlyApprovalPath(status.Settings.StateDir, status.Request.ID))
	if err != nil {
		return status, err
	}
	defer file.Close() //nolint:errcheck // read-only approval file
	var approval clientOnlyApproval
	if err := json.NewDecoder(io.LimitReader(file, 4096)).Decode(&approval); err != nil {
		return status, err
	}
	if approval.ID != status.Request.ID || approval.TargetID != status.Request.TargetID || approval.Generation != status.Request.Generation || approval.ManifestDigest != status.Request.Manifest.Digest() {
		return status, errors.New("local update approval does not match staged installation")
	}
	return engine.Grant(ctx, status.Request.ID, status.Request.Generation)
}
