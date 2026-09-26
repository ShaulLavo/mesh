package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	meshserve "github.com/shaul/mesh/internal/serve"
	dbsqlc "github.com/shaul/mesh/internal/storage/sqlc"
)

// UpsertService records the complete definition of one origin service.
func (s *Store) UpsertService(ctx context.Context, service meshserve.Service) (meshserve.Service, error) {
	normalized, err := meshserve.Normalize(service)
	if err != nil {
		return meshserve.Service{}, err
	}
	listens, demand, err := encodeDemand(normalized)
	if err != nil {
		return meshserve.Service{}, fmt.Errorf("storage: upsert service %s: %w", normalized.Name, err)
	}
	row, err := s.queries.UpsertService(ctx, dbsqlc.UpsertServiceParams{
		Name:          normalized.Name,
		Kind:          string(normalized.Kind),
		Target:        normalized.Target,
		PublicName:    normalized.PublicName,
		WakeOnRequest: boolInt64(normalized.WakeOnRequest),
		Isolate:       boolInt64(normalized.Isolate),
		Listens:       listens,
		Demand:        demand,
		LocalOnly:     boolInt64(normalized.LocalOnly),
	})
	if err != nil {
		return meshserve.Service{}, fmt.Errorf("storage: upsert service %s: %w", normalized.Name, err)
	}
	return serviceFromRow(row)
}

// GetService returns one service by its route name.
func (s *Store) GetService(ctx context.Context, name string) (meshserve.Service, error) {
	if err := meshserve.ValidateName(name); err != nil {
		return meshserve.Service{}, err
	}
	row, err := s.queries.GetService(ctx, name)
	if err != nil {
		return meshserve.Service{}, fmt.Errorf("storage: get service %s: %w", name, err)
	}
	return serviceFromRow(row)
}

// ListServices returns every service in route-name order.
func (s *Store) ListServices(ctx context.Context) ([]meshserve.Service, error) {
	rows, err := s.queries.ListServices(ctx)
	if err != nil {
		return nil, fmt.Errorf("storage: list services: %w", err)
	}
	services := make([]meshserve.Service, 0, len(rows))
	for _, row := range rows {
		service, err := serviceFromRow(row)
		if err != nil {
			return nil, fmt.Errorf("storage: list services: %w", err)
		}
		services = append(services, service)
	}
	return services, nil
}

// DeleteService removes one service by its route name.
func (s *Store) DeleteService(ctx context.Context, name string) error {
	if err := meshserve.ValidateName(name); err != nil {
		return err
	}
	deleted, err := s.queries.DeleteService(ctx, name)
	if err != nil {
		return fmt.Errorf("storage: delete service %s: %w", name, err)
	}
	if deleted == 0 {
		return fmt.Errorf("storage: delete service %s: %w", name, sql.ErrNoRows)
	}
	return nil
}

func serviceFromRow(row dbsqlc.Service) (meshserve.Service, error) {
	wakeOnRequest, err := sqliteBool("wake_on_request", row.WakeOnRequest)
	if err != nil {
		return meshserve.Service{}, fmt.Errorf("storage: decode service %s: %w", row.Name, err)
	}
	isolate, err := sqliteBool("isolate", row.Isolate)
	if err != nil {
		return meshserve.Service{}, fmt.Errorf("storage: decode service %s: %w", row.Name, err)
	}
	localOnly, err := sqliteBool("local_only", row.LocalOnly)
	if err != nil {
		return meshserve.Service{}, fmt.Errorf("storage: decode service %s: %w", row.Name, err)
	}
	service := meshserve.Service{
		Name:          row.Name,
		Kind:          meshserve.Kind(row.Kind),
		Target:        row.Target,
		PublicName:    row.PublicName,
		WakeOnRequest: wakeOnRequest,
		Isolate:       isolate,
		LocalOnly:     localOnly,
	}
	if err := decodeDemand(row.Listens, row.Demand, &service); err != nil {
		return meshserve.Service{}, fmt.Errorf("storage: decode service %s: %w", row.Name, err)
	}
	service, err = meshserve.Normalize(service)
	if err != nil {
		return meshserve.Service{}, fmt.Errorf("storage: decode service %s: %w", row.Name, err)
	}
	return service, nil
}

func boolInt64(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

func sqliteBool(name string, value int64) (bool, error) {
	switch value {
	case 0:
		return false, nil
	case 1:
		return true, nil
	default:
		return false, fmt.Errorf("%s is %d, want 0 or 1", name, value)
	}
}

// storedDemand is the JSON column form of a launch recipe. Durations are
// milliseconds so the column reads plainly in sqlite3.
type storedDemand struct {
	Command            string   `json:"command"`
	Cwd                string   `json:"cwd"`
	Env                []string `json:"env,omitempty"`
	IdleMillis         int64    `json:"idleMs"`
	ReadyTimeoutMillis int64    `json:"readyTimeoutMs"`
}

type storedListen struct {
	Public   uint16 `json:"public"`
	Upstream uint16 `json:"upstream"`
}

// encodeDemand stores an ordinary route as two empty strings, so every row
// written before on-demand routes existed already reads as one.
func encodeDemand(service meshserve.Service) (string, string, error) {
	listens, demand := "", ""
	if len(service.Listens) > 0 {
		stored := make([]storedListen, len(service.Listens))
		for index, listen := range service.Listens {
			stored[index] = storedListen(listen)
		}
		encoded, err := json.Marshal(stored)
		if err != nil {
			return "", "", fmt.Errorf("encode listeners: %w", err)
		}
		listens = string(encoded)
	}
	if service.Demand != nil {
		encoded, err := json.Marshal(storedDemand{
			Command: service.Demand.Command, Cwd: service.Demand.Cwd, Env: service.Demand.Env,
			IdleMillis:         service.Demand.Idle.Milliseconds(),
			ReadyTimeoutMillis: service.Demand.ReadyTimeout.Milliseconds(),
		})
		if err != nil {
			return "", "", fmt.Errorf("encode launch recipe: %w", err)
		}
		demand = string(encoded)
	}
	return listens, demand, nil
}

func decodeDemand(listens, demand string, service *meshserve.Service) error {
	if listens != "" {
		var stored []storedListen
		if err := json.Unmarshal([]byte(listens), &stored); err != nil {
			return fmt.Errorf("listeners: %w", err)
		}
		for _, listen := range stored {
			service.Listens = append(service.Listens, meshserve.Listen(listen))
		}
	}
	if demand != "" {
		var stored storedDemand
		if err := json.Unmarshal([]byte(demand), &stored); err != nil {
			return fmt.Errorf("launch recipe: %w", err)
		}
		service.Demand = &meshserve.Demand{
			Command: stored.Command, Cwd: stored.Cwd, Env: stored.Env,
			Idle:         time.Duration(stored.IdleMillis) * time.Millisecond,
			ReadyTimeout: time.Duration(stored.ReadyTimeoutMillis) * time.Millisecond,
		}
	}
	return nil
}
