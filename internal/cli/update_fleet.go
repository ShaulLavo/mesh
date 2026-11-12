package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/privacy"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/updateinstall"
	"github.com/spf13/cobra"
)

const (
	fleetCurrent = "current"
	fleetPending = "pending"
	fleetUnknown = agentUnknown
	fleetFailed  = "failed"
	fleetOffline = "offline"
	fleetRefused = "refused"
)

type FleetUpdateMember struct {
	Host       update.Host                 `json:"host"`
	Build      *release.Build              `json:"build"`
	Update     *protocol.HostUpdateSummary `json:"update"`
	State      string                      `json:"state"`
	Problem    string                      `json:"problem,omitempty"`
	ObservedAt time.Time                   `json:"observedAt"`
	LastReply  time.Time                   `json:"lastReply"`
}

type FleetUpdateProblem struct {
	HostID  string `json:"hostId"`
	State   string `json:"state"`
	Problem string `json:"problem"`
}

type FleetUpdateStatus struct {
	Members  []FleetUpdateMember  `json:"members"`
	Problems []FleetUpdateProblem `json:"problems"`
	ExitCode int                  `json:"exitCode"`
}

func (a *application) updateFleetCommand(options *updateOptions) *cobra.Command {
	return &cobra.Command{Use: "fleet", Short: "Read update status from every machine in your saved fleet", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runUpdateFleet(cmd, options)
		}}
}

func (a *application) runUpdateFleet(cmd *cobra.Command, options *updateOptions) error {
	if options.coordinator != "" {
		return errors.New("fleet status reads each machine directly; omit --coordinator")
	}
	environment, err := existingAutomaticEnvironment(cmd.Context(), "")
	if err != nil {
		return err
	}
	fleet, err := savedUpdateFleet()
	if err != nil {
		return err
	}
	status := readFleetUpdates(cmd.Context(), fleet.Members, environment.local.ID, a.dependencies.DialPassive, a.dependencies.Now())
	if options.json {
		err = writeAutomaticJSON(cmd.OutOrStdout(), status)
	} else {
		err = printFleetUpdates(cmd.OutOrStdout(), status, options.details, a.privacy)
	}
	if err != nil {
		return err
	}
	return updateExit(status.ExitCode)
}

func savedUpdateFleet() (update.Fleet, error) {
	path, err := ConfigPath()
	if err != nil {
		return update.Fleet{}, err
	}
	fleet, err := update.ReadFleet(filepath.Join(filepath.Dir(path), "fleet.json"))
	if err != nil {
		return fleet, fmt.Errorf("read saved fleet for update status: %w", err)
	}
	return fleet, nil
}

func readFleetUpdates(ctx context.Context, members []update.Host, localID string, dial HostDialer, now time.Time) FleetUpdateStatus {
	status := FleetUpdateStatus{Members: make([]FleetUpdateMember, len(members)), Problems: []FleetUpdateProblem{}}
	last := fleetLastReplies(ctx, members)
	limit := make(chan struct{}, 8)
	var readers sync.WaitGroup
	for index, host := range members {
		readers.Go(func() {
			status.Members[index] = observeFleetUpdate(ctx, host, localID, dial, limit)
		})
	}
	readers.Wait()
	for index := range status.Members {
		member := &status.Members[index]
		member.LastReply = last[member.Host.ID]
		code := classifyFleetUpdate(member, now)
		status.ExitCode = combineFleetExit(status.ExitCode, code)
		if code == 1 {
			status.Problems = append(status.Problems, FleetUpdateProblem{member.Host.ID, member.State, member.Problem})
		}
	}
	return status
}

func observeFleetUpdate(ctx context.Context, host update.Host, localID string, dial HostDialer, limit chan struct{}) FleetUpdateMember {
	member := FleetUpdateMember{Host: host}
	select {
	case limit <- struct{}{}:
		defer func() { <-limit }()
	case <-ctx.Done():
		member.State, member.Problem = fleetOffline, ctx.Err().Error()
		return member
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	record := HostRecord{ID: host.ID, MeshIdentity: host.ID, Endpoint: host.Endpoint, local: host.ID == localID}
	conn, info, err := openAgentHostInfo(ctx, record, dial)
	member.ObservedAt = time.Now().UTC()
	if err != nil {
		member.State, member.Problem = fleetObservationError(err), err.Error()
		observeLocalClient(ctx, record, &member)
		return member
	}
	_ = conn.Close()
	member.Build, member.Update = info.Build, info.HostUpdate
	member.Host.MachineName = info.MachineName
	return member
}

func observeLocalClient(ctx context.Context, record HostRecord, member *FleetUpdateMember) {
	if !record.local {
		return
	}
	state, err := paths.StateDirPath()
	if err != nil || !localDaemonAbsent(ctx, state) {
		return
	}
	build := release.Current()
	member.Build = &build
	member.Update = &protocol.HostUpdateSummary{Installation: updateinstall.InstallationKind("", build, false)}
	member.State, member.Problem = "", ""
}

func fleetObservationError(err error) string {
	var remote *update.RemoteError
	var changed *hostIdentityError
	var refused *ControlError
	if errors.Is(err, transport.ErrAuthentication) || errors.As(err, &remote) || errors.As(err, &changed) || errors.As(err, &refused) {
		return fleetRefused
	}
	var network *net.OpError
	if errors.As(err, &network) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return fleetOffline
	}
	return fleetFailed
}

