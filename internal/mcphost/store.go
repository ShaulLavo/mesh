package mcphost

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/shaul/mesh/internal/storage"
)

// Store persists definitions and instance intent. *storage.Store implements it.
type Store interface {
	ListMCPServices(context.Context) ([]storage.MCPServiceRow, error)
	PutMCPService(ctx context.Context, row storage.MCPServiceRow, expected int64) error
	DeleteMCPService(ctx context.Context, id string, expected int64) error
	ListMCPInstances(context.Context) ([]storage.MCPInstanceRow, error)
	PutMCPInstance(context.Context, storage.MCPInstanceRow) error
	DeleteMCPInstance(ctx context.Context, id string) error
}

// Instance states reported to callers.
const (
	StateStopped      = "stopped"
	StateStarting     = "starting"
	StateReady        = "ready"
	StateFailed       = "failed"
	StateIncompatible = "incompatible"
	// StateUncertain means recovery could not tell whether the previous worker
	// still exists. No second worker launches until that is settled.
	StateUncertain = "uncertain"
	// StateInterrupted means the host rebooted and the process is gone.
	StateInterrupted = "interrupted"
)

const (
	intentRun  = "run"
	intentStop = "stop"
)

// InstanceRecord is the persisted part of one application instance.
type InstanceRecord struct {
	ID        string `json:"id"`
	ServiceID string `json:"serviceId"`
	// Owner is the caller key of an isolated instance and empty when shared.
	Owner string `json:"owner,omitempty"`
	// OwnerAccount lets revocation find the instances of a retired account.
	OwnerAccount string  `json:"ownerAccount,omitempty"`
	Default      bool    `json:"default,omitempty"`
	Revision     int64   `json:"revision"`
	Generation   int64   `json:"generation"`
	Intent       string  `json:"intent"`
	State        string  `json:"state"`
	Reason       string  `json:"reason,omitempty"`
	ExitCode     *int    `json:"exitCode,omitempty"`
	Leases       []Lease `json:"leases,omitempty"`
	// Worker is the exact reservation of the current generation.
	Worker    *WorkerRef `json:"worker,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
}

// Lease holds an instance for a bounded time, optionally until a co-located
// native job ends.
type Lease struct {
	ID      string    `json:"id"`
	Caller  string    `json:"caller"`
	Account string    `json:"account"`
	Grant   string    `json:"grant"`
	Job     string    `json:"job,omitempty"`
	Expires time.Time `json:"expires"`
}

func serviceRow(d Definition) (storage.MCPServiceRow, error) {
	record, err := json.Marshal(d)
	if err != nil {
		return storage.MCPServiceRow{}, fmt.Errorf("encode service %s: %w", d.Name, err)
	}
	return storage.MCPServiceRow{ID: d.ID, Name: d.Name, Revision: d.Revision, Transport: d.Transport, Record: record}, nil
}

func instanceRow(r InstanceRecord) (storage.MCPInstanceRow, error) {
	record, err := json.Marshal(r)
	if err != nil {
		return storage.MCPInstanceRow{}, fmt.Errorf("encode instance %s: %w", r.ID, err)
	}
	return storage.MCPInstanceRow{ID: r.ID, ServiceID: r.ServiceID, Record: record}, nil
}

func loadRecords(ctx context.Context, store Store) ([]Definition, []InstanceRecord, error) {
	rows, err := store.ListMCPServices(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("load MCP services: %w", err)
	}
	defs := make([]Definition, 0, len(rows))
	for _, row := range rows {
		var d Definition
		if err := json.Unmarshal(row.Record, &d); err != nil {
			return nil, nil, fmt.Errorf("decode MCP service %s: %w", row.Name, err)
		}
		defs = append(defs, d)
	}
	instances, err := store.ListMCPInstances(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("load MCP instances: %w", err)
	}
	records := make([]InstanceRecord, 0, len(instances))
	for _, row := range instances {
		var r InstanceRecord
		if err := json.Unmarshal(row.Record, &r); err != nil {
			return nil, nil, fmt.Errorf("decode MCP instance %s: %w", row.ID, err)
		}
		records = append(records, r)
	}
	return defs, records, nil
}
