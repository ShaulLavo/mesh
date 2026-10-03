package cli

import (
	"strings"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/update"
)

const updateLocalAlias = "local"

const updateRecoveryProblem = "An earlier update needs recovery."

const updateBridgeProblem = "An intermediate release with a verified upgrade path is required."

func updateAuthorizationUnavailable(problem string) bool {
	return strings.HasPrefix(problem, "update administrator is not enrolled on this host") || problem == "update administrator is not authorized" || strings.Contains(problem, "transport: device key is not approved")
}

func updateIdentityFailure(problem string) bool {
	return strings.Contains(problem, "destination Mesh identity changed") || strings.Contains(problem, "invalid Mesh identity certificate") || strings.Contains(problem, "update response identity mismatch") || problem == "invalid update signature"
}

func updateAccessUnverified(problem string) bool {
	if updateIdentityFailure(problem) {
		return false
	}
	return strings.Contains(problem, "transport: Mesh peer authentication failed")
}

func prepareUpdateApproval(preview updatePreview) updatePreview {
	for index := range preview.Targets {
		if problem := prepareUpdateTarget(&preview.Targets[index], preview.Release); problem != "" {
			preview.ApprovalProblem = problem
		}
	}
	return preview
}

func prepareUpdateTarget(target *update.Target, manifest release.Manifest) string {
	if updateAuthorizationUnavailable(target.Problem) || updateAccessUnverified(target.Problem) {
		return "Selected machines cannot be updated from here. Run mesh update --local --check to review only this machine."
	}
	if target.State == update.Failed {
		return "Update blocked. Resolve the reported problem on the selected machine before retrying."
	}
	if target.State != update.Pending || target.Build == nil {
		return ""
	}
	if updateBuildReady(target, manifest) {
		return ""
	}
	target.Problem = updateBridgeProblem
	return "Update blocked. Review an intermediate release with mesh update --local --check --version VERSION on that machine before approving it."
}

func updateBuildReady(target *update.Target, manifest release.Manifest) bool {
	build := *target.Build
	if build.UpdateProtocol != release.CurrentUpdateProtocol {
		return false
	}
	artifact, err := manifest.Artifact(build.Platform)
	if err != nil {
		return false
	}
	if build.Digest == artifact.BinarySHA256 {
		target.State = update.Updated
		return true
	}
	comparison, err := release.CompareVersions(build.Version, manifest.Version)
	if err == nil && comparison > 0 {
		if !newerUpdateBuildCompatible(build, manifest.Compatibility) {
			return false
		}
		target.State = update.Newer
		return true
	}
	return manifest.Allows(build) == nil
}

func newerUpdateBuildCompatible(build release.Build, compatibility release.Compatibility) bool {
	return !build.Modified && build.StateVersion >= compatibility.StateReadMin && build.StateVersion <= compatibility.StateReadMax && build.WorkerProtocol >= compatibility.WorkerMin && build.WorkerProtocol <= compatibility.WorkerMax
}

func updatePreviewCurrent(preview updatePreview) bool {
	if len(preview.Targets) == 0 {
		return false
	}
	for _, target := range preview.Targets {
		if target.State != update.Updated && target.State != update.Newer {
			return false
		}
	}
	return true
}

func updateTargetLabel(target update.Target) string {
	if updateAuthorizationUnavailable(target.Problem) {
		return "not managed here"
	}
	if updateAccessUnverified(target.Problem) {
		return "access unverified"
	}
	if target.State == update.Pending && target.Problem == updateBridgeProblem {
		return "needs intermediate release"
	}
	if target.State == update.Updated {
		return "up to date"
	}
	return string(target.State)
}

func updateTargetProblem(target update.Target) string {
	if updateAuthorizationUnavailable(target.Problem) {
		return "This machine has not authorized updates from here. Run mesh update --check on " + target.Host.Alias + " to review it locally."
	}
	if updateAccessUnverified(target.Problem) {
		return "Mesh could not verify access to this machine. Run mesh update --check on " + target.Host.Alias + " to review it locally."
	}
	if target.Problem == updateBridgeProblem || target.Problem == updateRecoveryProblem {
		location := target.Host.Alias
		if location == updateLocalAlias {
			location = "this machine"
		}
		return target.Problem + " Run mesh update status on " + location + " before retrying."
	}
	return target.Problem
}
