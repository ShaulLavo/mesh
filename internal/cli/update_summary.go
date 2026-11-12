package cli

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

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
		lines = append(lines, installationCommandLines(target.Commands, mask)...)
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
		lines = append(lines, installationCommandLines(target.Commands, mask)...)
	}
	action := "to check again."
	if run.Problem != "" {
		lines = append(lines, "Mesh could not finish this update.")
		action = "--details to review this saved update."
	}
	lines = append(lines, fmt.Sprintf("%d of %d machines verified. Run mesh update status %s %s", updated, len(run.Targets), mask.Value("update", run.ID), action))
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

const updateStatusSaved = "saved"

// DashboardUpdateRows projects status without discovery or consumer I/O.
func DashboardUpdateRows(view DashboardUpdates, now time.Time) []DashboardUpdateRow {
	var rows []DashboardUpdateRow
	if view.Problem != "" {
		rows = append(rows, DashboardUpdateRow{Text: view.Problem, Attention: true})
	}
	for _, source := range view.Sources {
		rows = append(rows, dashboardConfigurationRows(source, now)...)
	}
	return rows
}
func statusUnavailableRow(host, subject string, observation DashboardStatusObservation, now time.Time) []DashboardUpdateRow {
	if observation.State == DashboardStatusObserved && !observation.At.IsZero() && now.Sub(observation.At) < 2*dashboardUpdateInterval {
		return nil
	}
	state := string(observation.State)
	if observation.State == DashboardStatusObserved {
		state = "stale"
	}
	if state == "" {
		state = string(DashboardStatusUnavailable)
	}
	return []DashboardUpdateRow{{HostID: host, Text: subject + " status " + state + "; inspect mesh update status --details.", Attention: true}}
}
func dashboardConfigurationRows(source DashboardUpdateSource, now time.Time) []DashboardUpdateRow {
	observed := source.Configuration
	if observed == nil {
		return nil
	}
	if rows := statusUnavailableRow(source.Coordinator.ID, "Configuration", observed.Observation, now); len(rows) > 0 {
		return rows
	}
	if observed.Value == nil {
		return nil
	}
	change := observed.Value.Change
	if change == nil {
		return []DashboardUpdateRow{{HostID: source.Coordinator.ID, Text: "No current configuration delivery."}}
	}
	var rows []DashboardUpdateRow
	if change.Blocked != "" {
		rows = append(rows, DashboardUpdateRow{HostID: source.Coordinator.ID, Text: "Configuration delivery blocked; inspect mesh update config status --details.", Attention: true})
	}
	if change.Cancelled {
		rows = append(rows, DashboardUpdateRow{HostID: source.Coordinator.ID, Text: "Configuration cancelled; earlier issued work may still save settings."})
	}
	for index, target := range change.Targets {
		rows = append(rows, DashboardUpdateRow{HostID: target.HostID, Text: "Configuration publication " + SafeTerminalText(target.State) + "; consumer state is separate.", Attention: target.State != updateStatusSaved})
		if target.Issued {
			rows = append(rows, DashboardUpdateRow{HostID: target.HostID, Text: "Configuration dispatch issued; settings may still save after cancellation.", Attention: true})
		}
		if target.Problem != "" {
			rows = append(rows, DashboardUpdateRow{HostID: target.HostID, Text: "Configuration delivery has a problem; inspect mesh update config status --details.", Attention: true})
		}
		if index >= len(change.Proposal.Recipients) {
			continue
		}
		recipient := change.Proposal.Recipients[index]
		rows = append(rows, domainStatusRows(target, recipient, now)...)
		rows = append(rows, feedStatusRows(target, recipient, now)...)
	}
	return rows
}
func domainStatusRows(target update.ConfigurationTarget, recipient update.RecipientConfiguration, now time.Time) []DashboardUpdateRow {
	if recipient.Domain == nil {
		return nil
	}
	row := DashboardUpdateRow{HostID: target.HostID, Attention: true, Text: "Domain publication pending; loaded policy unknown."}
	if target.Receipt == nil || target.Receipt.Domain == nil || target.Receipt.Domain.State != updateStatusSaved {
		return []DashboardUpdateRow{row}
	}
	domain := target.Receipt.Domain
	wanted := domain.Desired.Digest()
	row.Text = "Domain saved; daemon policy observation unavailable."

	if target.Daemon != nil && target.Daemon.InstanceID != "" && !target.Daemon.ObservedAt.IsZero() && now.Sub(target.Daemon.ObservedAt) <= 45*time.Second {
		row.Text = "Domain saved; daemon restart required through its native service manager."
		if target.Daemon.Digest == wanted {
			row.Text, row.Attention = "Domain saved and loaded by the observed daemon.", false
		}
	}
	rows := []DashboardUpdateRow{row}
	if target.ObservedDomain != nil && target.ObservedDomain.Digest != wanted {
		rows = append(rows, DashboardUpdateRow{HostID: target.HostID, Text: "Saved domain policy changed locally; delivery does not overwrite it.", Attention: true})
	}
	observers := activeStatusObservers(target, now)
	text := "Domain saved; no active dashboard observation."
	if len(observers) > 0 {
		text = "Domain loaded by active dashboards."
	}
	for _, observer := range observers {
		if observer.DomainDigest != wanted || observer.CatalogGeneration != domain.Generation.Generation {
			text = "Domain saved; dashboard reopen required."
		}
	}
	rows = append(rows, DashboardUpdateRow{HostID: target.HostID, Text: text + " Service HTTPS verification unknown.", Attention: text != "Domain loaded by active dashboards."})
	return rows
}
func feedStatusRows(target update.ConfigurationTarget, recipient update.RecipientConfiguration, now time.Time) []DashboardUpdateRow {
	if recipient.Feed == nil {
		return nil
	}
	row := DashboardUpdateRow{HostID: target.HostID, Attention: true, Text: "Feed publication pending; loaded feed unknown."}
	if target.Receipt == nil || target.Receipt.Revision == 0 {
		return []DashboardUpdateRow{row}
	}
	state := target.Receipt.FeedState
	if state == "" && target.Receipt.Domain == nil {
		state = target.Receipt.State
	}
	if state != updateStatusSaved {
		return []DashboardUpdateRow{row}
	}
	wanted := (update.FeedValue{Present: true, URL: recipient.Feed.URL}).Digest()
	row.Text = "Feed saved; no active dashboard observation."
	if target.ObservedFeed != nil && target.ObservedFeed.Digest != wanted {
		row.Text = "Feed saved; selected usage feed changed locally."
		return []DashboardUpdateRow{row}
	}
	observers := activeStatusObservers(target, now)
	if len(observers) == 0 {
		return []DashboardUpdateRow{row}
	}
	rows := make([]DashboardUpdateRow, 0, len(observers))
	for _, observer := range observers {
		rows = append(rows, feedObserverRow(target.HostID, wanted, observer, now))
	}
	return rows
}
func feedObserverRow(host, wanted string, observer update.ConfigurationObservation, now time.Time) DashboardUpdateRow {
	row := DashboardUpdateRow{HostID: host, Attention: true, Text: "Feed saved; dashboard reload pending."}
	if observer.Digest != wanted || observer.State == "invalid" {
		return row
	}
	if !observer.Enabled {
		row.Text, row.Attention = "Feed saved and disabled in the active dashboard; quota unknown.", false
		return row
	}
	row.Text = "Feed loaded; awaiting transport and parse observations; quota unknown."
	if observer.Failing {
		row.Text = "Feed loaded; transport or parse failed; retained quota stays stale or unknown."
	}
	if !observer.Failing && !observer.PublishedAt.IsZero() {
		row.Text, row.Attention = "Feed loaded and parsed; quota freshness follows upstream observations.", false
	}
	row.Text += " HTTP " + statusTime(observer.HTTPAt) + "; publication " + statusTime(observer.PublishedAt) + "; upstream "
	if observer.UpstreamAt == nil {
		row.Attention = true
		row.Text += "unknown; quota unknown."
		return row
	}
	row.Text += statusTime(*observer.UpstreamAt) + "."
	if now.Sub(*observer.UpstreamAt) >= 15*time.Minute {
		row.Text += " Upstream observations stale."
		row.Attention = true
	}
	return row
}
func activeStatusObservers(target update.ConfigurationTarget, now time.Time) []update.ConfigurationObservation {
	return slices.DeleteFunc(slices.Clone(target.Observers), func(observer update.ConfigurationObservation) bool {
		return observer.HostID != target.HostID || observer.RenewedAt.IsZero() || now.Sub(observer.RenewedAt) >= update.ConfigurationObserverLease
	})
}
func statusTime(at time.Time) string {
	if at.IsZero() {
		return agentUnknown
	}
	return at.UTC().Format(time.RFC3339)
}

