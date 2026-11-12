package daemon

import (
	"context"
	"os"
	"slices"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/updateinstall"
)

func (u *updateController) hostSummary() *protocol.HostUpdateSummary {
	ctx, cancel := context.WithTimeout(context.Background(), updateSummaryTimeout)
	defer cancel()
	status, err := u.coordinator.Store.HostStatus(ctx)
	build := release.Current()
	executable, _ := os.Executable()
	summary := summarizeHostUpdate(status, build, updateinstall.InstallationKind(executable, build, true))
	if err != nil {
		summary.Phase, summary.Problem = string(update.HostAttention), err.Error()
	}
	return summary
}

func summarizeHostUpdate(status update.HostStatus, build release.Build, installation string) *protocol.HostUpdateSummary {
	summary := &protocol.HostUpdateSummary{Mode: string(status.Policy.Mode), Revision: status.Policy.Revision,
		Phase: string(status.State.Phase), Installation: installation, Soak: status.Policy.Soak}
	summaryProblem(summary, status.State)
	summaryPeerProblem(summary, status.Peers, status.Policy.After)
	if discovery := status.Discovery; discovery != nil {
		summary.Head, summary.Target = summaryRelease(discovery.Head), summaryRelease(discovery.Target)
		summary.LatestEpoch = discovery.LatestEpoch
		summary.SourceVerified = summarySourceMatches(status, build)
		if discovery.Epoch == discovery.LatestEpoch {
			summary.Latest = summaryRelease(discovery.Head)
		}
		at := discovery.ObservedAt
		summary.ObservedAt = &at
		summaryLag(summary, build)
		summaryPending(summary, discovery)
	}
	if owned := status.State.Owned; owned != nil && summary.Target == nil {
		summary.Target = &protocol.HostUpdateRelease{Version: owned.Installation.Manifest.Version, Metadata: owned.Metadata}
	}
	if owned := status.State.Owned; owned != nil {
		summary.Next = &protocol.HostUpdateRelease{Version: owned.Installation.Manifest.Version, Metadata: owned.Metadata}
	}
	summaryLocalTarget(summary, status.LocalTarget)
	if last := status.Last; last != nil {
		summary.Last = &protocol.HostUpdateCommit{Version: last.Version, Metadata: last.Metadata, Binary: last.Binary, At: last.CommittedAt}
	}
	return summary
}

func summaryLocalTarget(summary *protocol.HostUpdateSummary, target *update.HostTargetStatus) {
	if target == nil || target.Phase == update.HostTargetIdle || target.Phase == update.HostTargetCommitted || target.Phase == update.HostTargetCancelled {
		return
	}
	summary.Target = &protocol.HostUpdateRelease{Version: target.Target.Version, Metadata: target.Target.Metadata}
	if target.Next != nil {
		summary.Next = &protocol.HostUpdateRelease{Version: target.Next.Version, Metadata: target.Next.Metadata}
	}
	at := target.PendingSince
	summary.PendingSince = &at
	summary.Pending = "An approved local update is in progress."
	summary.Phase = string(update.HostInstalling)
	if target.Phase == update.HostTargetWaiting {
		summary.Phase = string(update.HostWaiting)
	}
	if target.Phase == update.HostTargetAttention {
		summary.Phase, summary.Problem = string(update.HostAttention), "An approved local update needs attention."
	}
	if target.Problem != nil {
		summary.Pending = target.Problem.Detail
	}
}

func summarySourceMatches(status update.HostStatus, build release.Build) bool {
	if build.Modified || status.Discovery == nil {
		return false
	}
	source := status.Discovery.Source
	if source.Digest == build.Digest && source.Version == build.Version && source.Commit == build.Commit && source.Platform == build.Platform {
		return true
	}
	last := status.Last
	return last != nil && last.Metadata == status.Discovery.Head.Metadata && last.Binary == build.Digest && last.Version == build.Version
}

func summaryPeerProblem(summary *protocol.HostUpdateSummary, peers []update.HostPeer, configured []string) {
	priority := 0
	for _, peer := range peers {
		if !peer.Held || !slices.Contains(configured, peer.ID) {
			continue
		}
		if next := summaryPeerPriority(peer.State); next > priority {
			priority, summary.WaitingState, summary.WaitingPeer = next, string(peer.State), peer.ID
		}
	}
}

func summaryPeerPriority(state update.EvidenceReadState) int {
	switch state {
	case update.EvidenceRefused, update.EvidenceInvalid:
		return 3
	case update.EvidenceUnsupported:
		return 2
	case update.EvidenceVerified, update.EvidenceReading, update.EvidenceOffline:
		return 1
	default:
		return 1
	}
}

func summaryProblem(summary *protocol.HostUpdateSummary, state update.HostUpdate) {
	if state.Problem == nil {
		return
	}
	summary.Problem = state.Problem.Detail
	if state.Problem.Kind != "waiting" {
		return
	}
	summary.Pending, summary.Problem = state.Problem.Detail, ""
	if !state.ProblemSince.IsZero() {
		at := state.ProblemSince
		summary.PendingSince = &at
	}
}

func summaryLag(summary *protocol.HostUpdateSummary, build release.Build) {
	if summary.Latest == nil || !summary.SourceVerified {
		return
	}
	if comparison, err := release.CompareVersions(build.Version, summary.Latest.Version); err == nil && comparison >= 0 {
		lag := uint64(0)
		summary.Lag = &lag
	}
}

func summaryRelease(value update.HostObservedRelease) *protocol.HostUpdateRelease {
	return &protocol.HostUpdateRelease{Version: value.Version, Metadata: value.Metadata, PublishedAt: value.PublishedAt}
}

func summaryPending(summary *protocol.HostUpdateSummary, discovery *update.HostDiscoveryObservation) {
	if summary.Lag != nil && *summary.Lag == 0 {
		return
	}
	if !discovery.PendingSince.IsZero() && (summary.PendingSince == nil || discovery.PendingSince.After(*summary.PendingSince)) {
		at := discovery.PendingSince
		summary.PendingSince = &at
	}
	if summary.Pending == "" {
		summary.Pending = "A newer release is waiting to install."
	}
}
