package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/shaul/mesh/internal/privacy"
	"github.com/shaul/mesh/internal/update"
)

func printUpdateSummary(output io.Writer, preview updatePreview, mask *privacy.Mask) error {
	heading := "Mesh %s is available."
	if updatePreviewCurrent(preview) {
		heading = "Selected release: Mesh %s."
	}
	lines := []string{fmt.Sprintf(heading, SafeTerminalText(preview.Release.Version))}
	for _, target := range preview.Targets {
		subject := mask.Value("host", updateTargetLocation(target))
		if update.IsLocal(target.Host) {
			subject = strings.Replace(subject, "this ", "This ", 1)
		}
		lines = append(lines, updateSubjectLines(subject, target, preview.Reviews[target.Host.ID])...)
	}
	switch {
	case preview.ApprovalProblem != "":
		lines = append(lines, SafeTerminalText(mask.Text(preview.ApprovalProblem)))
	case preview.ClientOnly:
		lines = append(lines, "Updating this installation also installs its update helper.")
	case preview.CoordinatorSetup:
		lines = append(lines, "This update also installs and starts Mesh on this machine to manage the selected machines.")
	}
	return writeUpdateLines(output, lines)
}

func updateSubjectLines(subject string, target update.Target, review updateTargetReview) []string {
	description := SafeTerminalText(subject) + ": Mesh version unknown."
	if target.Build != nil && target.Build.Version != "" {
		verb := "runs"
		if target.State == update.Updated {
			verb = "already runs"
		}
		description = fmt.Sprintf("%s %s Mesh %s.", SafeTerminalText(subject), verb, SafeTerminalText(target.Build.Version))
	}
	lines := []string{description}
	message := review.Message
	if message == "" && target.State == update.Failed {
		message = "The update check failed."
	}
	if message == "" && target.State == update.Offline {
		message = "Mesh could not reach this machine. It stays in the selected update."
	}
	if target.State == update.Newer {
		message = "This installation is newer than the selected release."
	}
	if message != "" {
		lines = append(lines, message)
	}
	return lines
}

func sessionCountText(count int) string {
	if count == 1 {
		return "1 running session"
	}
	return fmt.Sprintf("%d running sessions", count)
}

func printUpdateRunSummary(output io.Writer, run update.Run, mask *privacy.Mask) error {
	lines := []string{fmt.Sprintf("Mesh %s update.", SafeTerminalText(run.Release.Version))}
	updated := 0
	for _, target := range run.Targets {
		if target.State == update.Updated || target.State == update.Newer {
			updated++
		}
		lines = append(lines, fmt.Sprintf("%s: %s", SafeTerminalText(mask.Value("host", updateTargetLocation(target))), updateRunTargetLabel(target.State)))
	}
	if run.Problem != "" {
		lines = append(lines, SafeTerminalText(mask.Value("error", run.Problem)))
	}
	lines = append(lines, fmt.Sprintf("%d of %d machines verified. Run mesh update status %s to check again.", updated, len(run.Targets), mask.Value("update", run.ID)))
	return writeUpdateLines(output, lines)
}

func updateRunTargetLabel(state update.State) string {
	switch state {
	case update.Updated:
		return "Mesh is up to date."
	case update.Newer:
		return "Mesh is newer than the selected release."
	case update.Failed:
		return "The update failed."
	case update.Cancelled:
		return "The update was cancelled."
	case update.Offline:
		return "Mesh is currently unreachable."
	case update.Pending, update.Staged, update.Granted, update.Bootstrap:
		return "The update is in progress."
	}
	return "Waiting for this machine."
}

func writeUpdateLines(output io.Writer, lines []string) error {
	if _, err := fmt.Fprintln(output, strings.Join(lines, "\n")); err != nil {
		return fmt.Errorf("print update summary: %w", err)
	}
	return nil
}
