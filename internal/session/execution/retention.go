package execution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/shaul/mesh/internal/session"
)

const receiptFloor = 30 * 24 * time.Hour

// RetireSession keeps the exact finalizer fence across the existing catalog retirement.
func (j *Journal) RetireSession(ctx context.Context, sessions, id string, retire func() error) error {
	var ref session.Reservation
	if err := j.files.Read(filepath.Join(filepath.Base(sessions), id, ".reservation"), &ref); err != nil {
		return err
	}
	if err := ref.Validate(); err != nil {
		return executionBoundaryError("retention.go RetireSession", err)
	}
	if ref.ID != id || ref.Purpose != session.Execution {
		return ErrProtected
	}
	var binding Binding
	if err := j.files.Read(filepath.Join("execution", "attempts", ref.Instance, "reference.json"), &binding); err != nil {
		return err
	}
	if binding.Reservation != ref {
		return ErrProtected
	}
	fence, err := j.files.LockExisting(ctx, attempt(binding, "lifetime.lock"))
	if err != nil {
		return err
	}
	defer func() { _ = fence.Close() }()
	if err := j.prepareRetirement(ctx, binding); err != nil {
		return err
	}
	// Re-read the directory reference under the lifetime fence immediately before deletion.
	var current session.Reservation
	if err := j.files.Read(filepath.Join(filepath.Base(sessions), id, ".reservation"), &current); err != nil {
		return err
	}
	if current != ref {
		return ErrProtected
	}
	if err := retire(); err != nil {
		return err
	}
	if err := j.files.RemoveTree(filepath.Join(filepath.Base(sessions), id), "execution/.barrier"); err != nil {
		return err
	}
	return j.completeRetirement(ctx, binding)
}
func (j *Journal) prepareRetirement(ctx context.Context, b Binding) error {
	gate, err := j.lock(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = gate.Close() }()
	r, err := j.read(b.RequestHash)
	if err != nil {
		return err
	}
	if r.Binding != b {
		return ErrProtected
	}
	if r.Phase != phaseTerminal {
		var result Result
		if err := j.files.Read(attempt(b, "result.json"), &result); err != nil {
			return ErrProtected
		}
	}
	r, err = j.refresh(r)
	if err != nil {
		return err
	}
	if r.Phase != phaseTerminal {
		return ErrProtected
	}
	r.Removal = pending
	_, err = j.save(r)
	return err
}
func (j *Journal) completeRetirement(ctx context.Context, b Binding) error {
	gate, err := j.lock(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = gate.Close() }()
	r, err := j.read(b.RequestHash)
	if err != nil {
		return err
	}
	if r.Binding != b || r.Removal != pending {
		return ErrProtected
	}
	r.Removal = removalRemoved
	_, err = j.save(r)
	return err
}

