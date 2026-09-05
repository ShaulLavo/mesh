package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	meshserve "github.com/shaul/mesh/internal/serve"
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
	observed := make([]storage.Session, len(rows))
	for i, row := range rows {
		if row.HostID != host.ID {
			return fmt.Errorf("host %s listed a session for a different host", host.Alias)
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
	return c.store.ReconcileHost(ctx, cachedHost(host, c.now().UTC()), observed)
}

// LoadAllServices returns the bounded cached service snapshot keyed by host ID.
func (c *SQLiteCatalogCache) LoadAllServices(ctx context.Context) (map[string][]storage.CachedService, error) {
	rows, err := c.store.ListAllCachedServices(ctx)
	if err != nil {
		return nil, err
	}
	result := make(map[string][]storage.CachedService)
	for _, row := range rows {
		key := string(row.HostID)
		result[key] = append(result[key], row)
	}
	return result, nil
}

// LoadServices returns only the selected host's cached service snapshot. The
// picker uses this path on its hot refresh loop instead of scanning all hosts.
func (c *SQLiteCatalogCache) LoadServices(ctx context.Context, host HostRecord) ([]storage.CachedService, error) {
	return c.store.ListCachedServices(ctx, storage.HostID(host.ID))
}

// SaveServices atomically replaces the cached list after a live service.list.
func (c *SQLiteCatalogCache) SaveServices(ctx context.Context, host HostRecord, privateName string, rows []protocol.ServiceInfo) error {
	now := c.now().UTC()
	services := make([]storage.CachedService, len(rows))
	for index, row := range rows {
		validated, err := validateRemoteService(row)
		if err != nil {
			return fmt.Errorf("cache host %s service: %w", host.Alias, err)
		}
		services[index] = storage.CachedService{
			HostID: storage.HostID(host.ID), PrivateName: privateName,
			Service: meshserve.Service{
				Name: validated.Name, Kind: meshserve.Kind(validated.Kind), Target: validated.Target,
				PublicName: validated.PublicName, WakeOnRequest: validated.WakeOnRequest, Isolate: validated.Isolate,
			},
			Healthy: validated.Healthy, Problem: validated.Problem, ObservedAt: now,
		}
	}
	return c.store.ReplaceCachedServices(ctx, cachedHost(host, now), services)
}

func cachedHost(host HostRecord, observedAt time.Time) storage.Host {
	alias := host.Alias
	var tailscaleName *string
	if host.TailscaleName != "" {
		name := host.TailscaleName
		tailscaleName = &name
	}
	return storage.Host{
		ID: storage.HostID(host.ID), Alias: &alias, MeshIdentity: host.MeshIdentity,
		TailscaleName: tailscaleName, LastSeenAt: observedAt,
	}
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
