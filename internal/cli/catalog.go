package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/protocol"
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
type CatalogCache interface {
	Load(context.Context, HostRecord) ([]protocol.SessionInfo, error)
	Save(context.Context, HostRecord, []protocol.SessionInfo) error
}

type hostQueryResult struct {
	index    int
	sessions []protocol.SessionInfo
	err      error
}

// CollectHostSessions queries all hosts concurrently. It returns cached rows
// for failures and stops waiting when the shared deadline expires, even if a
// broken query implementation ignores context cancellation.
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
	var saveWG sync.WaitGroup
	for len(pending) > 0 {
		select {
		case result := <-results:
			if _, waiting := pending[result.index]; !waiting {
				continue
			}
			delete(pending, result.index)
			if result.err == nil {
				out[result.index].Sessions = cloneSessionInfo(result.sessions)
				saveWG.Add(1)
				go func(index int, rows []protocol.SessionInfo) {
					defer saveWG.Done()
					if err := cache.Save(parent, hosts[index], rows); err != nil {
						out[index].CacheErr = fmt.Errorf("cache host %s: %w", hosts[index].Alias, err)
					}
				}(result.index, cloneSessionInfo(result.sessions))
				continue
			}
			out[result.index].Stale = true
			out[result.index].Err = result.err
			rows, err := cache.Load(parent, hosts[result.index])
			if err != nil {
				out[result.index].Err = errors.Join(result.err, fmt.Errorf("load cache: %w", err))
			} else {
				out[result.index].Sessions = cloneSessionInfo(rows)
			}
		case <-ctx.Done():
			for index := range pending {
				out[index].Stale = true
				out[index].Err = ctx.Err()
				rows, err := cache.Load(parent, hosts[index])
				if err != nil {
					out[index].Err = errors.Join(ctx.Err(), fmt.Errorf("load cache: %w", err))
				} else {
					out[index].Sessions = cloneSessionInfo(rows)
				}
			}
			pending = nil
		}
	}
	saveWG.Wait()
	sort.SliceStable(out, func(i, j int) bool { return out[i].Host.Alias < out[j].Host.Alias })
	return out, nil
}

func cloneSessionInfo(rows []protocol.SessionInfo) []protocol.SessionInfo {
	cloned := make([]protocol.SessionInfo, len(rows))
	for i, row := range rows {
		row.Command = append([]string(nil), row.Command...)
		cloned[i] = row
	}
	return cloned
}
