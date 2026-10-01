package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
)

// HostSessions is one host's live catalog or its last cached catalog.
type HostSessions struct {
	Local    bool
	Host     HostRecord
	Sessions []protocol.SessionInfo
	Stale    bool
	Err      error
	CacheErr error
}

// HostQuery fetches one authoritative host catalog.
type HostQuery func(context.Context, HostRecord) ([]protocol.SessionInfo, error)

// CatalogCache stores the last authoritative catalog for offline display.
// Implementations must honor context cancellation and permit concurrent calls.
type CatalogCache interface {
	Load(context.Context, HostRecord) ([]protocol.SessionInfo, error)
	Save(context.Context, HostRecord, []protocol.SessionInfo) error
}

const (
	catalogCacheWriteTimeout = 200 * time.Millisecond
	catalogCacheReadTimeout  = 200 * time.Millisecond
)

type hostQueryResult struct {
	index    int
	sessions []protocol.SessionInfo
	err      error
}

// CollectHostSessions queries all hosts concurrently under a shared deadline,
// even if a broken query implementation ignores context cancellation. Cache
// writes fit within that deadline; stale fallback reads get a separate bounded
// budget so the network deadline cannot prevent offline display.
func CollectHostSessions(parent context.Context, hosts []HostRecord, timeout time.Duration, query HostQuery, cache CatalogCache) ([]HostSessions, error) {
	if parent == nil {
		return nil, errors.New("collect host sessions with nil context")
	}
	if timeout <= 0 {
		return nil, errors.New("collect host sessions with non-positive timeout")
	}
	if query == nil || cache == nil {
		return nil, errors.New("collect host sessions with incomplete dependencies")
	}
	if err := parent.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	results := make(chan hostQueryResult, len(hosts))
	for i, host := range hosts {
		i, host := i, host
		go func() {
			sessions, err := query(ctx, host)
			results <- hostQueryResult{index: i, sessions: sessions, err: err}
		}()
	}

	out := make([]HostSessions, len(hosts))
	pending := make(map[int]struct{}, len(hosts))
	for i, host := range hosts {
		out[i].Host = host
		pending[i] = struct{}{}
	}
	var cacheWG sync.WaitGroup
	loadCached := func(index int, queryErr error) {
		out[index].Stale = true
		out[index].Err = queryErr
		cacheWG.Add(1)
		go func() {
			defer cacheWG.Done()
			cacheCtx, cacheCancel := context.WithTimeout(parent, catalogCacheReadTimeout)
			defer cacheCancel()
			rows, err := cache.Load(cacheCtx, hosts[index])
			if err != nil {
				out[index].Err = errors.Join(queryErr, fmt.Errorf("load cache host %s: %w", hosts[index].Alias, err))
			} else {
				out[index].Sessions = cloneSessionInfo(rows)
			}
		}()
	}
	for len(pending) > 0 {
		select {
		case result := <-results:
			if _, waiting := pending[result.index]; !waiting {
				continue
			}
			delete(pending, result.index)
			if result.err == nil {
				out[result.index].Sessions = cloneSessionInfo(result.sessions)
				cacheWG.Add(1)
				go func(index int, rows []protocol.SessionInfo) {
					defer cacheWG.Done()
					cacheCtx, cacheCancel := context.WithTimeout(ctx, catalogCacheWriteTimeout)
					defer cacheCancel()
					if err := cache.Save(cacheCtx, hosts[index], rows); err != nil {
						out[index].CacheErr = fmt.Errorf("cache host %s: %w", hosts[index].Alias, err)
					}
				}(result.index, cloneSessionInfo(result.sessions))
				continue
			}
			loadCached(result.index, result.err)
		case <-ctx.Done():
			for index := range pending {
				loadCached(index, ctx.Err())
			}
			pending = nil
		}
	}
	cacheWG.Wait()
	cancel()
	if err := parent.Err(); err != nil {
		return nil, fmt.Errorf("collect host sessions: %w", err)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Host.Alias < out[j].Host.Alias })
	return out, nil
}

func cloneSessionInfo(rows []protocol.SessionInfo) []protocol.SessionInfo {
	cloned := make([]protocol.SessionInfo, len(rows))
	for i, row := range rows {
		row.Command = append([]string(nil), row.Command...)
		row.LastAttachedAt = cloneTime(row.LastAttachedAt)
		row.DetachedAt = cloneTime(row.DetachedAt)
		row.ExitCode = cloneInt(row.ExitCode)
		row.Recovery = cloneRecoveryRecord(row.Recovery)
		if row.Hibernated != nil {
			h := *row.Hibernated
			row.Hibernated = &h
		}
		cloned[i] = row
	}
	return cloned
}

func cloneRecoveryRecord(record *recovery.Record) *recovery.Record {
	if record == nil {
		return nil
	}
	data, _ := json.Marshal(record)
	var cloned recovery.Record
	_ = json.Unmarshal(data, &cloned)
	return &cloned
}