func fleetLastReplies(ctx context.Context, members []update.Host) map[string]time.Time {
	result := make(map[string]time.Time)
	cache, err := openAgentCatalogCache(ctx)
	if err != nil || cache.store == nil {
		return result
	}
	defer func() { _ = cache.Close() }()
	for _, host := range members {
		stored, err := cache.store.GetHost(ctx, storage.HostID(host.ID))
		if err == nil && stored.MeshIdentity == host.ID {
			result[host.ID] = stored.LastSeenAt
		}
	}
	return result
}

func classifyFleetUpdate(member *FleetUpdateMember, now time.Time) int {
	if code, handled := classifyFleetConnection(member, now); handled {
		return code
	}
	if member.Update == nil || member.Build == nil {
		member.State, member.Problem = fleetUnknown, "Automatic update status is unknown. This machine may need adoption."
		return 1
	}
	if code, handled := classifyFleetProblems(member); handled {
		return code
	}
	if code, handled := classifyFleetEligibility(member); handled {
		return code
	}
	if verifiedFleetCurrent(member, now) {
		member.State = fleetCurrent
		return 0
	}
	return classifyPendingFleetUpdate(member, now)
}

func classifyFleetConnection(member *FleetUpdateMember, now time.Time) (int, bool) {
	if member.State == fleetOffline {
		if !member.LastReply.IsZero() && !member.LastReply.After(now) && now.Sub(member.LastReply) < 24*time.Hour {
			return 2, true
		}
		member.Problem = "Machine is unreachable; its last verified reply is unknown or more than a day old."
		return 1, true
	}
	if member.State == fleetRefused || member.State == fleetFailed {
		return 1, true
	}
	return 0, false
}

func classifyFleetEligibility(member *FleetUpdateMember) (int, bool) {
	summary := member.Update
	if _, err := release.CompareVersions(member.Build.Version, member.Build.Version); err != nil || member.Build.Modified {
		member.State, member.Problem = updateinstall.InstallationDevelopment, "This development build needs manual updates."
		return 1, true
	}
	if summary.Installation != updateinstall.InstallationManaged {
		member.State, member.Problem = summary.Installation, "This machine needs a managed installation before automatic updates can keep it current."
		if member.State != updateinstall.InstallationPackage && member.State != updateinstall.InstallationClient && member.State != updateinstall.InstallationDevelopment {
			member.State = fleetUnknown
		}
		return 1, true
	}
	if summary.Mode != string(update.HostAutomatic) {
		member.State, member.Problem = string(update.HostManual), "Automatic updates are off on this machine."
		if summary.Mode != string(update.HostManual) {
			member.State, member.Problem = fleetUnknown, "The automatic update policy is unknown."
		}
		return 1, true
	}
	return 0, false
}

func classifyFleetProblems(member *FleetUpdateMember) (int, bool) {
	summary := member.Update
	if summary.Problem != "" || summary.Phase == string(update.HostAttention) {
		member.State, member.Problem = fleetFailed, summary.Problem
		if member.Problem == "" {
			member.Problem = "This update needs attention."
		}
		return 1, true
	}
	if summary.WaitingState == string(update.EvidenceRefused) {
		member.State, member.Problem = fleetRefused, "An earlier machine refused access to its update history. Review its device approval."
		return 1, true
	}
	if summary.WaitingState == string(update.EvidenceInvalid) {
		member.State, member.Problem = fleetFailed, "Mesh could not verify an earlier machine's update report."
		return 1, true
	}
	if summary.WaitingState == string(update.EvidenceUnsupported) {
		member.State, member.Problem = fleetUnknown, "An earlier machine still needs adoption."
		return 1, true
	}
	return 0, false
}

func verifiedFleetCurrent(member *FleetUpdateMember, now time.Time) bool {
	summary := member.Update
	if !summary.SourceVerified || summary.Latest == nil || summary.Lag == nil || *summary.Lag != 0 || member.Build.Modified || !validUpdateDigest(member.Build.Digest) || !validUpdateDigest(summary.Latest.Metadata) {
		return false
	}
	if summary.ObservedAt == nil || summary.ObservedAt.After(now) || now.Sub(*summary.ObservedAt) > 40*time.Minute {
		return false
	}
	order, err := release.CompareVersions(member.Build.Version, summary.Latest.Version)
	return err == nil && order >= 0
}

