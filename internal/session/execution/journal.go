package execution

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Journal never launches a process. A durable new Reserve result elects its sole launcher.
type Journal struct {
	files            *Files
	boot             string
	owner            string
	now              func() time.Time
	clock            func() ClockAnchor
	maxRecords       int
	maxActive        int
	maintenanceAfter string
	maintenanceMu    sync.Mutex
}

func Open(state, boot, owner string) (*Journal, error) {
	if boot == "" || owner == "" {
		return nil, fmt.Errorf("execution: known boot and coordinator required")
	}
	f, err := OpenFiles(state)
	if err != nil {
		return nil, err
	}
	for _, dir := range []string{"execution/requests", "execution/attempts"} {
		if err := f.Ensure(dir); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	j := &Journal{files: f, boot: boot, owner: owner, now: time.Now, maxRecords: 10000, maxActive: 128}
	j.clock = func() ClockAnchor { return clockAnchor(boot) }
	return j, nil
}
func (j *Journal) Close() error { return j.files.Close() }
func (j *Journal) lock(ctx context.Context) (*Fence, error) {
	return j.files.Lock(ctx, "execution/journal.lock")
}
func (j *Journal) read(hash string) (Receipt, error) {
	var r Receipt
	if err := j.files.Read(requestName(hash), &r); err != nil {
		return Receipt{}, err
	}
	if err := r.validate(); err != nil {
		return Receipt{}, err
	}
	if r.Binding.RequestHash != hash {
		return Receipt{}, fmt.Errorf("execution: record hash mismatch")
	}
	err := j.files.Confirm(requestName(hash))
	if err != nil {
		r.Acceptance = string(Unconfirmed)
	}
	return r, err
}
func (j *Journal) save(r Receipt) (Receipt, error) {
	if err := r.validate(); err != nil {
		return r, err
	}
	stage, err := j.files.Publish(requestName(r.Binding.RequestHash), r, false)
	if err != nil {
		r.Acceptance = string(stage)
	}
	return r, err
}
func (j *Journal) Reserve(ctx context.Context, k Key, i Intent, reserve func() (Binding, error)) (Receipt, bool, error) {
	if err := k.Validate(); err != nil {
		return Receipt{}, false, err
	}
	normalized, err := Normalize(i)
	if err != nil {
		return Receipt{}, false, err
	}
	gate, err := j.lock(ctx)
	if err != nil {
		return Receipt{}, false, err
	}
	defer func() { _ = gate.Close() }()
	r, err := j.read(k.Hash())
	if err == nil {
		if r.Key != k || r.IntentDigest != digest(normalized) {
			return Receipt{}, false, ErrConflict
		}
		r, err = j.refresh(r)
		if err == nil {
			err = receiptDiagnostic(r)
		}
		return r, false, err
	}
	if !errors.Is(err, os.ErrNotExist) {
		return r, false, err
	}
	return j.acceptNew(ctx, k, normalized, reserve)
}
func (j *Journal) acceptNew(ctx context.Context, k Key, i Intent, reserve func() (Binding, error)) (Receipt, bool, error) {
	var r Receipt
	if err := j.capacity(); err != nil {
		return Receipt{}, false, err
	}
	if err := ctx.Err(); err != nil {
		return Receipt{}, false, executionBoundaryError("journal.go Reserve", err)
	}
	binding, err := reserve()
	if err != nil {
		return Receipt{}, false, err
	}
	if err := binding.Validate(); err != nil {
		return Receipt{}, false, err
	}
	r = Receipt{Binding: binding, Key: k, IntentDigest: digest(i), Owner: j.owner, Phase: phaseReserved, Acceptance: string(BeforePublish), Publication: pending, LaunchOutcome: notStarted, AcceptedAt: j.now().UTC(), Removal: removalPresent}
	if binding.RequestHash != k.Hash() {
		return r, false, fmt.Errorf("execution: reservation namespace mismatch")
	}
	if err := j.files.Ensure(filepath.Dir(attempt(binding, "reference.json"))); err != nil {
		return r, false, err
	}
	if _, err := j.files.Publish(attempt(binding, "reference.json"), binding, true); err != nil {
		return r, false, err
	}
	// The lifetime inode exists before acceptance and survives until receipt purge.
	fence, err := j.files.Lock(ctx, attempt(binding, "lifetime.lock"))
	if err != nil {
		return r, false, err
	}
	err = errors.Join(j.files.Confirm(attempt(binding, "lifetime.lock")), fence.Close())
	if err != nil {
		return r, false, err
	}
	r.Acceptance = string(Durable)
	r, err = j.save(r)
	return r, err == nil, err
}
func (j *Journal) capacity() error {
	entries, err := j.files.Names("execution/requests")
	if err != nil {
		return err
	}
	count, active := 0, 0
	for _, entry := range entries {
		if entry.Name() == ".barrier" {
			continue
		}
		count++
		active += j.activeEntry(entry.Name())
	}
	if count >= j.maxRecords || active >= j.maxActive {
		return ErrLimit
	}
	return nil
}
func (j *Journal) activeEntry(name string) int {
	if len(name) != 69 || filepath.Ext(name) != ".json" {
		return 1
	}
	r, err := j.read(name[:64])
	if err != nil {
		return 1
	}
	r, err = j.refresh(r)
	if err != nil || r.Phase != phaseTerminal {
		return 1
	}
	return 0
}
func (j *Journal) Lookup(ctx context.Context, k Key) (Receipt, error) {
	if err := k.Validate(); err != nil {
		return Receipt{}, err
	}
	gate, err := j.lock(ctx)
	if err != nil {
		return Receipt{}, err
	}
	defer func() { _ = gate.Close() }()
	r, err := j.read(k.Hash())
	if errors.Is(err, os.ErrNotExist) {
		return Receipt{}, ErrNotFound
	}
	if err != nil {
		return r, err
	}
	if r.Key != k {
		return Receipt{}, ErrConflict
	}
	r, err = j.refresh(r)
	if err == nil {
		err = receiptDiagnostic(r)
	}
	return r, err
}
func (j *Journal) Dispatch(ctx context.Context, b Binding, i Intent) error {
	gate, err := j.lock(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = gate.Close() }()
	r, err := j.read(b.RequestHash)
	if err != nil {
		return err
	}
	if r.Binding != b || r.Owner != j.owner || r.Phase != phaseReserved || r.IntentDigest != digest(i) {
		return ErrUnknown
	}
	d := dispatch{Binding: b, Key: r.Key, Intent: i}
	if _, err := j.files.Publish(attempt(b, "dispatch.json"), d, true); err != nil {
		return err
	}
	r.Phase = phaseDispatched
	r.LaunchOutcome = outcomeUnknown
	_, err = j.save(r)
	return err
}
func (j *Journal) Published(ctx context.Context, b Binding) (Receipt, error) {
	gate, err := j.lock(ctx)
	if err != nil {
		return Receipt{}, err
	}
	defer func() { _ = gate.Close() }()
	r, err := j.read(b.RequestHash)
	if err != nil {
		return r, err
	}
	if r.Binding != b {
		return Receipt{}, ErrUnknown
	}
	r.Publication = publicationPublished
	if r.Phase != phaseTerminal {
		r.Phase = phaseLive
		r.LaunchOutcome = outcomeStarted
	}
	return j.save(r)
}
func (j *Journal) NotStarted(ctx context.Context, b Binding, reason string) (Receipt, error) {
	gate, err := j.lock(ctx)
	if err != nil {
		return Receipt{}, err
	}
	defer func() { _ = gate.Close() }()
	r, err := j.read(b.RequestHash)
	if err != nil {
		return r, err
	}
	if r.Binding != b {
		return Receipt{}, ErrUnknown
	}
	// The launcher can use this only when exec.Start proved no worker was created.
	return j.terminal(r, Result{Binding: b, Termination: Termination{Kind: notStarted, Reason: reason}, Capture: captureNotRecorded})
}
func (j *Journal) refresh(r Receipt) (Receipt, error) {
	if r.Phase == phaseTerminal {
		return j.terminalView(r)
	}
	var result Result
	err := j.files.Read(attempt(r.Binding, "result.json"), &result)
	if err == nil {
		if err := result.validate(r.Binding); err != nil {
			return r, err
		}
		if err := j.files.Confirm(attempt(r.Binding, "result.json")); err != nil {
			return r, err
		}
		return j.terminal(r, result)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return r, err
	}
	if r.Binding.Reservation.BootID != j.boot {
		r.Phase = phaseUncertain
		r.LaunchOutcome = outcomeUnknown
		return j.save(r)
	}
	if r.Phase == phaseReserved {
		return j.abandonReserved(r)
	}
	return r, nil
}
func (j *Journal) abandonReserved(r Receipt) (Receipt, error) {
	var d dispatch
	err := j.files.Read(attempt(r.Binding, "dispatch.json"), &d)
	if !errors.Is(err, os.ErrNotExist) {
		r.Phase = phaseUncertain
		r.LaunchOutcome = outcomeUnknown
		return j.save(r)
	}
	if r.Owner == j.owner {
		return r, nil
	}
	return j.terminal(r, Result{Binding: r.Binding, Termination: Termination{Kind: notStarted, Reason: "coordinator_replaced"}, Capture: captureNotRecorded})
}
func (j *Journal) terminal(r Receipt, result Result) (Receipt, error) {
	if r.Phase == phaseTerminal {
		return r, nil
	}
	now := j.now().UTC()
	retained := now
	if result.EndedAt != nil && result.EndedAt.After(retained) {
		retained = *result.EndedAt
	}
	until := retained.Add(30 * 24 * time.Hour)
	anchor := j.clock()
	r.Phase = phaseTerminal
	r.Result = &result
	r.TerminalAt = &now
	r.RetainUntil = &until
	r.Anchor = &anchor
	if result.Termination.Kind == notStarted {
		r.LaunchOutcome = notStarted
	} else {
		r.LaunchOutcome = outcomeStarted
	}
	return j.save(r)
}

func receiptDiagnostic(r Receipt) error {
	switch r.Phase {
	case phaseReserved:
		return ErrProgress
	case phaseDispatched, phaseUncertain:
		return ErrUnknown
	default:
		return nil
	}
}
