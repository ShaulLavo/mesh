package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// MCPServiceRow is one hosted MCP definition. The mcphost package owns the
// JSON record's schema.
type MCPServiceRow struct {
	ID        string
	Name      string
	Revision  int64
	Transport string
	Record    []byte
}

// MCPInstanceRow is one hosted MCP instance and its recorded intent.
type MCPInstanceRow struct {
	ID        string
	ServiceID string
	Record    []byte
}

// ErrMCPRevision reports a compare-and-swap whose expected revision is stale
// or a name already used by another service.
var ErrMCPRevision = errors.New("storage: MCP service changed")

func (s *Store) ListMCPServices(ctx context.Context) ([]MCPServiceRow, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, name, revision, transport, record FROM mcp_services ORDER BY name")
	if err != nil {
		return nil, fmt.Errorf("storage: list MCP services: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []MCPServiceRow
	for rows.Next() {
		var row MCPServiceRow
		if err := rows.Scan(&row.ID, &row.Name, &row.Revision, &row.Transport, &row.Record); err != nil {
			return nil, fmt.Errorf("storage: read MCP service: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: list MCP services: %w", err)
	}
	return out, nil
}

// PutMCPService inserts row when expected is zero, and otherwise replaces the
// service only while it still has the expected revision.
func (s *Store) PutMCPService(ctx context.Context, row MCPServiceRow, expected int64) error {
	var result sql.Result
	var err error
	if expected == 0 {
		result, err = s.db.ExecContext(ctx, "INSERT INTO mcp_services (id, name, revision, transport, record) VALUES (?, ?, ?, ?, ?) ON CONFLICT DO NOTHING",
			row.ID, row.Name, row.Revision, row.Transport, string(row.Record))
	} else {
		result, err = s.db.ExecContext(ctx, "UPDATE mcp_services SET name = ?, revision = ?, transport = ?, record = ? WHERE id = ? AND revision = ?",
			row.Name, row.Revision, row.Transport, string(row.Record), row.ID, expected)
	}
	if err != nil {
		return fmt.Errorf("storage: save MCP service %s: %w", row.Name, err)
	}
	return changedOne(result, row.Name)
}

func changedOne(result sql.Result, name string) error {
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("storage: save MCP service %s: %w", name, err)
	}
	if n != 1 {
		return fmt.Errorf("MCP service %s: %w", name, ErrMCPRevision)
	}
	return nil
}

// DeleteMCPService removes the service and all of its instance records.
func (s *Store) DeleteMCPService(ctx context.Context, id string, expected int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("storage: delete MCP service: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, "DELETE FROM mcp_services WHERE id = ? AND revision = ?", id, expected)
	if err != nil {
		return fmt.Errorf("storage: delete MCP service: %w", err)
	}
	if err := changedOne(result, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM mcp_instances WHERE service_id = ?", id); err != nil {
		return fmt.Errorf("storage: delete MCP instances: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: delete MCP service: %w", err)
	}
	return nil
}

func (s *Store) ListMCPInstances(ctx context.Context) ([]MCPInstanceRow, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, service_id, record FROM mcp_instances ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("storage: list MCP instances: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []MCPInstanceRow
	for rows.Next() {
		var row MCPInstanceRow
		if err := rows.Scan(&row.ID, &row.ServiceID, &row.Record); err != nil {
			return nil, fmt.Errorf("storage: read MCP instance: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: list MCP instances: %w", err)
	}
	return out, nil
}

// PutMCPInstance records an instance's intent before the supervisor touches
// its process. The service must still exist.
func (s *Store) PutMCPInstance(ctx context.Context, row MCPInstanceRow) error {
	result, err := s.db.ExecContext(ctx, `INSERT INTO mcp_instances (id, service_id, record)
        SELECT ?, ?, ? WHERE EXISTS (SELECT 1 FROM mcp_services WHERE id = ?)
        ON CONFLICT(id) DO UPDATE SET record = excluded.record`, row.ID, row.ServiceID, string(row.Record), row.ServiceID)
	if err != nil {
		return fmt.Errorf("storage: save MCP instance %s: %w", row.ID, err)
	}
	return changedOne(result, row.ServiceID)
}

func (s *Store) DeleteMCPInstance(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM mcp_instances WHERE id = ?", id); err != nil {
		return fmt.Errorf("storage: delete MCP instance %s: %w", id, err)
	}
	return nil
}
