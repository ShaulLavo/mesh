package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/shaul/mesh/internal/storage"
)

func writeCache(ctx context.Context, path string) error {
	store, err := storage.Open(ctx, path)
	if err != nil {
		return fmt.Errorf("boundary: open candidate cache: %w", err)
	}
	defer func() { _ = store.Close() }()
	hosts, err := store.ListHosts(ctx)
	if err != nil {
		return fmt.Errorf("boundary: list candidate cache hosts: %w", err)
	}
	if len(hosts) != 1 {
		return errors.New("boundary: isolated cache must contain exactly one source host")
	}
	rows, err := store.ListHostSessions(ctx, hosts[0].ID)
	if err != nil {
		return fmt.Errorf("boundary: list candidate cache sessions: %w", err)
	}
	observed := make([]storage.ObservedSession, len(rows))
	for i, row := range rows {
		observed[i] = storage.ObservedSession{Session: row, Facts: &storage.SessionFacts{Label: "candidate-written cache facts"}}
	}
	if err := store.ReconcileHost(ctx, hosts[0], observed); err != nil {
		return fmt.Errorf("boundary: write candidate cache facts: %w", err)
	}
	return nil
}
