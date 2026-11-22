package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/mcpgateway"
	"github.com/shaul/mesh/internal/protocol"
)

// Watch follows one host's hosted listing revisions on the mcp topic of
// state.watch (PLAN section 7.3). The first snapshot is the baseline; only
// later changes are reported. A host that cannot serve the topic gets
// mcpgateway.ErrWatchUnsupported, and the gateway refreshes it itself.
func (c *gatewayCatalog) Watch(ctx context.Context, hostID string, changed func(mcpgateway.ListingChange)) error {
	host, err := c.hosts.lookup(hostID)
	if err != nil {
		return err
	}
	release, err := c.pool.AcquireStream(ctx, host)
	if err != nil {
		return gatewayFailure(hostID, err)
	}
	defer release()
	conn, _, err := openAgentHostInfo(ctx, host, c.pool.dial)
	if err != nil {
		return gatewayFailure(hostID, err)
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	id, err := newDaemonRequestID()
	if err != nil {
		return fmt.Errorf("watch %s listings: %w", hostID, err)
	}
	watch := protocol.StateWatch{Topics: []string{protocol.TopicMCP}}
	first, err := controlRequest(ctx, conn, protocol.Control{Type: protocol.TypeStateWatch, RequestID: id, Watch: &watch})
	if err != nil {
		return watchEnded(ctx, hostID, err)
	}
	if first.Type == protocol.TypeError {
		return watchRefused(hostID, first)
	}
	if first.Type != protocol.TypeStateSnapshot || first.StateSnapshot == nil {
		return mcpgateway.ErrWatchUnsupported
	}
	known := listingTokens(first.StateSnapshot.MCPListings)
	for {
		next, err := protocol.ReadControl(conn.ReadFrame, conn.WriteFrame)
		if err != nil {
			return watchEnded(ctx, hostID, err)
		}
		reportListings(next, known, changed)
	}
}

func listingTokens(rows []protocol.MCPListingRevision) map[string]string {
	tokens := make(map[string]string, len(rows))
	for _, row := range rows {
		tokens[row.ServiceID+"\x00"+row.Account] = row.Token
	}
	return tokens
}

// reportListings reports each changed listing revision once, from events
// and from the snapshot that follows a resync.
func reportListings(msg protocol.Control, known map[string]string, changed func(mcpgateway.ListingChange)) {
	var rows []protocol.MCPListingRevision
	switch {
	case msg.Type == protocol.TypeStateEvent && msg.StateEvent != nil && msg.StateEvent.Payload.MCPListing != nil:
		rows = []protocol.MCPListingRevision{*msg.StateEvent.Payload.MCPListing}
	case msg.Type == protocol.TypeStateSnapshot && msg.StateSnapshot != nil:
		rows = msg.StateSnapshot.MCPListings
	}
	for _, row := range rows {
		key := row.ServiceID + "\x00" + row.Account
		if known[key] == row.Token {
			continue
		}
		known[key] = row.Token
		changed(mcpgateway.ListingChange{ServiceID: row.ServiceID, Account: row.Account, Token: row.Token})
	}
}

// watchEnded classifies a broken watch; one the frontend itself cancelled
// ends quietly.
func watchEnded(ctx context.Context, hostID string, err error) error {
	if ctx.Err() != nil {
		return nil //nolint:nilerr // Cancelling the watch is how it ends.
	}
	return gatewayFailure(hostID, err)
}

// watchRefused tells a host that does not know the topic from one that
// refused this caller.
func watchRefused(hostID string, msg protocol.Control) error {
	if msg.ErrorCode == "" || msg.ErrorCode == protocol.ErrorCodeUnknownControl || msg.ErrorCode == string(agentapi.UsageInvalid) {
		return mcpgateway.ErrWatchUnsupported
	}
	err := typedControlError(msg.ErrorCode, protocol.TypeStateWatch, msg.Message, msg.Message)
	var rejected *ControlError
	if errors.As(err, &rejected) {
		return gatewayFailure(hostID, err)
	}
	return err
}
