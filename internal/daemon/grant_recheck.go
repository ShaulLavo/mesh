package daemon

import (
	"context"
	"fmt"

	"github.com/shaul/mesh/internal/access"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

// grantStillCurrent rechecks the device grant that admitted a request. A
// mutation can wait behind a gate after admission, and its effect and reply
// must not outlive a revocation in between. Requests without a peer (the
// local socket and the daemon's own work) have no grant to lose.
func grantStillCurrent(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("daemon: request cancelled: %w", err)
	}
	if peer, ok := transport.Peer(ctx); ok && !peer.Current() {
		return fmt.Errorf("daemon: device grant changed after admission: %w", access.ErrDenied)
	}
	return nil
}

// replyAdmission returns the check a state-changing control's reply must
// pass at delivery, or nil for reads.
func replyAdmission(ctx context.Context, kind string) func() error {
	switch kind {
	case protocol.TypeServiceUpsert, protocol.TypeServiceDelete, protocol.TypeServiceLabel,
		protocol.TypeServiceStart, protocol.TypeServiceStop,
		protocol.TypeHibernate, protocol.TypeRecover, protocol.TypeRecoveryCommand:
		return func() error { return grantStillCurrent(ctx) }
	default:
		return nil
	}
}
