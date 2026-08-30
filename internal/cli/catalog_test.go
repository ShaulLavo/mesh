package cli

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
)

type memoryCatalogCache struct {
	rows  map[string][]protocol.SessionInfo
	saved []string
}

func (c *memoryCatalogCache) Load(_ context.Context, host HostRecord) ([]protocol.SessionInfo, error) {
	return slices.Clone(c.rows[host.ID]), nil
}

func (c *memoryCatalogCache) Save(_ context.Context, host HostRecord, _ []protocol.SessionInfo) error {
	c.saved = append(c.saved, host.ID)
	return nil
}

func TestCollectHostSessionsUsesCacheWithoutWaitingPastDeadline(t *testing.T) {
	hosts := []HostRecord{
		{Alias: "fast", ID: "host-fast"},
		{Alias: "offline", ID: "host-offline"},
		{Alias: "stuck", ID: "host-stuck"},
	}
	created := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	cache := &memoryCatalogCache{rows: map[string][]protocol.SessionInfo{
		"host-offline": {{ID: "OLD1", HostID: "host-offline", Command: []string{"sh"}, State: "running", CreatedAt: created}},
		"host-stuck":   {{ID: "OLD2", HostID: "host-stuck", Command: []string{"sh"}, State: "running", CreatedAt: created}},
	}}
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	query := func(ctx context.Context, host HostRecord) ([]protocol.SessionInfo, error) {
		switch host.Alias {
		case "fast":
			return []protocol.SessionInfo{{ID: "LIVE", HostID: host.ID, Command: []string{"bash"}, State: "running", CreatedAt: created}}, nil
		case "offline":
			return nil, errors.New("connection refused")
		default:
			<-release // deliberately ignores ctx; the collector must still return
			return nil, ctx.Err()
		}
	}

	started := time.Now()
	got, err := CollectHostSessions(context.Background(), hosts, 40*time.Millisecond, query, cache)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("fan-out took %v, want bounded by deadline", elapsed)
	}
	if len(got) != 3 {
		t.Fatalf("host results = %d, want 3: %#v", len(got), got)
	}
	byAlias := make(map[string]HostSessions, len(got))
	for _, result := range got {
		byAlias[result.Host.Alias] = result
	}
	if byAlias["fast"].Stale || len(byAlias["fast"].Sessions) != 1 {
		t.Fatalf("fast result = %#v", byAlias["fast"])
	}
	if !byAlias["offline"].Stale || byAlias["offline"].Sessions[0].ID != "OLD1" {
		t.Fatalf("offline result = %#v", byAlias["offline"])
	}
	if !byAlias["stuck"].Stale || byAlias["stuck"].Sessions[0].ID != "OLD2" {
		t.Fatalf("stuck result = %#v", byAlias["stuck"])
	}
	if !slices.Equal(cache.saved, []string{"host-fast"}) {
		t.Fatalf("saved hosts = %v, want [host-fast]", cache.saved)
	}
}
