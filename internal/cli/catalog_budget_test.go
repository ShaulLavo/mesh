package cli

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
)

type catalogCacheFuncs struct {
	load func(context.Context, HostRecord) ([]protocol.SessionInfo, error)
	save func(context.Context, HostRecord, []protocol.SessionInfo) error
}

func (c catalogCacheFuncs) Load(ctx context.Context, host HostRecord) ([]protocol.SessionInfo, error) {
	return c.load(ctx, host)
}

func (c catalogCacheFuncs) Save(ctx context.Context, host HostRecord, rows []protocol.SessionInfo) error {
	return c.save(ctx, host, rows)
}

type catalogCollection struct {
	rows []HostSessions
	err  error
}

func awaitCatalogCollection(t *testing.T, done <-chan catalogCollection) catalogCollection {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(750 * time.Millisecond):
		t.Fatal("catalog collection exceeded its query and cache budgets")
		return catalogCollection{}
	}
}

func TestCatalogDeadlineBoundsCacheSave(t *testing.T) {
	for _, timeout := range []time.Duration{40 * time.Millisecond, 2 * time.Second} {
		t.Run(timeout.String(), func(t *testing.T) {
			release := make(chan struct{})
			finished := make(chan struct{})
			done := make(chan catalogCollection, 1)
			t.Cleanup(func() { close(release); <-finished })
			cache := catalogCacheFuncs{save: func(ctx context.Context, _ HostRecord, _ []protocol.SessionInfo) error {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-release:
					return errors.New("test released blocked save")
				}
			}}
			go func() {
				defer close(finished)
				rows, err := CollectHostSessions(context.Background(), []HostRecord{{Alias: "pc", ID: "pc"}}, HostQueryBudget{Reply: timeout},
					func(context.Context, HostRecord, HostQueryBudget) ([]protocol.SessionInfo, error) {
						return []protocol.SessionInfo{{ID: "LIVE"}}, nil
					}, cache)
				done <- catalogCollection{rows: rows, err: err}
			}()
			result := awaitCatalogCollection(t, done)
			if result.err != nil || len(result.rows) != 1 {
				t.Fatalf("collection = %#v", result)
			}
			row := result.rows[0]
			if row.Stale || row.Err != nil || len(row.Sessions) != 1 || row.Sessions[0].ID != "LIVE" || !errors.Is(row.CacheErr, context.DeadlineExceeded) {
				t.Fatalf("live row lost authority or cache warning: %#v", row)
			}
		})
	}
}

func TestCatalogDeadlineBoundsCacheLoad(t *testing.T) {
	for _, waitForDeadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "query failure", true: "query deadline"}[waitForDeadline], func(t *testing.T) {
			release := make(chan struct{})
			finished := make(chan struct{})
			done := make(chan catalogCollection, 1)
			t.Cleanup(func() { close(release); <-finished })
			cache := catalogCacheFuncs{load: func(ctx context.Context, _ HostRecord) ([]protocol.SessionInfo, error) {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-release:
					return nil, errors.New("test released blocked load")
				}
			}}
			hosts := []HostRecord{
				{Alias: "a", ID: "a"}, {Alias: "b", ID: "b"}, {Alias: "c", ID: "c"},
				{Alias: "d", ID: "d"}, {Alias: "e", ID: "e"}, {Alias: "f", ID: "f"},
			}
			go func() {
				defer close(finished)
				rows, err := CollectHostSessions(context.Background(), hosts, HostQueryBudget{Reply: 40 * time.Millisecond},
					func(ctx context.Context, _ HostRecord, _ HostQueryBudget) ([]protocol.SessionInfo, error) {
						if waitForDeadline {
							<-ctx.Done()
							return nil, ctx.Err()
						}
						return nil, errors.New("offline")
					}, cache)
				done <- catalogCollection{rows: rows, err: err}
			}()
			result := awaitCatalogCollection(t, done)
			if result.err != nil || len(result.rows) != len(hosts) {
				t.Fatalf("collection = %#v", result)
			}
			for _, row := range result.rows {
				if !row.Stale || !errors.Is(row.Err, context.DeadlineExceeded) {
					t.Fatalf("stale cache diagnostic = %#v", row)
				}
			}
		})
	}
}

