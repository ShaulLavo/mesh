package cli

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/updateinstall"
)

const updateReviewRecovery = "recovery"
const updateReviewJournal = "journal"
const updateMacOS = "darwin"
const updateRecoveryProblem = "An earlier update needs recovery."
const updateBridgeProblem = "This release does not support updating this installation directly."

type updateTargetReview struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
	Cause   string `json:"cause,omitempty"`
}

func prepareUpdateApproval(preview updatePreview) updatePreview {
	if preview.Reviews == nil {
		preview.Reviews = make(map[string]updateTargetReview)
	}
	for index := range preview.Targets {
		target := &preview.Targets[index]
		review, blocked := prepareUpdateTarget(target, preview.Release, preview.Reviews[target.Host.ID])
		if review.Kind != "" {
			preview.Reviews[target.Host.ID] = review
		}
		if blocked && preview.ApprovalProblem == "" {
			preview.ApprovalProblem = updateTargetReviewAction(*target, review)
		}
	}
	return preview
}

func prepareUpdateTarget(target *update.Target, manifest release.Manifest, review updateTargetReview) (updateTargetReview, bool) {
	if review.Kind == updateReviewRecovery || review.Kind == updateReviewJournal {
		return review, true
	}
	if target.State == update.Failed {
		return review, true
	}
	if target.State == update.Offline {
		return review, false
	}
	if target.State != update.Pending && target.State != update.Bootstrap {
		return review, false
	}
	if target.Build == nil {
		return updateTargetReview{Kind: "build", Message: "Mesh could not check this installation's update information.", Cause: "source build information is unavailable"}, true
	}
	next, err := updateBuildReady(target, manifest)
	if err == nil {
		return updateTargetReview{}, false
	}
	if target.Problem == "" {
		target.Problem = err.Error()
	}
	next.Cause = err.Error()
	return next, true
}

func updateBuildReady(target *update.Target, manifest release.Manifest) (updateTargetReview, error) {
	build := *target.Build
	if target.State != update.Bootstrap && build.UpdateProtocol != release.CurrentUpdateProtocol {
		return updateTargetReview{Kind: "protocol", Message: "This installation needs an updater that supports this release."}, errors.New("unsupported update protocol")
	}
	artifact, err := manifest.Artifact(build.Platform)
	if err != nil {
		return updateTargetReview{Kind: "platform", Message: "This release does not support this installation's platform."}, fmt.Errorf("check release platform: %w", err)
	}
	if build.Digest == artifact.BinarySHA256 {
		target.State = update.Updated
		return updateTargetReview{}, nil
	}
	comparison, versionErr := release.CompareVersions(build.Version, manifest.Version)
	if versionErr == nil && comparison > 0 && newerUpdateBuildCompatible(build, manifest.Compatibility) {
		target.State = update.Newer
		return updateTargetReview{}, nil
	}
	if manifest.Compatibility.JournalVersion != release.CurrentJournalVersion {
		return updateTargetReview{Kind: "format", Message: "This release needs a newer installation format."}, errors.New("release requires an unsupported installation journal format")
	}
	return updateInstallationEligibility(target, manifest, artifact, comparison, versionErr == nil)
}

func updateInstallationEligibility(target *update.Target, manifest release.Manifest, artifact release.Artifact, comparison int, knownVersion bool) (updateTargetReview, error) {
	build := *target.Build
	if review, err := updateSourceCompatibility(build, artifact, manifest); err != nil {
		return review, err
	}

	if knownVersion && comparison >= 0 {
		message := "This installation has a different build of the same version."
		if comparison > 0 {
			message = "This release is older than the installed version."
		}
		return updateTargetReview{Kind: "version-order", Message: message}, errors.New("installation updates require a strictly newer release")
	}
	for _, worker := range target.Workers {
		if worker.Protocol < manifest.Compatibility.WorkerMin || worker.Protocol > manifest.Compatibility.WorkerMax {
			return updateTargetReview{Kind: "worker", Message: "A running session needs a supported session version before this update."}, fmt.Errorf("session %s has an incompatible or unknown worker protocol", worker.ID)
		}
	}
	return updateTargetReview{}, nil
}

