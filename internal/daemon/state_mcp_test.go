package daemon

import (
	"testing"
	"time"

	"github.com/shaul/mesh/internal/mcphost"
	"github.com/shaul/mesh/internal/protocol"
)

// The mcp topic carries each hosted service's listing revision: in the
// snapshot, as coalesced events, and as a removal with an empty token.
func TestWatchMCPListingRevisions(t *testing.T) {
	now := time.Now()
	b := newStateBroker(4, func() time.Time { return now })
	b.mcpListingChanged(mcphost.ListingRevision{ServiceID: "s1", Token: "1.e.0"})
	sub, initial, err := b.subscribe(protocol.StateWatch{Topics: []string{protocol.TopicMCP}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.unsubscribe(sub)
	if len(initial.MCPListings) != 1 || initial.MCPListings[0].Token != "1.e.0" {
		t.Fatalf("snapshot %+v", initial.MCPListings)
	}
	b.mcpListingChanged(mcphost.ListingRevision{ServiceID: "s1", Token: "1.e.1"})
	b.mcpListingChanged(mcphost.ListingRevision{ServiceID: "s1", Token: "1.e.2"})
	b.mcpListingChanged(mcphost.ListingRevision{ServiceID: "s2", Token: ""})
	messages := b.take(sub)
	if len(messages) != 2 || messages[0].StateEvent.Payload.MCPListing.Token != "1.e.2" || messages[1].StateEvent.Payload.MCPListing.Token != "" {
		t.Fatalf("events %+v", messages)
	}
	other, _, err := b.subscribe(protocol.StateWatch{Topics: []string{protocol.TopicSessions}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.unsubscribe(other)
	b.mcpListingChanged(mcphost.ListingRevision{ServiceID: "s1", Token: "1.e.3"})
	if got := b.take(other); len(got) != 0 {
		t.Fatalf("a sessions watcher got mcp events: %+v", got)
	}
}