func TestCatalogParentCancellationJoinsCacheWork(t *testing.T) {
	for _, write := range []bool{false, true} {
		t.Run(map[bool]string{false: "load", true: "save"}[write], func(t *testing.T) {
			parent, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			started := make(chan struct{})
			completed := make(chan struct{})
			blocked := func(ctx context.Context) error {
				close(started)
				<-ctx.Done()
				close(completed)
				return ctx.Err()
			}
			cache := catalogCacheFuncs{
				load: func(ctx context.Context, _ HostRecord) ([]protocol.SessionInfo, error) { return nil, blocked(ctx) },
				save: func(ctx context.Context, _ HostRecord, _ []protocol.SessionInfo) error { return blocked(ctx) },
			}
			done := make(chan catalogCollection, 1)
			go func() {
				rows, err := CollectHostSessions(parent, []HostRecord{{Alias: "pc", ID: "pc"}}, HostQueryBudget{Reply: 2 * time.Second},
					func(context.Context, HostRecord, HostQueryBudget) ([]protocol.SessionInfo, error) {
						if write {
							return nil, nil
						}
						return nil, errors.New("offline")
					}, cache)
				done <- catalogCollection{rows: rows, err: err}
			}()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("cache operation did not start")
			}
			cancel()
			result := awaitCatalogCollection(t, done)
			if !errors.Is(result.err, context.Canceled) {
				t.Fatalf("collection error = %v, want parent cancellation", result.err)
			}
			select {
			case <-completed:
			default:
				t.Fatal("collector returned before joining cache work")
			}
		})
	}
}

func TestCatalogSuccessfulEmptyQueryDoesNotLoadStaleRows(t *testing.T) {
	cacheErr := errors.New("cache write failed")
	var saveCtx context.Context
	cache := catalogCacheFuncs{
		load: func(context.Context, HostRecord) ([]protocol.SessionInfo, error) {
			t.Error("successful empty catalog must not load stale rows")
			return []protocol.SessionInfo{{ID: "OLD1"}}, nil
		},
		save: func(ctx context.Context, _ HostRecord, rows []protocol.SessionInfo) error {
			saveCtx = ctx
			if len(rows) != 0 {
				t.Errorf("cached rows = %#v, want empty authoritative catalog", rows)
			}
			return cacheErr
		},
	}
	rows, err := CollectHostSessions(context.Background(), []HostRecord{{Alias: "pc", ID: "pc"}}, HostQueryBudget{Reply: time.Second},
		func(context.Context, HostRecord, HostQueryBudget) ([]protocol.SessionInfo, error) { return nil, nil }, cache)
	if err != nil || len(rows) != 1 || rows[0].Stale || rows[0].Err != nil || len(rows[0].Sessions) != 0 || !errors.Is(rows[0].CacheErr, cacheErr) {
		t.Fatalf("empty authoritative catalog = %#v, %v", rows, err)
	}
	if saveCtx == nil || !errors.Is(saveCtx.Err(), context.Canceled) {
		t.Fatal("cache write context was not canceled before return")
	}
}

func TestCatalogDeadlineAllowsFreshCacheReadBudget(t *testing.T) {
	cache := catalogCacheFuncs{load: func(ctx context.Context, _ HostRecord) ([]protocol.SessionInfo, error) {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("load cached rows: %w", err)
		}
		if _, bounded := ctx.Deadline(); !bounded {
			return nil, errors.New("cache load has no deadline")
		}
		return []protocol.SessionInfo{{ID: "OLD1"}}, nil
	}}
	rows, err := CollectHostSessions(context.Background(), []HostRecord{{Alias: "pc", ID: "pc"}}, HostQueryBudget{Reply: 20 * time.Millisecond},
		func(ctx context.Context, _ HostRecord, _ HostQueryBudget) ([]protocol.SessionInfo, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}, cache)
	if err != nil || len(rows) != 1 || !rows[0].Stale || len(rows[0].Sessions) != 1 || rows[0].Sessions[0].ID != "OLD1" || !errors.Is(rows[0].Err, context.DeadlineExceeded) {
		t.Fatalf("fallback after query deadline = %#v, %v", rows, err)
	}
}

func TestCatalogPhaseBudgetsBoundUncooperativeQuery(t *testing.T) {
	release := make(chan struct{})
	finished := make(chan struct{})
	t.Cleanup(func() { close(release); <-finished })
	budget := HostQueryBudget{Setup: 20 * time.Millisecond, Reply: 40 * time.Millisecond}
	cache := catalogCacheFuncs{load: func(context.Context, HostRecord) ([]protocol.SessionInfo, error) {
		return []protocol.SessionInfo{{ID: "OLD1"}}, nil
	}}
	done := make(chan catalogCollection, 1)
	go func() {
		rows, err := CollectHostSessions(t.Context(), []HostRecord{{Alias: "fixture", ID: "fixture"}}, budget,
			func(ctx context.Context, _ HostRecord, actual HostQueryBudget) ([]protocol.SessionInfo, error) {
				defer close(finished)
				if actual != budget {
					t.Error("collector changed phase budgets")
				}
				deadline, bounded := ctx.Deadline()
				if !bounded || time.Until(deadline) > budget.Setup+budget.Reply {
					t.Error("collector lost total query cap")
				}
				<-release
				return nil, ctx.Err()
			}, cache)
		done <- catalogCollection{rows: rows, err: err}
	}()
	result := awaitCatalogCollection(t, done)
	if result.err != nil || len(result.rows) != 1 || !result.rows[0].Stale || !errors.Is(result.rows[0].Err, context.DeadlineExceeded) || len(result.rows[0].Sessions) != 1 {
		t.Fatalf("uncooperative query lost bounded stale fallback: %#v", result)
	}
}
