package update

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"

	"github.com/shaul/mesh/internal/updatebootstrap"
	"github.com/shaul/mesh/internal/updateinstall"
)

func inspectAdoptionService(ctx context.Context, expected updateinstall.Settings) (string, error) {
	installation, err := updatebootstrap.InspectInstallation(ctx, expected.StateDir)
	if err != nil {
		return "", hostError(err)
	}
	if installation.Settings != expected {
		return "", errors.New("update: actual daemon service settings differ from adoption")
	}
	return adoptionServiceSnapshotDigest(installation.Service)
}

func adoptionServiceSnapshotDigest(snapshot updatebootstrap.ServiceSnapshot) (string, error) {
	data, err := json.Marshal(snapshot)
	if err != nil {
		return "", hostError(err)
	}
	return discoveryDigest(data), nil
}

func adoptionInstallationFiles(installation updatebootstrap.Installation, extra []string) []string {
	state, config := installation.Settings.StateDir, installation.ConfigDir
	files := []string{filepath.Join(state, "authorized_keys"), filepath.Join(state, "access", "scopes.json"), filepath.Join(state, "updates", "administrators.json")}
	if config != "" {
		files = append(files, filepath.Join(config, "hosts.json"), filepath.Join(config, "fleet.json"), filepath.Join(config, "domains.json"))
	}
	return append(files, extra...)
}