// Maintain consumes bounded batches on the existing daemon maintenance tick.
func (j *Journal) Maintain(ctx context.Context) error {
	j.maintenanceMu.Lock()
	defer j.maintenanceMu.Unlock()
	entries, err := j.files.Names("execution/requests")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var failures []error
	processed := 0
	for _, entry := range entries {
		name := entry.Name()
		if len(name) != 69 || filepath.Ext(name) != ".json" || name <= j.maintenanceAfter {
			continue
		}
		if err := ctx.Err(); err != nil {
			return executionBoundaryError("retention.go Maintain", err)
		}
		j.maintenanceAfter = name
		processed++
		if err := j.maintainEntry(ctx, name[:64]); err != nil {
			failures = append(failures, err)
		}
		if processed >= 64 {
			return errors.Join(failures...)
		}
	}
	j.maintenanceAfter = ""
	return errors.Join(failures...)
}
func (j *Journal) maintainEntry(ctx context.Context, hash string) error {
	r, err := j.maintenanceRead(ctx, hash)
	if err != nil {
		return err
	}
	if r.Phase != phaseTerminal {
		return nil
	}
	if r.Removal == pending {
		return j.resumeRetirement(ctx, r.Binding)
	}
	if r.Removal != removalRemoved || !elapsedFloor(*r.Anchor, j.clock()) {
		return nil
	}
	if r.RetainUntil != nil && j.now().Before(*r.RetainUntil) {
		return nil
	}
	fence, err := j.files.LockExisting(ctx, attempt(r.Binding, "lifetime.lock"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if fence != nil {
		defer func() { _ = fence.Close() }()
	}
	// A durable removed tombstone already proves the previous lifetime fence.
	// A purge interrupted after deleting that inode must still converge.
	return j.purgeRemoved(ctx, r)
}
func (j *Journal) purgeRemoved(ctx context.Context, r Receipt) error {
	hash := r.Binding.RequestHash
	gate, err := j.lock(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = gate.Close() }()
	current, err := j.read(hash)
	if err != nil {
		return err
	}
	if current.Binding != r.Binding || current.Removal != removalRemoved || current.Anchor == nil || !elapsedFloor(*current.Anchor, j.clock()) {
		return ErrProtected
	}
	if err := j.files.RemoveTree(filepath.Dir(attempt(r.Binding, "reference.json")), "execution/.barrier"); err != nil {
		return err
	}
	return j.files.Remove(requestName(hash), "execution/.barrier")
}
func (j *Journal) maintenanceRead(ctx context.Context, hash string) (Receipt, error) {
	gate, err := j.lock(ctx)
	if err != nil {
		return Receipt{}, err
	}
	defer func() { _ = gate.Close() }()
	r, err := j.read(hash)
	if err != nil {
		return r, err
	}
	return j.refresh(r)
}
func (j *Journal) resumeRetirement(ctx context.Context, b Binding) error {
	fence, err := j.files.LockExisting(ctx, attempt(b, "lifetime.lock"))
	if err != nil {
		return err
	}
	defer func() { _ = fence.Close() }()
	if err := j.retirePayload(b); err != nil {
		return err
	}
	return j.completeRetirement(ctx, b)
}

func (j *Journal) retirePayload(b Binding) error {
	name := filepath.Join("s", b.Reservation.ID)
	var current session.Reservation
	err := j.files.Read(filepath.Join(name, ".reservation"), &current)
	if errors.Is(err, os.ErrNotExist) {
		return j.confirmAbsentPayload(name)
	}
	if err != nil {
		return err
	}
	if current != b.Reservation {
		return ErrProtected
	}
	return j.files.RemoveTree(name, "execution/.barrier")
}

func (j *Journal) confirmAbsentPayload(name string) error {
	_, err := j.files.root.Lstat(name)
	if err == nil {
		return ErrProtected
	}
	if !errors.Is(err, os.ErrNotExist) {
		return executionBoundaryError("inspect retired payload", err)
	}
	// A new allocation may now use this name. Settle the previous deletion
	// without removing anything at the reused path.
	return errors.Join(j.files.syncDirs(name), j.files.Confirm("execution/.barrier"))
}
func (j *Journal) terminalView(r Receipt) (Receipt, error) {
	now := j.clock()
	if now.valid() && (!r.Anchor.valid() || now.Boot != r.Anchor.Boot) {
		r.Anchor = &now
		until := j.now().UTC().Add(receiptFloor)
		r.RetainUntil = &until
		return j.save(r)
	}
	if !elapsedFloor(*r.Anchor, now) && r.RetainUntil != nil && !j.now().Before(*r.RetainUntil) {
		r.RetainUntil = nil
		r.RetentionReason = "elapsed_unconfirmed"
	}
	if !r.Anchor.valid() {
		r.RetainUntil = nil
		r.RetentionReason = "elapsed_unconfirmed"
	}
	if j.recoveryPinned(r) {
		r.RetentionReason = "recovery_pinned"
	}
	return r, nil
}

func (j *Journal) recoveryPinned(r Receipt) bool {
	if r.Removal != removalPresent {
		return false
	}
	var current session.Reservation
	if err := j.files.Read("s/"+r.Binding.Reservation.ID+"/.reservation", &current); err != nil || current != r.Binding.Reservation {
		return false
	}
	return j.files.check("s/"+r.Binding.Reservation.ID+"/recovery.json", false) == nil
}
