package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/updateinstall"
)

type dashboardRestart struct {
	current   release.Build
	argv, env []string
	installed func(release.Build) (string, error)
	exec      func(string, []string, []string) error
}

type dashboardRestartTarget struct {
	path string
	err  error
}

func (r dashboardRestart) run(ctx context.Context, input DashboardInput, dashboard DashboardFunc) error {
	run, cancel := context.WithCancel(ctx)
	defer cancel()
	next := input
	var target *dashboardRestartTarget
	next.Watch, target = r.watchForRestart(input.Watch, cancel)
	err := dashboard(run, next)
	cancel()
	if ctx.Err() != nil || (target.path == "" && target.err == nil) {
		return err
	}
	// The terminal runner restores modes and joins its readers before returning.
	if target.err == nil {
		target.err = r.exec(target.path, r.argv, r.env)
	}
	if target.err == nil {
		return nil
	}
	input.Notice = "Dashboard restart failed: " + dashboardText(target.err.Error())
	return dashboard(ctx, input)
}

func (r dashboardRestart) watchForRestart(watch DashboardWatch, cancel context.CancelFunc) (DashboardWatch, *dashboardRestartTarget) {
	target := new(dashboardRestartTarget)
	return func(ctx context.Context, publish func(DashboardHostView)) error {
		return watch(ctx, func(view DashboardHostView) {
			if ctx.Err() != nil {
				return
			}
			if dashboardChangedBuild(view, r.current) {
				target.path, target.err = r.installed(view.Build)
				if target.path != "" || target.err != nil {
					cancel()
					return
				}
			}
			publish(view)
		})
	}, target
}

func dashboardChangedBuild(view DashboardHostView, current release.Build) bool {
	return view.Host.Local && view.Connection == StateReachable && !view.LastReply.IsZero() && current.Digest != "" && view.Build.Digest != "" && view.Build.Digest != current.Digest
}

func dashboardInstalledTarget(stateDir, localID string, build release.Build) (string, error) {
	status, err := updateinstall.Read(stateDir)
	if err != nil {
		return "", nil
	}
	// A restarted daemon can answer before worker validation finishes or rollback starts.
	if status.Phase != updateinstall.Committed || status.Verified == nil || status.Verified.HostID != localID || status.Verified.Build.Digest != build.Digest {
		return "", nil
	}
	path := status.Settings.Executable
	if !filepath.IsAbs(path) || path == status.Candidate || path == status.Previous {
		return "", fmt.Errorf("installed mesh path is unavailable")
	}
	file, err := os.Open(path) //nolint:gosec // executable comes from the local installation journal
	if err != nil {
		return "", fmt.Errorf("open installed mesh: %w", err)
	}
	defer file.Close() //nolint:errcheck // read-only
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("read installed mesh: %w", err)
	}
	if hex.EncodeToString(hash.Sum(nil)) != build.Digest {
		return "", fmt.Errorf("installed mesh differs from the healthy daemon")
	}
	return path, nil
}
