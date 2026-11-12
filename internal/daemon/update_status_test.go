package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/update"
)

func TestHostSummaryCurrentFutureAndUnknown(t *testing.T) {
	now := time.Now().UTC()
	build := release.Build{Version: "v0.1.0", Digest: strings.Repeat("a", 64)}
	observed := update.HostObservedRelease{Epoch: 1, Version: build.Version, Metadata: strings.Repeat("b", 64), PublishedAt: now.Add(-time.Hour)}
	status := update.HostStatus{Policy: update.HostPolicy{Mode: update.HostAutomatic}, State: update.HostUpdate{Phase: update.HostIdle},
		Discovery: &update.HostDiscoveryObservation{Source: build, Epoch: 1, LatestEpoch: 1, Head: observed, Target: observed, ObservedAt: now},
		Last:      &update.LedgerEntry{Version: build.Version, Metadata: observed.Metadata, Binary: build.Digest, CommittedAt: now}}
	summary := summarizeHostUpdate(status, build, "managed")
	if summary.Latest == nil || summary.Lag == nil || *summary.Lag != 0 || summary.Last == nil || summary.Last.Metadata != observed.Metadata {
		t.Fatalf("known status lost pins/currentness: %+v", summary)
	}
	status.Discovery.Source.Digest = strings.Repeat("c", 64)
	if matched := summarizeHostUpdate(status, build, "managed"); !matched.SourceVerified || matched.Lag == nil {
		t.Fatal("exact committed image and head pin were not recognized")
	}
	status.Last.Metadata = strings.Repeat("d", 64)
	if mismatched := summarizeHostUpdate(status, build, "managed"); mismatched.SourceVerified || mismatched.Lag != nil {
		t.Fatal("another metadata pin supplied currentness")
	}
	status.Discovery.LatestEpoch = 2
	summary = summarizeHostUpdate(status, build, "managed")
	if summary.Latest != nil || summary.Lag != nil || summary.Head == nil || summary.LatestEpoch != 2 {
		t.Fatalf("future latest was treated as compatible head: %+v", summary)
	}
	status.Discovery = nil
	summary = summarizeHostUpdate(status, build, "managed")
	data, err := json.Marshal(summary)
	if err != nil || !strings.Contains(string(data), `"latest":null`) || !strings.Contains(string(data), `"lag":null`) {
		t.Fatalf("unknown status was omitted or became zero: %s %v", data, err)
	}
}

func TestHostPolicyForgetRequiresUnixOwnerAndReleasesRealRetention(t *testing.T) {
	controller, owner, administrator, local, remote := policyControlFixture(t)
	if _, err := controller.coordinator.Store.HostEvidence(t.Context(), administrator.ID, update.EvidenceRequest{Settled: "v0.1.0", Limit: 1}); err != nil {
		t.Fatal(err)
	}
	forget := update.HostDependentForget{ID: administrator.ID}
	if err := administrator.Call(t.Context(), remote, "host-policy.forget", forget, nil); err == nil {
		t.Fatal("network administrator released retention")
	}
	if err := administrator.Call(t.Context(), local, "host-policy.forget", forget, nil); err == nil {
		t.Fatal("different local signer released retention")
	}
	deps, err := controller.coordinator.Store.HostDependents(t.Context())
	if err != nil || len(deps) != 1 {
		t.Fatal(deps, err)
	}
	if err := owner.Call(t.Context(), local, "host-policy.forget", forget, nil); err != nil {
		t.Fatal(err)
	}
	deps, err = controller.coordinator.Store.HostDependents(t.Context())
	if err != nil || len(deps) != 0 {
		t.Fatalf("forgotten dependent retained history: %+v %v", deps, err)
	}
}

func TestHostSummaryUsesOnlyConfiguredPredecessorsAndPrioritizesProblems(t *testing.T) {
	status := update.HostStatus{Policy: update.HostPolicy{After: []string{"reading", "refused"}}, Peers: []update.HostPeer{
		{ID: "reading", State: update.EvidenceReading, Held: true},
		{ID: "refused", State: update.EvidenceRefused, Held: true},
		{ID: "removed", State: update.EvidenceInvalid, Held: true},
	}}
	summary := summarizeHostUpdate(status, release.Build{}, "managed")
	if summary.WaitingState != string(update.EvidenceRefused) || summary.WaitingPeer != "refused" {
		t.Fatalf("pending or removed predecessor concealed refusal: %+v", summary)
	}
	status.Policy.After = []string{"reading"}
	summary = summarizeHostUpdate(status, release.Build{}, "managed")
	if summary.WaitingState != string(update.EvidenceReading) || summary.WaitingPeer != "reading" {
		t.Fatalf("removed predecessor still affected status: %+v", summary)
	}
}
