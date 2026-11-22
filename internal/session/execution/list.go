package execution

import (
	"context"
	"sort"
	"strings"
)

const requestJSONSuffix = ".json"

// List returns retained execution receipts owned by (host, class, principal),
// excluding internal line workers, ordered newest first.
func (j *Journal) List(ctx context.Context, host, class, principal string) ([]Receipt, error) {
	snapshots, err := j.listSnapshots(ctx, host, class, principal)
	if err != nil {
		return nil, err
	}
	return j.refreshReceipts(ctx, snapshots)
}

func (j *Journal) listSnapshots(ctx context.Context, host, class, principal string) ([]Receipt, error) {
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
		r, ok := j.readOwnedSnapshot(entry.Name(), host, class, principal)
		if ok {
			snapshots = append(snapshots, r)
		}
	}
	return snapshots, nil
}

func (j *Journal) readOwnedSnapshot(name, host, class, principal string) (Receipt, bool) {
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
	return r, true
}

func (j *Journal) refreshReceipts(ctx context.Context, snapshots []Receipt) ([]Receipt, error) {
	results := make([]Receipt, 0, len(snapshots))
	for _, snapshot := range snapshots {
		if err := ctx.Err(); err != nil {
			return nil, executionBoundaryError("list execution receipts", err)
		}
		if refreshed, err := j.refresh(ctx, snapshot); err == nil {
			results = append(results, refreshed)
		} else {
			results = append(results, snapshot)
		}
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].AcceptedAt.Equal(results[j].AcceptedAt) {
			return results[i].Binding.Reservation.ID < results[j].Binding.Reservation.ID
		}
		return results[i].AcceptedAt.After(results[j].AcceptedAt)
	})
	return results, nil
}
