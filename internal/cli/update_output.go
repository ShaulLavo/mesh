package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/charmbracelet/x/term"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/privacy"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/updateinstall"
)

func (a *application) updateInteractive() bool {
	return a.dependencies.Stdin != nil && a.dependencies.Stdout != nil && term.IsTerminal(a.dependencies.Stdin.Fd()) && term.IsTerminal(a.dependencies.Stdout.Fd())
}

func printUpdatePreview(output io.Writer, preview updatePreview, structured, details bool, masks ...*privacy.Mask) error {
	mask := presentationMask(masks)
	// Structured results are operational records and keep their real identifiers.
	if structured {
		return json.NewEncoder(output).Encode(preview)
	}
	if !details {
		return printUpdateSummary(output, preview, mask)
	}
	if _, err := fmt.Fprintf(output, "Mesh %s · commit %s\nRelease digest: %s\nFleet %s revision %d · %d machines\n", preview.Release.Version, preview.Release.Commit, preview.ReleaseDigest, SafeTerminalText(mask.Value("fleet", preview.Fleet.Name)), preview.Fleet.Revision, len(preview.Fleet.Members)); err != nil {
		return err
	}
	if len(preview.Fleet.Members) == 1 && update.IsLocal(preview.Fleet.Members[0]) {
		_, _ = fmt.Fprintln(output, "This machine only.")
	}
	if len(preview.OutsideFleet) > 0 {
		_, _ = fmt.Fprintf(output, "Other machines are outside this update: %s. Use --fleet FILE to choose a different group.\n", privateUpdateHosts(mask, preview.OutsideFleet))
	}
	if preview.ClientOnly {
		_, _ = fmt.Fprintln(output, "This local CLI needs a supervised update helper. Approval includes installing that helper; it does not add a hosting daemon.")
	}
	if preview.CoordinatorBootstrap {
		if preview.CoordinatorSetup {
			_, _ = fmt.Fprintln(output, "This machine needs a local coordinator. Approval includes installing and starting a supervised Mesh daemon and update helper before continuing this exact fleet operation.")
		} else {
			_, _ = fmt.Fprintln(output, "The existing local daemon needs its first updater. Approval includes updating this coordinator first, preserving its current service configuration and sessions; then it resumes this exact fleet operation.")
		}
		if preview.CoordinatorAdded {
			_, _ = fmt.Fprintln(output, "Additional machine in this operation: the local coordinator, which was outside the requested fleet scope.")
		}
	}

	if err := printUpdatePreviewTargets(output, preview, mask); err != nil {
		return err
	}
	if preview.ApprovalProblem != "" {
		_, err := fmt.Fprintln(output, SafeTerminalText(mask.Text(preview.ApprovalProblem)))
		return err
	}
	return nil
}

func printUpdatePreviewTargets(output io.Writer, preview updatePreview, mask *privacy.Mask) error {
	if err := printUpdateTargets(output, updateReviewDisplayTargets(preview), preview.Release, mask); err != nil {
		return err
	}
	for _, target := range preview.Targets {
		review := preview.Reviews[target.Host.ID]
		var lines []string
		if review.Cause != "" {
			lines = append(lines, review.Cause)
		}
		if review.Bridge != nil {
			lines = append(lines, fmt.Sprintf("Verified next hop: Mesh %s · release digest %s", review.Bridge.Version, review.Bridge.Digest()))
		}
		if len(lines) == 0 {
			continue
		}
		if _, err := fmt.Fprintln(output, SafeTerminalText(mask.Text(strings.Join(lines, "\n")))); err != nil {
			return fmt.Errorf("print update bridge evidence: %w", err)
		}
	}
	return nil
}

func printUpdateTargets(output io.Writer, targets []update.Target, manifest release.Manifest, masks ...*privacy.Mask) error {
	mask := presentationMask(masks)
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	for _, target := range targets {
		build := updateTargetBuildText(target)
		if _, err := fmt.Fprintf(writer, "%s\t%s\t%s%s\n", SafeTerminalText(mask.Value("host", target.Host.Label())), SafeTerminalText(updateTargetLabel(target)), SafeTerminalText(mask.Text(build)), workerUpdateSummary(target.Workers, manifest)); err != nil {
			return err
		}
		if len(target.InterruptedWorkers) > 0 {
			if _, err := fmt.Fprintf(writer, "\t\t%d sessions interrupted by a machine reboot; review recovery with mesh ls\n", len(target.InterruptedWorkers)); err != nil {
				return err
			}
		}
		if target.Problem != "" {
			if _, err := fmt.Fprintf(writer, "  %s\n", SafeTerminalText(mask.Value("error", updateTargetProblem(target)))); err != nil {
				return err
			}
		}
	}
	return writer.Flush()
}

func workerUpdateSummary(workers []updateinstall.Worker, manifest release.Manifest) string {
	if len(workers) == 0 {
		return ""
	}
	older, unknown := 0, 0
	for _, worker := range workers {
		if worker.Build == nil {
			unknown++
			continue
		}
		comparison, err := release.CompareVersions(worker.Build.Version, manifest.Version)
		if err != nil {
			unknown++
			continue
		}
		if comparison < 0 {
			older++
		}
	}
	text := "; " + sessionCountText(len(workers))
	if older > 0 {
		text += fmt.Sprintf("; %d using an older Mesh version", older)
	}
	if unknown > 0 {
		text += fmt.Sprintf("; %d with unknown Mesh version", unknown)
	}
	return text
}

