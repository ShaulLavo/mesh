package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/shaul/mesh/internal/access"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/update"
)

const updateSummaryTimeout = 100 * time.Millisecond

func (u *updateController) evidence(ctx context.Context, request protocol.Control) (protocol.Control, bool, error) {
	dependent := u.id
	if peer, remote := transport.Peer(ctx); remote {
		if peer.Class() != access.FullDevice {
			return protocol.Control{}, true, errors.New("update evidence requires ordinary device access")
		}
		dependent = peer.Identity
	}
	var input update.EvidenceRequest
	if err := update.DecodeEvidence(request.Update, &input); err != nil {
		return protocol.Control{}, true, hostControllerError(err)
	}
	reply, err := u.coordinator.Store.HostEvidence(ctx, dependent, input)
	if err != nil {
		return protocol.Control{}, true, hostControllerError(err)
	}
	reply.ID = u.id
	data, err := json.Marshal(reply)
	return protocol.Control{Type: protocol.TypeUpdateEvidenceResult, RequestID: request.RequestID, Update: data}, true, hostControllerError(err)
}

func (u *updateController) hostSummary() *protocol.HostUpdateSummary {
	ctx, cancel := context.WithTimeout(context.Background(), updateSummaryTimeout)
	defer cancel()
	policy, err := u.coordinator.Store.HostPolicy(ctx)
	if err != nil {
		return &protocol.HostUpdateSummary{Phase: "attention", Problem: err.Error()}
	}
	state, err := u.coordinator.Store.HostState(ctx)
	if err != nil {
		return &protocol.HostUpdateSummary{Mode: string(policy.Mode), Phase: "attention", Problem: err.Error()}
	}
	summary := &protocol.HostUpdateSummary{Mode: string(policy.Mode), Revision: policy.Revision, Phase: string(state.Phase)}
	if state.Problem != nil {
		summary.Problem = state.Problem.Detail
	}
	return summary
}
