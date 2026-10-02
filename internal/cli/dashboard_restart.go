package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/updateinstall"
)

type dashboardRestart struct {
	current   release.Build
	argv, env []string
	installed func(release.Build) (string, error)
	exec      func(context.Context, release.Build, string, []string, []string) error
}

type dashboardRestartTarget struct {
	path  string
	build release.Build
	err   error
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
		target.err = r.exec(ctx, target.build, target.path, r.argv, r.env)
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
				target.build = view.Build
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
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read dashboard installation: %w", err)
	}
	path, err := updateinstall.CommittedExecutable(status, localID, build)
	if err != nil {
		return "", fmt.Errorf("dashboard installed target: %w", err)
	}
	return path, nil
}
