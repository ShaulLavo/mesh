package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	meshserve "github.com/shaul/mesh/internal/serve"
	"github.com/shaul/mesh/internal/session"
	dbsqlc "github.com/shaul/mesh/internal/storage/sqlc"
)

type ServicePhase string

const (
	ServiceNone      ServicePhase = "none"
	ServiceStarting  ServicePhase = "starting"
	ServiceReadiness ServicePhase = "readiness"
	ServiceReady     ServicePhase = "ready"
	ServiceCleanup   ServicePhase = "cleanup"
)

// ServiceAttempt retains the exact launched recipe even after its definition is removed.
type ServiceAttempt struct {
	Reference session.Reservation
	Launched  meshserve.Service
	StartedAt time.Time
	Deadline  time.Time
	ReadyAt   time.Time
	StopToken string
	StopCause string
}

type ServiceRuntime struct {
	Name       string
	Generation string
	Version    int64
	Phase      ServicePhase
	Attempt    *ServiceAttempt
	Definition *meshserve.Service
	Failures   int
	Restarts   int
	RestartAt  time.Time
}

func (r ServiceRuntime) Equal(other ServiceRuntime) bool {
	if r.Name != other.Name || r.Generation != other.Generation || r.Version != other.Version || r.Phase != other.Phase || r.Failures != other.Failures || r.Restarts != other.Restarts || !r.RestartAt.Equal(other.RestartAt) {
		return false
	}
	if r.Definition == nil || other.Definition == nil {
		if r.Definition != other.Definition {
			return false
		}
	} else if !r.Definition.Equal(*other.Definition) {
		return false
	}
	if r.Attempt == nil || other.Attempt == nil {
		return r.Attempt == other.Attempt
	}
	a, b := r.Attempt, other.Attempt
	return a.Reference == b.Reference && a.Launched.Equal(b.Launched) && a.StartedAt.Equal(b.StartedAt) && a.Deadline.Equal(b.Deadline) && a.ReadyAt.Equal(b.ReadyAt) && a.StopToken == b.StopToken && a.StopCause == b.StopCause
}

var ErrServiceRuntimeConflict = errors.New("service runtime changed")

func (s *Store) ListServiceRuntime(ctx context.Context) ([]ServiceRuntime, error) {
	rows, err := s.queries.ListServiceRuntime(ctx)
	if err != nil {
		return nil, fmt.Errorf("storage: list service runtime: %w", err)
	}
	records := make([]ServiceRuntime, 0, len(rows))
	for _, row := range rows {
		record, err := decodeServiceRuntime(row)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, nil
}

func (s *Store) GetServiceRuntime(ctx context.Context, name string) (ServiceRuntime, error) {
	row, err := s.queries.GetServiceRuntime(ctx, name)
	if err != nil {
		return ServiceRuntime{}, fmt.Errorf("storage: get service runtime %s: %w", name, err)
	}
	return decodeServiceRuntime(row)
}

// CompareServiceRuntime also confirms a previously uncertain transaction. Every
// successful retry crosses a FULL synchronous write barrier, including deletion.
func (s *Store) CompareServiceRuntime(ctx context.Context, before, next *ServiceRuntime) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("storage: runtime connection: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "PRAGMA synchronous = FULL"); err != nil {
		return fmt.Errorf("storage: runtime durability: %w", err)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("storage: runtime transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	q := dbsqlc.New(tx)
	// Acquire the writer before reading: a deferred read-to-write upgrade can
	// return SQLITE_BUSY without waiting for another catalog writer.
	if err := q.ConfirmServiceRuntime(ctx); err != nil {
		return fmt.Errorf("storage: confirm runtime: %w", err)
	}
	if err := compareServiceRuntime(ctx, q, before, next); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: commit runtime: %w", err)
	}
	return nil
}

func compareServiceRuntime(ctx context.Context, q *dbsqlc.Queries, before, next *ServiceRuntime) error {
	name := ""
	if before != nil {
		name = before.Name
	}
	if next != nil {
		name = next.Name
	}
	current, err := q.GetServiceRuntime(ctx, name)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("storage: read runtime comparison: %w", err)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return compareAbsentRuntime(ctx, q, before, next)
	}
	record, err := decodeServiceRuntime(current)
	if err != nil {
		return err
	}
	if next != nil && record.Equal(*next) {
		return nil
	}
	if before == nil || !record.Equal(*before) {
		return ErrServiceRuntimeConflict
	}
	if next == nil {
		count, err := q.DeleteServiceRuntime(ctx, dbsqlc.DeleteServiceRuntimeParams{Name: before.Name, Generation: before.Generation, Version: before.Version})
		return runtimeRows(count, err)
	}
	if err := validateRuntimeSuccessor(before, next); err != nil {
		return err
	}
	encoded, err := json.Marshal(next)
	if err != nil {
		return fmt.Errorf("storage: encode runtime: %w", err)
	}
	count, err := q.UpdateServiceRuntime(ctx, dbsqlc.UpdateServiceRuntimeParams{Name: next.Name, Generation: next.Generation, NextVersion: next.Version, ExpectedVersion: before.Version, Record: string(encoded)})
	return runtimeRows(count, err)
}