func updateSourceCompatibility(build release.Build, artifact release.Artifact, manifest release.Manifest) (updateTargetReview, error) {
	if err := manifest.Allows(build); err != nil {
		kind := "build"
		if updateTransitionMissing(build, artifact, manifest.Compatibility) {
			kind = "transition"
		}
		message := "Mesh could not verify that this installation supports the release."
		if kind == "transition" {
			message = updateBridgeProblem
		}
		return updateTargetReview{Kind: kind, Message: message}, fmt.Errorf("check source compatibility: %w", err)
	}
	return updateTargetReview{}, nil
}

func updateTransitionMissing(build release.Build, artifact release.Artifact, compatibility release.Compatibility) bool {
	if len(build.Digest) != 64 || build.StateVersion < compatibility.StateReadMin || build.StateVersion > compatibility.StateReadMax || build.WorkerProtocol < compatibility.WorkerMin || build.WorkerProtocol > compatibility.WorkerMax {
		return false
	}
	for _, transition := range compatibility.Transitions {
		if transition.FromDigest == build.Digest && transition.ToDigest == artifact.BinarySHA256 && transition.Platform == build.Platform {
			return false
		}
	}
	// Allows validates digest syntax before reporting a missing transition.
	return validUpdateDigest(build.Digest)
}

func validUpdateDigest(digest string) bool {
	if len(digest) != 64 || digest != strings.ToLower(digest) {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

func newerUpdateBuildCompatible(build release.Build, compatibility release.Compatibility) bool {
	return !build.Modified && build.StateVersion >= compatibility.StateReadMin && build.StateVersion <= compatibility.StateReadMax && build.WorkerProtocol >= compatibility.WorkerMin && build.WorkerProtocol <= compatibility.WorkerMax
}

func updateTargetLocation(target update.Target) string {
	if update.IsLocal(target.Host) {
		if target.Build != nil && target.Build.Platform.OS == updateMacOS {
			return "this Mac"
		}
		return "this machine"
	}
	return target.Host.Label()
}

func updateTargetReviewAction(target update.Target, review updateTargetReview) string {
	if review.Kind == updateReviewRecovery || review.Kind == updateReviewJournal {
		if update.IsLocal(target.Host) {
			return "Run mesh update status to review recovery on " + updateTargetLocation(target) + "."
		}
		return "Run mesh update status on " + target.Host.Label() + " to review recovery."
	}
	if update.IsLocal(target.Host) {
		return "Run mesh update --local --check to review " + updateTargetLocation(target) + " before updating."
	}
	return "Run mesh update --local --check on " + target.Host.Label() + " before updating."
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

func reviewLocalUpdateJournal(preview updatePreview, stateDir string, local update.Host) updatePreview {
	selected := false
	for _, target := range preview.Targets {
		if target.Host.ID == local.ID {
			selected = true
		}
	}
	if !selected {
		return preview
	}
	status, err := updateinstall.Read(stateDir)
	if errors.Is(err, os.ErrNotExist) {
		return preview
	}
	review := updateTargetReview{Kind: updateReviewRecovery, Message: updateRecoveryProblem}
	if err != nil {
		review = updateTargetReview{Kind: updateReviewJournal, Message: "Mesh could not read this installation's update history.", Cause: err.Error()}
	}
	if err == nil {
		switch status.Phase {
		case updateinstall.Committed, updateinstall.RolledBack, updateinstall.Cancelled, updateinstall.Failed:
			return preview
		case updateinstall.Accepted, updateinstall.Staged, updateinstall.Granted, updateinstall.Activating, updateinstall.Validating, updateinstall.RollingBack, updateinstall.RollbackFailed:
			review.Cause = string(status.Phase) + ": " + status.Error
		default:
			review.Cause = "unknown installation phase: " + string(status.Phase)
		}
	}
	if preview.Reviews == nil {
		preview.Reviews = make(map[string]updateTargetReview)
	}
	preview.Reviews[local.ID] = review
	preview.ApprovalProblem = updateTargetReviewAction(update.Target{Host: local}, review)
	return preview
}

func updateTargetLabel(target update.Target) string {
	if target.State == update.Updated {
		return "up to date"
	}
	return string(target.State)
}

func updateTargetProblem(target update.Target) string { return target.Problem }