func printUpdateRunMode(output io.Writer, run update.Run, structured, details bool, masks ...*privacy.Mask) error {
	if !structured && !details {
		return printUpdateRunSummary(output, run, presentationMask(masks))
	}
	return printUpdateRun(output, run, structured, masks...)
}

func printUpdateRun(output io.Writer, run update.Run, structured bool, masks ...*privacy.Mask) error {
	mask := presentationMask(masks)
	if structured {
		return json.NewEncoder(output).Encode(run)
	}
	if _, err := fmt.Fprintf(output, "Update %s · %s\n", mask.Value("update", run.ID), run.Release.Version); err != nil {
		return err
	}
	if err := printUpdateTargets(output, run.Targets, run.Release, mask); err != nil {
		return err
	}
	if run.Problem != "" {
		_, _ = fmt.Fprintln(output, SafeTerminalText(mask.Value("error", run.Problem)))
	}
	updated := 0
	for _, target := range run.Targets {
		if target.State == update.Updated || target.State == update.Newer {
			updated++
		}
	}
	_, err := fmt.Fprintf(output, "%d of %d machines verified. Run mesh update status %s to check again.\n", updated, len(run.Targets), mask.Value("update", run.ID))
	return err
}

func observeUpdate(ctx context.Context, environment updateEnvironment, run update.Run, structured bool, output updateOutput) error {
	if !structured {
		printUpdateStarted(output, run)
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for !updateObservationSettled(run) {
		select {
		case <-ctx.Done():
			if err := printDeclaredUpdateRun(ctx, output.out, run, structured, output.details, output.privacy); err != nil {
				return err
			}
			return statusError{code: 2}
		case <-ticker.C:
		}
		var current update.Run
		poll, stop := context.WithTimeout(ctx, 5*time.Second)
		err := environment.client.Call(poll, environment.coordinator, "status", update.Operation{ID: run.ID}, &current)
		stop()
		if err == nil {
			run = current
		} else if local, readErr := readFirstCoordinatorRun(environment, run.ID); readErr == nil {
			run = local
		}
	}
	if err := printDeclaredUpdateRun(ctx, output.out, run, structured, output.details, output.privacy); err != nil {
		return err
	}
	return updateExit(run.ExitCode())
}

func updateObservationSettled(run update.Run) bool {
	if run.Done() {
		return true
	}
	if run.Stopped || run.Problem != "" {
		return true
	}
	for _, target := range run.Targets {
		if target.State == update.Pending || target.State == update.Staged || target.State == update.Granted {
			return false
		}
		// An authorized target drops off while it restarts onto the release;
		// that is progress, not a verdict worth returning on.
		if target.State == update.Offline && target.Grant {
			return false
		}
	}
	return true
}

func updateExit(code int) error {
	if code == 0 {
		return nil
	}
	return statusError{code: code}
}

func privateUpdateHosts(mask *privacy.Mask, hosts []string) string {
	labels := make([]string, len(hosts))
	for i, host := range hosts {
		labels[i] = mask.Value("host", host)
	}
	return SafeTerminalText(strings.Join(labels, ", "))
}

func printDeclaredUpdateRun(ctx context.Context, output io.Writer, run update.Run, structured, details bool, masks ...*privacy.Mask) error {
	if !structured {
		targets, err := declaredUpdateTargets(ctx, run.Targets)
		if err != nil {
			return err
		}
		run.Targets = targets
	}
	return printUpdateRunMode(output, run, structured, details, masks...)
}

func declaredUpdateTargets(ctx context.Context, targets []update.Target) ([]update.Target, error) {
	hosts, err := LoadHosts()
	if err != nil {
		return nil, err
	}
	stateDir, err := paths.StateDir()
	if err != nil {
		return nil, fmt.Errorf("locate updater naming state: %w", err)
	}
	local, err := localNameRecord(ctx, stateDir)
	if err != nil {
		return nil, err
	}
	known := withOwnerClaim(hosts, local)
	ProjectHostNames(known)
	labels := make(map[string]string, len(known))
	for _, host := range known {
		if host.MachineName == "" {
			continue
		}
		label := HostLabel(host)
		if !host.NameVerified {
			label += retainedNameSuffix
		}
		labels[host.ID] = label
	}
	displayed := make([]update.Target, len(targets))
	copy(displayed, targets)
	for index := range displayed {
		displayed[index].Host.MachineName = labels[displayed[index].Host.ID]
	}
	return displayed, nil
}

func updateTargetBuildText(target update.Target) string {
	if target.Build == nil || target.Build.Version == "" {
		return "version unknown"
	}
	return "daemon " + target.Build.Version
}

func updateReviewDisplayTargets(preview updatePreview) []update.Target {
	targets := append([]update.Target(nil), preview.Targets...)
	for i := range targets {
		review := preview.Reviews[targets[i].Host.ID]
		if review.Kind == "authorization" {
			targets[i].Problem = review.Message
		}
	}
	return targets
}

func printUpdateStarted(output updateOutput, run update.Run) {
	sessions := 0
	for _, target := range run.Targets {
		if target.State == update.Updated || target.State == update.Newer || target.State == update.Failed {
			continue
		}
		for _, worker := range target.Workers {
			if worker.Protocol > 0 {
				sessions++
			}
		}
	}
	if sessions > 0 {
		verb := "keep"
		if sessions == 1 {
			verb = "keeps"
		}
		_, _ = fmt.Fprintf(output.diagnostic, "Your %s %s working during the update.\n", sessionCountText(sessions), verb)
	}
	_, _ = fmt.Fprintf(output.diagnostic, "Update %s saved. Closing this terminal does not cancel it.\n", output.privacy.Value("update", run.ID))
}
