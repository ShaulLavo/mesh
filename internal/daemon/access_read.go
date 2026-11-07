package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/access"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/storage"
)

type accessServices interface {
	metadata(context.Context, access.Binding) ([]protocol.AccessService, error)
}
type accessReader struct {
	lifecycle *lifecycle
	services  accessServices
	slots     chan struct{}
}

func (r *accessReader) Read(ctx context.Context, binding access.Binding, read protocol.AccessRead) (protocol.AccessReadResult, error) {
	if err := binding.RequireMetadata(); err != nil {
		return protocol.AccessReadResult{}, fmt.Errorf("daemon: metadata admission: %w", err)
	}
	select {
	case r.slots <- struct{}{}:
	case <-ctx.Done():
		return protocol.AccessReadResult{}, errAccessUnavailable
	}
	defer func() { <-r.slots }()
	if err := ctx.Err(); err != nil {
		return protocol.AccessReadResult{}, errAccessUnavailable
	}
	if err := binding.RequireMetadata(); err != nil {
		return protocol.AccessReadResult{}, fmt.Errorf("daemon: metadata admission: %w", err)
	}
	principal := binding.Principal()
	result := protocol.AccessReadResult{
		Subject: protocol.AccessSubject{TargetID: principal.Target(), Principal: string(principal.Subject()), Class: "scoped", Permissions: principal.Permissions(), ExpiresAt: principal.ExpiresAt()},
		Section: read.Section, ObservedAt: time.Now().UTC(), Freshness: "live", Snapshot: "non-atomic", Sessions: []protocol.AccessSession{}, Services: []protocol.AccessService{},
	}
	after, err := accessPageAfter(read, principal)
	if err != nil {
		return result, err
	}
	switch read.Section {
	case "host":
		result.Host = r.host()
	case "sessions":
		result.Sessions, err = r.sessions(ctx, binding, read, after)
	case "services":
		result.Services, err = r.serviceRows(ctx, binding, after)
	default:
		return result, errAccessInvalid
	}
	if err != nil {
		return result, err
	}
	if err := binding.RequireMetadata(); err != nil {
		return protocol.AccessReadResult{}, fmt.Errorf("daemon: metadata admission: %w", err)
	}
	return boundAccessPage(result, read, principal)
}
func (r *accessReader) host() *protocol.AccessHost {
	name := ""
	if r.lifecycle.names != nil {
		name = r.lifecycle.names.Current().MachineName
	}
	return &protocol.AccessHost{ID: string(r.lifecycle.host.ID), MachineName: name, Version: release.Metadata().Version}
}
func accessPageAfter(read protocol.AccessRead, principal access.Principal) (string, error) {
	if read.Cursor == "" {
		return "", nil
	}
	cursor, err := protocol.DecodeAccessCursor(read.Cursor)
	if err != nil {
		return "", errAccessInvalid
	}
	if cursor.Target != principal.Target() || cursor.Subject != string(principal.Subject()) || cursor.Incarnation != principal.Incarnation() || cursor.Section != read.Section || cursor.SessionID != read.SessionID {
		return "", errAccessInvalid
	}
	return cursor.Last, nil
}
func (r *accessReader) sessions(ctx context.Context, binding access.Binding, read protocol.AccessRead, after string) ([]protocol.AccessSession, error) {
	if err := binding.RequireMetadata(); err != nil {
		return nil, fmt.Errorf("daemon: session metadata admission: %w", err)
	}
	rows, err := r.sessionRows(ctx, read.SessionID)
	if err != nil {
		return nil, err
	}
	result := make([]protocol.AccessSession, 0, len(rows))
	for _, row := range rows {
		if string(row.ID) <= after {
			continue
		}
		result = append(result, protocol.AccessSession{ID: string(row.ID), HostID: string(r.lifecycle.host.ID), State: string(row.State), CreatedAt: row.CreatedAt, LastAttachedAt: row.LastAttachedAt, ExitCode: row.ExitCode})
	}
	slices.SortFunc(result, func(a, b protocol.AccessSession) int { return strings.Compare(a.ID, b.ID) })
	return result, nil
}
func (r *accessReader) sessionRows(ctx context.Context, id string) ([]storage.Session, error) {
	if id == "" {
		rows, err := r.lifecycle.catalog.List(ctx)
		if err != nil {
			return nil, errAccessUnavailable
		}
		return rows, nil
	}
	row, err := r.lifecycle.catalog.Get(ctx, storage.SessionID(id))
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, errAccessNotFound) {
		return nil, errAccessNotFound
	}
	if err != nil {
		return nil, errAccessUnavailable
	}
	return []storage.Session{row}, nil
}
func (r *accessReader) serviceRows(ctx context.Context, binding access.Binding, after string) ([]protocol.AccessService, error) {
	if r.services == nil {
		return nil, errAccessUnavailable
	}
	rows, err := r.services.metadata(ctx, binding)
	if err != nil {
		return nil, err
	}
	result := make([]protocol.AccessService, 0, len(rows))
	for _, row := range rows {
		if row.Name > after {
			result = append(result, row)
		}
	}
	slices.SortFunc(result, func(a, b protocol.AccessService) int { return strings.Compare(a.Name, b.Name) })
	return result, nil
}
func boundAccessPage(result protocol.AccessReadResult, read protocol.AccessRead, principal access.Principal) (protocol.AccessReadResult, error) {
	limit := read.Limit
	if limit == 0 {
		limit = 100
	}
	count := len(result.Sessions) + len(result.Services)
	if count == 0 {
		return result, nil
	}
	end := min(limit, count)
	page, fits, err := accessPageFits(result, read, principal, end, count)
	if err != nil || fits {
		return page, err
	}
	var best protocol.AccessReadResult
	low, high := 1, end-1
	for low <= high {
		middle := low + (high-low)/2
		page, fits, err = accessPageFits(result, read, principal, middle, count)
		if err != nil {
			return result, err
		}
		if !fits {
			high = middle - 1
			continue
		}
		best = page
		low = middle + 1
	}
	if best.Section == "" {
		return result, errAccessLimit
	}
	return best, nil
}
func accessPageFits(result protocol.AccessReadResult, read protocol.AccessRead, principal access.Principal, end, count int) (protocol.AccessReadResult, bool, error) {
	page, err := accessPage(result, read, principal, end, count)
	if err != nil {
		return result, false, err
	}
	encoded, err := json.Marshal(protocol.AccessReply{Type: protocol.TypeAccessResult, RequestID: strings.Repeat("x", protocol.MaximumAccessRequestID), Result: &page})
	if err != nil {
		return result, false, errAccessUnavailable
	}
	return page, len(encoded) <= protocol.MaximumAccessReply, nil
}

func accessPage(result protocol.AccessReadResult, read protocol.AccessRead, principal access.Principal, end, count int) (protocol.AccessReadResult, error) {
	page := result
	last := ""
	if read.Section == "sessions" {
		page.Sessions = result.Sessions[:end]
		last = page.Sessions[end-1].ID
	}
	if read.Section == "services" {
		page.Services = result.Services[:end]
		last = page.Services[end-1].Name
	}
	if end < count {
		token, err := (protocol.AccessCursor{Target: principal.Target(), Subject: string(principal.Subject()), Incarnation: principal.Incarnation(), Section: read.Section, Last: last, SessionID: read.SessionID}).Encode()
		if err != nil {
			return result, errAccessLimit
		}
		page.NextPage = &token
	}
	return page, nil
}
