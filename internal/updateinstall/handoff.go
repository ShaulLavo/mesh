package updateinstall

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/shaul/mesh/internal/release"
)

// CommittedExecutable names only the installed path from a matching health receipt.
func CommittedExecutable(status Status, hostID string, build release.Build) (string, error) {
	if status.Phase != Committed || status.Verified == nil || status.Verified.HostID != hostID || status.Verified.Build.Digest != build.Digest || build.Digest == "" {
		return "", nil
	}
	path := status.Settings.Executable
	if !filepath.IsAbs(path) || path == status.Candidate || path == status.Previous {
		return "", fmt.Errorf("installed mesh path is unavailable")
	}
	return path, nil
}

// WithCommittedExecutable holds activation's lock until the handoff returns or exec closes it.
func WithCommittedExecutable(ctx context.Context, stateDir, hostID string, build release.Build, handoff func(string) error) error {
	lock, err := lockInstallation(ctx, stateDir)
	if err != nil {
		return fmt.Errorf("lock installed mesh: %w", err)
	}
	defer unlock(lock)
	status, err := Read(stateDir)
	if err != nil {
		return fmt.Errorf("read installed mesh receipt: %w", err)
	}
	path, err := CommittedExecutable(status, hostID, build)
	if err != nil {
		return err
	}
	if path == "" {
		return fmt.Errorf("installed mesh health changed before restart")
	}
	if err := release.VerifyExecutable(ctx, path, build.Digest); err != nil {
		return fmt.Errorf("verify installed mesh: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("handoff installed mesh: %w", err)
	}
	return handoff(path)
}
