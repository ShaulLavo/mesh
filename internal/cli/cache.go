package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/storage"
)

const catalogDatabaseName = "mesh.db"

// SQLiteCatalogCache keeps the last catalog observed from each adopted host.
type SQLiteCatalogCache struct {
	store *storage.Store
	now   func() time.Time
}

// OpenCatalogCache opens the metadata database shared with the local daemon.
func OpenCatalogCache(ctx context.Context) (*SQLiteCatalogCache, error) {
	stateDir, err := paths.StateDir()
	if err != nil {
		return nil, err
	}
	store, err := storage.Open(ctx, filepath.Join(stateDir, catalogDatabaseName))
	if err != nil {
		return nil, err
	}
	return &SQLiteCatalogCache{store: store, now: time.Now}, nil
}

// Close closes the cache database.
func (c *SQLiteCatalogCache) Close() error { return c.store.Close() }

// Load returns the last catalog observed for host.
func (c *SQLiteCatalogCache) Load(ctx context.Context, host HostRecord) ([]protocol.SessionInfo, error) {
	rows, err := c.store.ListHostSessions(ctx, storage.HostID(host.ID))
	if err != nil {
		return nil, err
	}
	result := make([]protocol.SessionInfo, len(rows))
	for i, row := range rows {
		result[i] = protocol.SessionInfo{
			ID:                 string(row.ID),
			HostID:             string(row.HostID),
			Command:            append([]string(nil), row.Command...),
			Cwd:                row.Cwd,
			State:              string(row.State),
			CreatedAt:          row.CreatedAt,
			LastAttachedAt:     cloneTime(row.LastAttachedAt),
			ExitCode:           cloneInt(row.ExitCode),
			LastOutputSequence: row.LastOutputSequence,
		}
	}
	return result, nil
}

// Save replaces the cached active catalog after one authoritative query.
func (c *SQLiteCatalogCache) Save(ctx context.Context, host HostRecord, rows []protocol.SessionInfo) error {
	alias := host.Alias
	var tailscaleName *string
	if host.TailscaleName != "" {
		name := host.TailscaleName
		tailscaleName = &name
	}
	observed := make([]storage.Session, len(rows))
	for i, row := range rows {
		if row.HostID != host.ID {
			return fmt.Errorf("host %s listed session %s for host %s", host.Alias, row.ID, row.HostID)
		}
		observed[i] = storage.Session{
			ID:                 storage.SessionID(row.ID),
			HostID:             storage.HostID(row.HostID),
			Command:            append([]string(nil), row.Command...),
			Cwd:                row.Cwd,
			State:              storage.SessionState(row.State),
			CreatedAt:          row.CreatedAt,
			LastAttachedAt:     cloneTime(row.LastAttachedAt),
			ExitCode:           cloneInt(row.ExitCode),
			LastOutputSequence: row.LastOutputSequence,
		}
	}
	return c.store.ReconcileHost(ctx, storage.Host{
		ID:            storage.HostID(host.ID),
		Alias:         &alias,
		MeshIdentity:  host.MeshIdentity,
		TailscaleName: tailscaleName,
		LastSeenAt:    c.now().UTC(),
	}, observed)
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneInt(value *int) *int {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
