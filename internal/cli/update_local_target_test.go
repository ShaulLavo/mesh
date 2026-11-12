package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/update"
)

type localTargetCLIFixture struct {
	deps       Dependencies
	build      release.Build
	plan       update.HostTargetPlan
	status     update.HostTargetStatus
	submission *update.HostTargetSubmission
	planError  error
	badReceipt bool
}

func newLocalTargetCLIFixture(t *testing.T) *localTargetCLIFixture {
	t.Helper()
	_, host := setupUpdateCLI(t)
	f := &localTargetCLIFixture{build: release.Build{Version: "v0.1.1", Digest: strings.Repeat("a", 64), Platform: release.CurrentPlatform()}}
	target := update.HostTargetRelease{Version: "v0.2.0", Metadata: strings.Repeat("b", 64), Manifest: strings.Repeat("c", 64), Binary: strings.Repeat("d", 64)}
	next := &update.HostTargetRelease{Version: "v0.1.2", Metadata: strings.Repeat("e", 64), Manifest: strings.Repeat("f", 64), Binary: strings.Repeat("1", 64), Forward: strings.Repeat("2", 64), Rollback: strings.Repeat("3", 64)}
	f.plan = update.HostTargetPlan{PolicyRevision: 7, CurrentDigest: f.build.Digest, Target: target, Next: next}
	f.status = update.HostTargetStatus{Schema: 1, Phase: update.HostTargetIdle}
	f.deps = Dependencies{UpdateRelease: release.Client{BaseURL: "://manifest-latest-must-not-be-read"}, UpdateCaller: updateCallFunc(f.call),
		DialPassive: func(context.Context, HostRecord) (transport.Conn, error) {
			return newServiceRemoteTestConn(func(protocol.Control) protocol.Control {
				return protocol.Control{Type: protocol.TypeHostInfoResult, Host: &protocol.HostInfo{ID: host.ID, MeshIdentity: host.ID, Build: &f.build, HostUpdate: &protocol.HostUpdateSummary{Mode: "manual", Installation: "managed"}}}
			}), nil
		}}
	return f
}

func (f *localTargetCLIFixture) call(_ context.Context, host update.Host, action string, input, output any) error {
	if !update.IsLocal(host) {
		return errors.New("local target was sent to another machine")
	}
	switch action {
	case "host-policy.show":
		*output.(*update.HostPolicy) = update.HostPolicy{Schema: 1, Revision: 7, Mode: update.HostManual}
	case update.HostTargetStatusAction:
		*output.(*update.HostTargetStatus) = f.status
	case update.HostTargetPlanAction:
		if f.planError != nil {
			return f.planError
		}
		*output.(*update.HostTargetPlan) = f.plan
	case update.HostTargetSubmitAction:
		f.submit(input, output)
	case update.HostTargetCancelAction:
		if input.(uint64) != f.status.Revision {
			return errors.New("stale cancellation")
		}
		f.status.Revision++
		f.status.Phase = update.HostTargetCancelled
		*output.(*update.HostTargetStatus) = f.status
	default:
		return errors.New("unexpected legacy control or policy mutation: " + action)
	}
	return nil
}

func (f *localTargetCLIFixture) submit(input, output any) {
	submission := input.(update.HostTargetSubmission)
	f.submission = &submission
	f.status = update.HostTargetStatus{Schema: 1, Revision: submission.ExpectedRevision + 1, PolicyRevision: 7, Phase: update.HostTargetReserved, CurrentDigest: submission.CurrentDigest,
		Target: f.plan.Target, Next: f.plan.Next, Request: strings.Repeat("4", 32), Generation: 1}
	if f.badReceipt {
		f.status.Request, f.status.Generation = "", 0
	}
	*output.(*update.HostTargetStatus) = f.status
}

func TestLocalTargetCLIPreviewKeepsRealPinsAndDoesNotSubmit(t *testing.T) {
	f := newLocalTargetCLIFixture(t)
	stdout, _, err := executeCommand(t, f.deps, "update", "--local", "--check", "--json")
	if err != nil || f.submission != nil {
		t.Fatalf("preview reserved work: %s %v", stdout, err)
	}
	var plan update.HostTargetPlan
	if err := json.Unmarshal([]byte(stdout), &plan); err != nil || plan.Next == nil || plan.Next.Forward != f.plan.Next.Forward || plan.Target.Metadata != f.plan.Target.Metadata {
		t.Fatalf("preview lost verified evidence: %s %v", stdout, err)
	}
	if strings.Contains(stdout, `"request"`) || strings.Contains(stdout, `"generation"`) {
		t.Fatal("preview claimed a reservation", stdout)
	}
	stdout, _, err = executeCommand(t, f.deps, "update", "--local", "--check")
	if err != nil || !strings.Contains(stdout, "v0.1.1 → v0.1.2") || !strings.Contains(stdout, "Mesh v0.2.0") || strings.Contains(stdout, f.plan.Target.Metadata) {
		t.Fatalf("next-hop/source preview: %s %v", stdout, err)
	}
}