func classifyPendingFleetUpdate(member *FleetUpdateMember, now time.Time) int {
	summary := member.Update
	if summary.PendingSince == nil || summary.PendingSince.IsZero() || summary.PendingSince.After(now) {
		member.State, member.Problem = fleetUnknown, "The latest verified update or its waiting time is unknown."
		return 1
	}
	elapsed := now.Sub(*summary.PendingSince)
	if elapsed > summary.Soak && elapsed-summary.Soak > 6*time.Hour {
		member.State, member.Problem = fleetFailed, "The update has been waiting more than six hours beyond its configured wait."
		return 1
	}
	member.State = fleetPending
	return 2
}

func combineFleetExit(previous, next int) int {
	if previous == 1 || next == 1 {
		return 1
	}
	return max(previous, next)
}

func printFleetUpdates(out io.Writer, status FleetUpdateStatus, details bool, mask *privacy.Mask) error {
	writer := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	for index, member := range status.Members {
		label := fmt.Sprintf("Machine %d", index+1)
		if member.Host.MachineName != "" {
			label = mask.Value("host", member.Host.MachineName)
		}
		if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\n", SafeTerminalText(label), fleetVersion(member), member.State); err != nil {
			return fmt.Errorf("print fleet update: %w", err)
		}
		if err := printFleetUpdateDetail(writer, member, details, mask); err != nil {
			return err
		}
	}
	if err := writer.Flush(); err != nil {
		return fmt.Errorf("print fleet status: %w", err)
	}
	return nil
}

func fleetVersion(member FleetUpdateMember) string {
	if member.Build == nil || member.Build.Version == "" {
		return "version unknown"
	}
	return SafeTerminalText(member.Build.Version)
}

func printFleetUpdateDetail(out io.Writer, member FleetUpdateMember, details bool, mask *privacy.Mask) error {
	text := fleetUpdateDescription(member)
	if _, err := fmt.Fprintln(out, "  "+text); err != nil {
		return fmt.Errorf("print fleet details: %w", err)
	}
	if details {
		return printUpdateDetails(out, member, mask)
	}
	return nil
}

func fleetUpdateDescription(member FleetUpdateMember) string {
	summary := member.Update
	if summary == nil {
		return fleetUpdateAttention(member) + " Use mesh update fleet --details to inspect this machine."
	}
	target, latest := agentUnknown, agentUnknown
	if summary.Target != nil {
		target = SafeTerminalText(summary.Target.Version)
	}
	if summary.Latest != nil {
		latest = SafeTerminalText(summary.Latest.Version)
	}
	lag := agentUnknown
	if summary.Lag != nil {
		lag = fmt.Sprint(*summary.Lag)
	}
	text := fmt.Sprintf("%s; automatic updates %s; %s; target %s; latest %s; releases behind %s", fleetInstallationText(summary.Installation), hostPolicyMode(update.HostMode(summary.Mode)), fleetPhaseText(summary.Phase), target, latest, lag)
	if summary.Last != nil {
		text += "; last update " + SafeTerminalText(summary.Last.Version) + " at " + summary.Last.At.UTC().Format(time.RFC3339)
	} else {
		text += "; last update unknown"
	}
	if member.Problem != "" || summary.Pending != "" {
		text += ". " + fleetUpdateAttention(member)
	}
	return text
}

func fleetInstallationText(kind string) string {
	switch kind {
	case updateinstall.InstallationManaged:
		return "Mesh manages this installation"
	case updateinstall.InstallationPackage:
		return "Package manager handles this installation"
	case updateinstall.InstallationClient:
		return "Command only"
	case updateinstall.InstallationDevelopment:
		return "Development build"
	default:
		return "Installation unknown"
	}
}

func fleetPhaseText(phase string) string {
	switch phase {
	case string(update.HostIdle):
		return "no update running"
	case string(update.HostDiscovering):
		return "checking for updates"
	case string(update.HostWaiting):
		return "waiting"
	case string(update.HostInstalling):
		return "updating"
	case string(update.HostAttention):
		return "needs attention"
	default:
		return "status unknown"
	}
}

func fleetUpdateAttention(member FleetUpdateMember) string {
	switch member.State {
	case fleetPending:
		return "Waiting for this update or an earlier machine. Use --details for the reason."
	case fleetOffline:
		return "This machine is unreachable."
	case fleetRefused:
		return "This machine refused access. Review its device approval."
	case fleetUnknown:
		return "Update status is unknown. Use --details to inspect."
	default:
		return "This machine needs attention. Use --details to inspect."
	}
}