func automaticStatusRows(value update.AutomaticStatus) []DashboardUpdateRow {
	status := &value
	coordinator := ""
	if status.Record != nil {
		coordinator = status.Record.Policy.CoordinatorID
	}

	rows := []DashboardUpdateRow{{HostID: coordinator, Text: "Automatic updates: " + SafeTerminalText(status.State) + "."}}
	record := status.Record
	if record == nil {
		return rows
	}
	if record.LastCompleted == nil {
		rows = append(rows, DashboardUpdateRow{HostID: coordinator, Text: "Last successful fleet release unknown."})
	} else {
		rows = append(rows, DashboardUpdateRow{HostID: coordinator, Text: "Last successful fleet release " + SafeTerminalText(record.LastCompleted.Version) + "; " + statusTime(record.LastCompleted.At) + "."})
	}

	if record.Stop != nil {
		rows = append(rows, automaticStopRow(*record.Stop))
	}
	if status.Problem != "" || status.TransportProblem != "" {
		rows = append(rows, DashboardUpdateRow{HostID: coordinator, Text: "Automatic policy has a problem; inspect mesh update automatic status --details.", Attention: true})
	}
	if record.Continuation != nil {
		rows = append(rows, DashboardUpdateRow{HostID: coordinator, Text: "Pinned manual continuation: " + SafeTerminalText(record.Continuation.State) + "; binary policy remains separate."})
	}
	if record.Canary != nil {
		text := "Canary " + SafeTerminalText(record.Canary.State) + "; later activation waits for this attempt's proof."
		if record.Canary.State == "verified" {
			text = "Canary verified for this attempt; rollout follows the pinned policy."
		}
		rows = append(rows, DashboardUpdateRow{HostID: record.Canary.HostID, Text: text, Attention: record.Canary.State != "verified"})
	}
	if record.Current != nil {
		rows = append(rows, automaticTargetRows(*record.Current)...)
	}
	return rows
}
func automaticTargetRows(run update.Run) []DashboardUpdateRow {
	rows := make([]DashboardUpdateRow, 0, len(run.Targets))
	for _, target := range run.Targets {
		running := "daemon version unknown"
		if target.Build != nil && target.Build.Version != "" {
			running = "last observed daemon " + SafeTerminalText(target.Build.Version)
		}
		text := "Target " + SafeTerminalText(run.Release.Version) + "; " + running + "; " + SafeTerminalText(string(target.State)) + "."
		if target.State == update.Offline {
			text += " Pending at the exact pinned release."
		}
		if target.Grant || target.StageUnsettled || target.RetryUnsettled || target.JoinedUnsettled {
			text += " Issued or joined work may still finish."
		}
		if target.Problem != "" {
			text += " Inspect mesh update status --details."
		}
		text += " Run observed " + statusTime(run.UpdatedAt) + "."
		rows = append(rows, DashboardUpdateRow{HostID: target.Host.ID, Text: text, Attention: target.State != update.Updated && target.State != update.Newer})
	}
	return rows
}
func automaticStopRow(stop update.AutomaticStop) DashboardUpdateRow {
	text := "Stopped; review manual bridge if a direct transition is blocked. Inspect details before retry or cancel and acknowledge-stop."
	if stop.Kind == "recovering" {
		text = "Pinned recovery approved; the prior stop remains recorded for this attempt."
	}
	if stop.Kind == "cancelled" {
		text = "Automatic policy disabled and cancelled; settle issued work before acknowledge-stop."
	}
	return DashboardUpdateRow{HostID: stop.HostID, Text: text, Attention: true}
}
