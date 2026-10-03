package updateinstall

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/shaul/mesh/internal/release"
)

// ErrHandoffTimeout identifies a pre-exec deadline; callback failures are terminal.
var ErrHandoffTimeout = errors.New("installed mesh handoff wait timed out")

type installedImageMismatch struct{ cause error }

func (e installedImageMismatch) Error() string { return "executable differs from the healthy daemon" }
func (e installedImageMismatch) Unwrap() error { return e.cause }

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
		return handoffFailure(ctx, "lock installed mesh", err)
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
		if errors.Is(err, release.ErrExecutableChecksumMismatch) {
			err = installedImageMismatch{cause: err}
		}
		return handoffFailure(ctx, "verify installed mesh", err)
	}
	if err := ctx.Err(); err != nil {
		return handoffFailure(ctx, "handoff installed mesh", err)
	}
	return handoff(path)
}

func handoffFailure(ctx context.Context, operation string, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = errors.Join(ErrHandoffTimeout, err, ctx.Err())
	}
	return fmt.Errorf("%s: %w", operation, err)
}
