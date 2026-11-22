package execution

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	requestJSONSuffix       = ".json"
	defaultListLimit        = 100
	maxListLimit            = 500
	defaultListMaxWireBytes = 2 << 20
	maxListCursorBytes      = 2048
)

// ErrListCursorInvalid indicates a malformed or mismatched execution list cursor.
var ErrListCursorInvalid = errors.New("execution list cursor is invalid or bound to another owner or filter")

// ListQuery bounds one host-side page of caller-owned execution receipts.
type ListQuery struct {
	Host         string
	Class        string
	Principal    string
	Context      string
	IncludeEnded bool
	Limit        int
	Cursor       string
	MaxBytes     int
}

// ListPageResult is one byte- and count-bounded page of receipts plus an optional
// owner- and filter-bound continuation cursor.
type ListPageResult struct {
	Receipts   []Receipt
	NextCursor *string
}

type listCursorPayload struct {
	Host       string    `json:"host"`
	Class      string    `json:"class"`
	Principal  string    `json:"principal"`
	Context    string    `json:"context,omitempty"`
	All        bool      `json:"all"`
	AcceptedAt time.Time `json:"acceptedAt"`
	SessionID  string    `json:"sessionId"`
}

// ListPage returns one host-side bounded page of retained execution receipts
// owned by (Host, Class, Principal), excluding internal line workers, ordered
// by AcceptedAt descending then SessionID ascending.
func (j *Journal) ListPage(ctx context.Context, query ListQuery, encodeSize func(Receipt) int) (ListPageResult, error) {
	limit := query.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		return ListPageResult{}, fmt.Errorf("execution list limit exceeds %d: %w", maxListLimit, ErrListCursorInvalid)
	}
	maxBytes := query.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultListMaxWireBytes
	}
	cursor, err := decodeListCursor(query)
	if err != nil {
		return ListPageResult{}, err
	}
	snapshots, err := j.listSnapshots(ctx, query.Host, query.Class, query.Principal, query.Context)
	if err != nil {
		return ListPageResult{}, err
	}
	sortReceipts(snapshots)
	return j.collectPage(ctx, snapshots, query, cursor, limit, maxBytes, encodeSize)
}

func decodeListCursor(query ListQuery) (*listCursorPayload, error) {
	if query.Cursor == "" {
		return nil, nil
	}
	if len(query.Cursor) > maxListCursorBytes {
		return nil, ErrListCursorInvalid
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(query.Cursor)
	if err != nil {
		return nil, ErrListCursorInvalid
	}
	var c listCursorPayload
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, ErrListCursorInvalid
	}
	if c.Host != query.Host || c.Class != query.Class || c.Principal != query.Principal || c.Context != query.Context || c.All != query.IncludeEnded || c.SessionID == "" {
		return nil, ErrListCursorInvalid
	}
	return &c, nil
}

func encodeListCursor(query ListQuery, last Receipt) string {
	raw, _ := json.Marshal(listCursorPayload{
		Host:       query.Host,
		Class:      query.Class,
		Principal:  query.Principal,
		Context:    query.Context,
		All:        query.IncludeEnded,
		AcceptedAt: last.AcceptedAt.UTC(),
		SessionID:  last.Binding.Reservation.ID,
	})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func afterCursor(r Receipt, cursor *listCursorPayload) bool {
	if cursor == nil {
		return true
	}
	if r.AcceptedAt.Before(cursor.AcceptedAt) {
		return true
	}
	if r.AcceptedAt.After(cursor.AcceptedAt) {
		return false
	}
	return r.Binding.Reservation.ID > cursor.SessionID
}

func sortReceipts(items []Receipt) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].AcceptedAt.Equal(items[j].AcceptedAt) {
			return items[i].Binding.Reservation.ID < items[j].Binding.Reservation.ID
		}
		return items[i].AcceptedAt.After(items[j].AcceptedAt)
	})
}

func (j *Journal) collectPage(ctx context.Context, snapshots []Receipt, query ListQuery, cursor *listCursorPayload, limit, maxBytes int, encodeSize func(Receipt) int) (ListPageResult, error) {
	page := make([]Receipt, 0, min(limit, len(snapshots)))
	usedBytes := 0
	for i := range snapshots {
		if err := ctx.Err(); err != nil {
			return ListPageResult{}, executionBoundaryError("list execution receipts", err)
		}
		rec, ok := j.eligibleReceipt(ctx, snapshots[i], query, cursor)
		if !ok {
			continue
		}
		size := receiptSize(rec, encodeSize)
		if len(page) >= limit || (len(page) > 0 && usedBytes+size > maxBytes) {
			next := encodeListCursor(query, page[len(page)-1])
			return ListPageResult{Receipts: page, NextCursor: &next}, nil
		}
		page = append(page, rec)
		usedBytes += size
	}
	return ListPageResult{Receipts: page}, nil
}

func (j *Journal) eligibleReceipt(ctx context.Context, snap Receipt, query ListQuery, cursor *listCursorPayload) (Receipt, bool) {
	if !afterCursor(snap, cursor) {
		return Receipt{}, false
	}
	if !query.IncludeEnded && snap.Phase == phaseTerminal {
		return Receipt{}, false
	}
	rec := snap
	if refreshed, err := j.refresh(ctx, rec); err == nil {
		rec = refreshed
	}
	if !query.IncludeEnded && rec.Phase == phaseTerminal {
		return Receipt{}, false
	}
	return rec, true
}

func receiptSize(rec Receipt, encodeSize func(Receipt) int) int {
	if encodeSize != nil {
		return encodeSize(rec)
	}
	raw, _ := json.Marshal(rec)
	return len(raw) + 2
}

func (j *Journal) listSnapshots(ctx context.Context, host, class, principal, contextID string) ([]Receipt, error) {
	gate, err := j.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = gate.Close() }()
	entries, err := j.files.Names("execution/requests")
	if err != nil {
		return nil, err
	}
	snapshots := make([]Receipt, 0, len(entries))
	for _, entry := range entries {
		r, ok := j.readOwnedSnapshot(entry.Name(), host, class, principal, contextID)
		if ok {
			snapshots = append(snapshots, r)
		}
	}
	return snapshots, nil
}

func (j *Journal) readOwnedSnapshot(name, host, class, principal, contextID string) (Receipt, bool) {
	if len(name) != 69 || !strings.HasSuffix(name, requestJSONSuffix) {
		return Receipt{}, false
	}
	r, err := j.read(name[:64])
	if err != nil || r.Binding.Mode() == KindLine {
		return Receipt{}, false
	}
	if r.Key.Host != host || r.Key.Class != class || r.Key.Principal != principal {
		return Receipt{}, false
	}
	if contextID != "" && r.Key.Context != contextID {
		return Receipt{}, false
	}
	return r, true
}
