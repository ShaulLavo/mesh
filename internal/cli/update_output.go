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

func printUpdatePreview(output io.Writer, preview updatePreview, structured bool, masks ...*privacy.Mask) error {
	mask := presentationMask(masks)
	// Structured results are operational records and keep their real identifiers.
	if structured {
		return json.NewEncoder(output).Encode(preview)
	}
	if _, err := fmt.Fprintf(output, "Mesh %s · commit %s\nRelease digest: %s\nFleet %s revision %d · %d machines\n", preview.Release.Version, preview.Release.Commit, preview.ReleaseDigest, SafeTerminalText(mask.Value("fleet", preview.Fleet.Name)), preview.Fleet.Revision, len(preview.Fleet.Members)); err != nil {
		return err
	}
	if preview.FirstFleet {
		_, _ = fmt.Fprintln(output, "First fleet preview: only this machine and locally adopted hosts are known. Include every intended machine, including offline hosts, using --fleet FILE if this list is incomplete.")
	}
	if len(preview.OutsideFleet) > 0 {
		_, _ = fmt.Fprintf(output, "Adopted hosts outside this fleet: %s. Revise fleet.json or pass --fleet FILE to include them.\n", privateUpdateHosts(mask, preview.OutsideFleet))
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
	if err := printUpdateTargets(output, preview.Targets, preview.Release, mask); err != nil {
		return err
	}
	_, err := fmt.Fprintln(output, "Running sessions stay alive. Existing workers retain their installed code until their sessions end.")
	return err
}

func printUpdateTargets(output io.Writer, targets []update.Target, manifest release.Manifest, masks ...*privacy.Mask) error {
	mask := presentationMask(masks)
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	for _, target := range targets {
		build := "version unknown"
		if target.Build != nil {
			build = "daemon " + target.Build.Version
		}
		if _, err := fmt.Fprintf(writer, "%s\t%s\t%s%s\n", SafeTerminalText(mask.Value("host", target.Host.Label())), SafeTerminalText(string(target.State)), SafeTerminalText(mask.Text(build)), workerUpdateSummary(target.Workers, manifest)); err != nil {
			return err
		}
		if len(target.InterruptedWorkers) > 0 {
			if _, err := fmt.Fprintf(writer, "\t\t%d sessions interrupted by a machine reboot; review recovery with mesh ls\n", len(target.InterruptedWorkers)); err != nil {
				return err
			}
		}
		if target.Problem != "" {
			if _, err := fmt.Fprintf(writer, "\t\t%s\n", SafeTerminalText(mask.Value("error", target.Problem))); err != nil {
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
	text := fmt.Sprintf("; %d running sessions preserved", len(workers))
	if older > 0 {
		text += fmt.Sprintf("; %d use older workers", older)
	}
	if unknown > 0 {
		text += fmt.Sprintf("; %d worker versions unknown", unknown)
	}
	return text
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
		_, _ = fmt.Fprintf(output.diagnostic, "Update %s saved. Closing this terminal does not cancel it.\n", output.privacy.Value("update", run.ID))
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for !updateObservationSettled(run) {
		select {
		case <-ctx.Done():
			if err := printDeclaredUpdateRun(ctx, output.out, run, structured, output.privacy); err != nil {
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
	if err := printDeclaredUpdateRun(ctx, output.out, run, structured, output.privacy); err != nil {
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

func printDeclaredUpdateRun(ctx context.Context, output io.Writer, run update.Run, structured bool, masks ...*privacy.Mask) error {
	if !structured {
		targets, err := declaredUpdateTargets(ctx, run.Targets)
		if err != nil {
			return err
		}
		run.Targets = targets
	}
	return printUpdateRun(output, run, structured, masks...)
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
