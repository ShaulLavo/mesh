package daemon

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/updateinstall"
)

func TestLocalTargetControlsRequireActualUnixOwner(t *testing.T) {
	u, owner, administrator, local, remote := policyControlFixture(t)
	build := release.Build{Version: "v0.1.1", Digest: strings.Repeat("a", 64), Platform: release.CurrentPlatform()}
	u.hostUpdater.Current = func() release.Build { return build }
	u.hostUpdater.Probe = func(context.Context) (updateinstall.Health, error) {
		return updateinstall.Health{HostID: owner.ID, Build: build}, nil
	}
	var sourceCalls atomic.Int32
	u.hostUpdater.Source = func(context.Context, release.Build) (update.HostSource, release.DocumentSource, error) {
		sourceCalls.Add(1)
		return update.HostSource{}, release.DocumentSource{}, errors.New("source fixture reached")
	}
	input := update.HostTargetSubmission{Version: "v0.1.2", CurrentDigest: build.Digest}
	for _, action := range []string{update.HostTargetPlanAction, update.HostTargetSubmitAction, update.HostTargetCancelAction, update.HostTargetStatusAction} {
		if err := owner.Call(t.Context(), remote, action, input, nil); err == nil || !strings.Contains(err.Error(), "local Unix socket") {
			t.Fatalf("network owner reached %s: %v", action, err)
		}
		if err := administrator.Call(t.Context(), local, action, input, nil); err == nil || !strings.Contains(err.Error(), "local Unix socket") {
			t.Fatalf("different local signer reached %s: %v", action, err)
		}
	}
	if sourceCalls.Load() != 0 {
		t.Fatal("denied caller reached planning")
	}
	for _, action := range []string{update.HostTargetPlanAction, update.HostTargetSubmitAction} {
		if err := owner.Call(t.Context(), local, action, input, nil); err == nil || !strings.Contains(err.Error(), "source fixture reached") {
			t.Fatalf("Unix owner did not reach real planner %s: %v", action, err)
		}
	}
	if sourceCalls.Load() != 2 {
		t.Fatal("local target did not invoke the real source boundary")
	}
	var status update.HostTargetStatus
	if err := owner.Call(t.Context(), local, update.HostTargetStatusAction, nil, &status); err != nil || status.Revision != 0 || status.Phase != update.HostTargetIdle {
		t.Fatalf("failed plan created a reservation: %+v %v", status, err)
	}
	if err := owner.Call(t.Context(), local, update.HostTargetCancelAction, uint64(0), nil); err == nil || !strings.Contains(err.Error(), "revision changed") {
		t.Fatalf("cancel did not use real target revision API: %v", err)
	}
}
