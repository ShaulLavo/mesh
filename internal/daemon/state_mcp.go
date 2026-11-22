package daemon

import (
	"slices"
	"strings"

	"github.com/shaul/mesh/internal/mcphost"
	"github.com/shaul/mesh/internal/protocol"
)

// mcpListingChanged publishes one hosted service's listing revision on the
// mcp topic, so aggregate gateways refresh exactly the listings that changed
// (PLAN section 7.3).
func (b *stateBroker) mcpListingChanged(rev mcphost.ListingRevision) {
	b.mu.Lock()
	defer b.mu.Unlock()
	key := rev.ServiceID + "\x00" + rev.Account
	row := protocol.MCPListingRevision{ServiceID: rev.ServiceID, Account: rev.Account, Token: rev.Token}
	if rev.Token == "" {
		delete(b.mcpListings, key)
	} else {
		b.mcpListings[key] = row
	}
	b.publishLocked(protocol.TopicMCP, "mcp/"+key, protocol.StateEvent{Kind: "mcp.listing", Payload: protocol.StatePayload{MCPListing: &row}})
}

func (b *stateBroker) mcpSnapshotLocked() []protocol.MCPListingRevision {
	rows := make([]protocol.MCPListingRevision, 0, len(b.mcpListings))
	for _, row := range b.mcpListings {
		rows = append(rows, row)
	}
	slices.SortFunc(rows, func(a, c protocol.MCPListingRevision) int {
		return strings.Compare(a.ServiceID+"\x00"+a.Account, c.ServiceID+"\x00"+c.Account)
	})
	return rows
}