func compareAbsentRuntime(ctx context.Context, q *dbsqlc.Queries, before, next *ServiceRuntime) error {
	if next == nil {
		return nil
	}
	if before != nil {
		return ErrServiceRuntimeConflict
	}
	if err := validateRuntimeSuccessor(nil, next); err != nil {
		return err
	}
	encoded, err := json.Marshal(next)
	if err != nil {
		return fmt.Errorf("storage: encode runtime: %w", err)
	}
	count, err := q.InsertServiceRuntime(ctx, dbsqlc.InsertServiceRuntimeParams{Name: next.Name, Generation: next.Generation, Version: next.Version, Record: string(encoded)})
	return runtimeRows(count, err)
}

func runtimeRows(count int64, err error) error {
	if err != nil {
		return fmt.Errorf("storage: compare service runtime: %w", err)
	}
	if count != 1 {
		return ErrServiceRuntimeConflict
	}
	return nil
}

func validateRuntimeSuccessor(before, next *ServiceRuntime) error {
	if err := validateServiceRuntime(*next); err != nil {
		return err
	}
	if before == nil && next.Version == 1 {
		return nil
	}
	if before != nil && before.Name == next.Name && before.Generation == next.Generation && before.Version+1 == next.Version {
		return nil
	}
	return ErrServiceRuntimeConflict
}

func decodeServiceRuntime(row dbsqlc.ServiceRuntime) (ServiceRuntime, error) {
	var record ServiceRuntime
	if err := json.Unmarshal([]byte(row.Record), &record); err != nil {
		return record, fmt.Errorf("storage: decode runtime %s: %w", row.Name, err)
	}
	if record.Name != row.Name || record.Generation != row.Generation || record.Version != row.Version {
		return record, fmt.Errorf("storage: runtime identity mismatch for %s", row.Name)
	}
	return record, validateServiceRuntime(record)
}

func validateServiceRuntime(record ServiceRuntime) error {
	if err := meshserve.ValidateName(record.Name); err != nil {
		return fmt.Errorf("storage: runtime name: %w", err)
	}
	if len(record.Generation) != 32 || record.Version < 1 || record.Failures < 0 || record.Failures > 10 || record.Restarts < 0 {
		return fmt.Errorf("storage: invalid runtime counters or generation")
	}
	if record.Phase == ServiceNone && record.Attempt == nil {
		return nil
	}
	if record.Attempt == nil {
		return fmt.Errorf("storage: runtime phase requires owned attempt")
	}
	return validateServiceAttempt(record)
}

func validateServiceAttempt(record ServiceRuntime) error {
	a := record.Attempt
	if err := a.Reference.Validate(); err != nil {
		return fmt.Errorf("storage: runtime reference: %w", err)
	}
	if a.Reference.BootID == "" || a.Reference.Purpose != session.Managed {
		return fmt.Errorf("storage: runtime requires a known managed reference")
	}
	normalized, err := meshserve.Normalize(a.Launched)
	if err != nil {
		return fmt.Errorf("storage: runtime recipe: %w", err)
	}
	if normalized.Name != record.Name || normalized.Demand == nil || !normalized.Equal(a.Launched) {
		return fmt.Errorf("storage: runtime recipe is not canonical")
	}
	switch record.Phase {
	case ServiceNone:
		return fmt.Errorf("storage: empty runtime retains an attempt")
	case ServiceStarting:
		return nil
	case ServiceReadiness:
		if !a.StartedAt.IsZero() && a.Deadline.After(a.StartedAt) {
			return nil
		}
	case ServiceReady:
		if !a.ReadyAt.IsZero() {
			return nil
		}
	case ServiceCleanup:
		if a.StopToken != "" && a.StopCause != "" {
			return nil
		}
	}
	return fmt.Errorf("storage: invalid service runtime phase %s", record.Phase)
}
