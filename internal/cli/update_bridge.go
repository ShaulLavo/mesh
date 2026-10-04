package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/updatebootstrap"
	"github.com/shaul/mesh/internal/updateinstall"
)

func (a *application) reviewUpdateBridges(ctx context.Context, environment updateEnvironment, preview updatePreview) updatePreview {
	if preview.ApprovalProblem == "" {
		return preview
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	preview.ApprovalProblem = ""
	for _, target := range preview.Targets {
		review := preview.Reviews[target.Host.ID]
		if review.Kind == updateReviewTransition {
			review = a.reviewUpdateBridge(ctx, environment, preview, target, review)
			preview.Reviews[target.Host.ID] = review
		}
		blocked := review.Kind == updateReviewRecovery || review.Kind == updateReviewJournal || target.State == update.Failed || (target.State == update.Pending || target.State == update.Bootstrap) && review.Kind != ""
		if preview.ApprovalProblem == "" && blocked {
			preview.ApprovalProblem = updateTargetReviewAction(target, review)
		}
	}
	return preview
}

func (a *application) reviewUpdateBridge(ctx context.Context, environment updateEnvironment, preview updatePreview, target update.Target, review updateTargetReview) updateTargetReview {
	review.Message = updateBridgeProblem + " Mesh could not establish a tested runnable update path."
	if !update.IsLocal(target.Host) || target.Host.ID != environment.local.ID || preview.ClientOnly || preview.CoordinatorBootstrap {
		review.Cause += "; bridge readiness requires a local review of the existing daemon and helper"
		return review
	}
	hop, err := a.dependencies.UpdateRelease.Bridge(ctx, *target.Build, preview.Release)
	if err != nil {
		review.Cause += "; " + err.Error()
		return review
	}
	candidate := target
	if _, err := updateBuildReady(&candidate, hop); err != nil {
		review.Cause += "; bridge readiness: " + err.Error()
		return review
	}
	if err := bridgeRetainedWorkers(target, preview.Release); err != nil {
		review.Cause += "; " + err.Error()
		return review
	}
	preflight := a.dependencies.UpdateBridgePreflight
	if preflight == nil {
		preflight = a.preflightLocalBridge
	}
	if err := preflight(ctx, environment.stateDir, target, hop); err != nil {
		review.Cause += "; bridge readiness: " + err.Error()
		return review
	}
	review.Bridge = &hop
	review.Message = updateBridgeProblem
	return review
}

func bridgeRetainedWorkers(target update.Target, manifest release.Manifest) error {
	for _, worker := range target.Workers {
		if worker.Protocol < manifest.Compatibility.WorkerMin || worker.Protocol > manifest.Compatibility.WorkerMax {
			return fmt.Errorf("bridge session %s cannot reach the selected release", worker.ID)
		}
	}
	return nil
}

func (a *application) preflightLocalBridge(ctx context.Context, stateDir string, target update.Target, hop release.Manifest) error {
	inspect := a.dependencies.UpdateInspect
	if inspect == nil {
		inspect = updatebootstrap.Inspect
	}
	observed, err := inspect(ctx, stateDir)
	if err != nil {
		return fmt.Errorf("inspect bridge installation: %w", err)
	}
	if observed.Health.HostID != target.Host.ID || observed.Health.Build != *target.Build {
		return errors.New("bridge source differs from the reviewed installation")
	}
	if err := checkHelperRecoveryApprovals(ctx, updateinstall.Status{Request: updateinstall.Request{TargetID: target.Host.ID}, Settings: updateinstall.Settings{StateDir: stateDir}}); err != nil {
		return err
	}
	settings, err := updateinstall.ReadSettingsContext(ctx, stateDir)
	if err != nil {
		return fmt.Errorf("read bridge helper ownership: %w", err)
	}
	service, err := updateinstall.DefaultServiceSpec()
	if err != nil {
		return fmt.Errorf("locate bridge daemon service: %w", err)
	}
	if settings.Service != service {
		return errors.New("bridge journal names a different daemon service")
	}
	probe := updatebootstrap.HelperProcessProbe(updateinstall.HelperConfig{Kind: settings.Service.Kind, Domain: settings.Service.Domain})
	if err := updateinstall.ReviewBridge(ctx, stateDir, observed.Executable, observed.Health, hop, probe); err != nil {
		return fmt.Errorf("check bridge installation readiness: %w", err)
	}
	return nil
}