func TestLocalTargetCLISubmitsExactPlannedVersionWithManualPolicy(t *testing.T) {
	f := newLocalTargetCLIFixture(t)
	stdout, _, err := executeCommand(t, f.deps, "update", "--local", "--yes", "--json")
	code, _ := StatusCode(err)
	if code != 2 || f.submission == nil || f.submission.Version != f.plan.Target.Version || f.submission.CurrentDigest != f.build.Digest || f.submission.PolicyRevision != 7 || f.submission.ExpectedRevision != 0 {
		t.Fatalf("submission was not bound to preview/current policy: %+v %s %v", f.submission, stdout, err)
	}
	var status update.HostTargetStatus
	if err := json.Unmarshal([]byte(stdout), &status); err != nil || status.Request == "" || status.Generation == 0 || status.Target.Metadata != f.plan.Target.Metadata {
		t.Fatalf("submission did not return its actual receipt: %s %v", stdout, err)
	}
}

func TestLocalTargetCLICurrentAndFailedPlansNeverReserve(t *testing.T) {
	f := newLocalTargetCLIFixture(t)
	f.plan.Target.Version, f.plan.Target.Binary, f.plan.Next = f.build.Version, f.build.Digest, nil
	stdout, _, err := executeCommand(t, f.deps, "update", "--local", "--yes")
	if err != nil || f.submission != nil || !strings.Contains(stdout, "already runs the verified release") {
		t.Fatalf("verified current plan submitted work: %s %v", stdout, err)
	}
	f.planError = errors.New("source verification refused the route")
	stdout, _, err = executeCommand(t, f.deps, "update", "--local", "--check", "--json")
	if err == nil || !strings.Contains(stdout, f.planError.Error()) || f.submission != nil {
		t.Fatalf("failed planner fell through to legacy approval: %v", err)
	}
}

func TestLocalTargetCLIRejectsMissingDurableRequest(t *testing.T) {
	f := newLocalTargetCLIFixture(t)
	f.badReceipt = true
	stdout, _, err := executeCommand(t, f.deps, "update", "--local", "--yes", "--json")
	if err == nil || stdout != "" || !strings.Contains(err.Error(), "durable update request") {
		t.Fatalf("missing reservation was reported successful: %s %v", stdout, err)
	}
}

func TestLocalTargetCLIRejoinsOnlyUnchangedApproval(t *testing.T) {
	f := newLocalTargetCLIFixture(t)
	f.status = update.HostTargetStatus{Schema: 1, Revision: 1, PolicyRevision: 7, Phase: update.HostTargetInstalling,
		Target: f.plan.Target, Next: f.plan.Next, Request: strings.Repeat("4", 32), Generation: 1}
	_, _, err := executeCommand(t, f.deps, "update", "--local", "--yes", "--json")
	if code, _ := StatusCode(err); code != 2 || f.submission != nil {
		t.Fatalf("unchanged active target was submitted again: %+v %v", f.submission, err)
	}
	f.status.PolicyRevision = 6
	stdout, _, err := executeCommand(t, f.deps, "update", "--local", "--yes", "--json")
	if code, _ := StatusCode(err); code != 1 || f.submission != nil || !strings.Contains(stdout, "differs from the reviewed") {
		t.Fatalf("policy revision change was reported as joined work: %s %v", stdout, err)
	}
}

func TestLocalTargetCLIStatusCancelAndRetryUseRealRevision(t *testing.T) {
	f := newLocalTargetCLIFixture(t)
	f.status = update.HostTargetStatus{Schema: 1, Revision: 5, Phase: update.HostTargetAttention, Target: f.plan.Target, Next: f.plan.Next}
	_, _, err := executeCommand(t, f.deps, "update", "status", "--local", "--json")
	if code, _ := StatusCode(err); code != 1 {
		t.Fatal("failed local status did not exit 1", err)
	}
	_, _, err = executeCommand(t, f.deps, "update", "retry", "--local", "--json")
	if code, _ := StatusCode(err); code != 2 || f.submission == nil || f.submission.ExpectedRevision != 5 || f.submission.RetryEdge == nil || f.submission.RetryEdge.From != f.build.Digest || f.submission.RetryEdge.To != f.plan.Next.Binary {
		t.Fatalf("retry lost exact failed edge/current revision: %+v %v", f.submission, err)
	}
	_, _, err = executeCommand(t, f.deps, "update", "cancel", "--local", "--json")
	if code, _ := StatusCode(err); code != 1 || f.status.Phase != update.HostTargetCancelled {
		t.Fatal("local cancellation not saved", err)
	}
}
