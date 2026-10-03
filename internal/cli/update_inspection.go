package cli

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"syscall"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/updatebootstrap"
)

func classifyUpdateInspection(err error) updateTargetReview {
	if errors.Is(err, transport.ErrAuthenticationRequired) {
		return updateTargetReview{Kind: "authentication-upgrade", Message: "This machine needs a Mesh authentication update before it can be reviewed from here."}
	}
	if errors.Is(err, transport.ErrAuthentication) {
		return updateTargetReview{Kind: "authentication", Message: "Mesh authentication failed on this machine."}
	}
	var remote *update.RemoteError
	if errors.As(err, &remote) {
		if remote.Problem == "update administrator is not authorized" || strings.HasPrefix(remote.Problem, "update administrator is not enrolled on this host;") {
			return updateTargetReview{Kind: "authorization", Message: "This machine has not authorized updates from here."}
		}
		return updateTargetReview{Kind: "failure", Message: "Mesh could not check this machine's update information."}
	}
	if updateEndpointUnavailable(err) {
		return updateTargetReview{Kind: "offline", Message: "Mesh could not reach this machine."}
	}
	return updateTargetReview{Kind: "failure", Message: "Mesh could not check this machine's update information."}
}

func updateEndpointUnavailable(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH) {
		return true
	}
	var failure *net.OpError
	return errors.As(err, &failure) && failure.Op == "dial"
}

func (a *application) currentUpdateBuild() release.Build {
	if a.dependencies.UpdateBuild != nil {
		return a.dependencies.UpdateBuild()
	}
	return release.Current()
}

func (a *application) reviewLocalUpdateBuild(environment updateEnvironment, preview updatePreview) updatePreview {
	for index := range preview.Targets {
		target := &preview.Targets[index]
		if target.Host.ID != environment.local.ID {
			continue
		}
		if preview.ClientOnly {
			build := a.currentUpdateBuild()
			target.Build, target.State = &build, update.Pending
			// The absent daemon is expected for a client-only installation.
			target.Problem = ""
			delete(preview.Reviews, target.Host.ID)
			continue
		}
	}
	return preview
}

func (a *application) reviewLocalSource(ctx context.Context, stateDir string, target update.Target) update.Target {
	if target.State != update.Bootstrap {
		return target
	}
	inspect := a.dependencies.UpdateInspect
	if inspect == nil {
		inspect = updatebootstrap.Inspect
	}
	observation, err := inspect(ctx, stateDir)
	if err != nil {
		target.State = update.Failed
		target.Problem += "; inspect legacy source: " + err.Error()
		return target
	}
	if observation.Health.HostID != target.Host.ID {
		target.State = update.Failed
		target.Problem += "; legacy source identity differs from pinned identity"
		return target
	}
	target.Build, target.Workers = &observation.Health.Build, observation.Health.Workers
	return target
}
