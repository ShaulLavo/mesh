package execution

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
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
	// Context quotas: open contexts per owner and per host, retained in all.
	contextOwnerLimit, contextHostLimit, contextRetainLimit int
	// contextDirsReady records that this journal made the context directories
	// durable. Nothing removes them, so later calls skip three sync rounds
	// that every context create and receipt lookup paid.
	contextDirsReady atomic.Bool
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
	j := &Journal{files: f, boot: boot, owner: owner, now: time.Now, maxRecords: 10000, maxActive: 128, contextOwnerLimit: maxOpenPerOwner, contextHostLimit: maxOpenPerHost, contextRetainLimit: maxRetainedContexts}
	if err := j.initializeLock(); err != nil {
		_ = f.Close()
		return nil, err
	}
	j.clock = func() ClockAnchor { return clockAnchor(boot) }
	return j, nil
}
func (j *Journal) Close() error { return j.files.Close() }
func (j *Journal) lock(ctx context.Context) (*Fence, error) {
	return j.files.LockExisting(ctx, "execution/journal.lock")
}
func (j *Journal) read(hash string) (Receipt, error) {
	r, err := j.readRecord(hash)
	if err != nil {
		return r, err
	}
	return j.confirmRead(r)
}

func (j *Journal) confirmRead(r Receipt) (Receipt, error) {
	err := j.files.Confirm(requestName(r.Binding.RequestHash))
	if err != nil {
		r.Acceptance = string(Unconfirmed)
	}
	return r, err
}

// readToReplace reads a receipt, under the journal lock, that the caller
// replaces before it replies or acts. Saving the replacement makes the path's
// new contents durable, which is all the outcome relies on, so the receipt
// read here is not confirmed first. A caller that keeps it unchanged must
// confirm it with confirmRead.
func (j *Journal) readToReplace(hash string) (Receipt, error) {
	return j.readRecord(hash)
}

// readRecord decodes a receipt without proving it durable, for callers that
// only count or classify records and never act on their durability.
func (j *Journal) readRecord(hash string) (Receipt, error) {
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
	if r.Cwd == "" && r.Context != nil {
		if snap, snapErr := j.ReadLaunchSnapshot(r); snapErr == nil && snap.Intent.Cwd != "" {
			r.Cwd = snap.Intent.Cwd
		}
	}
	return r, nil
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
	return j.reserveWith(ctx, k, i, reserve, acceptHooks{})
}
func (j *Journal) reserveWith(ctx context.Context, k Key, i Intent, reserve func() (Binding, error), hooks acceptHooks) (Receipt, bool, error) {
	if err := k.Validate(); err != nil {
		return Receipt{}, false, err
	}
	normalized, err := Normalize(i)
	if err != nil {
		return Receipt{}, false, err
	}
	if err := j.prepareOutputAdmission(ctx, k); err != nil {
		return Receipt{}, false, err
	}
	r, owner, err := j.reserve(ctx, k, normalized, reserve, hooks)
	if err != nil || owner {
		return r, owner, err
	}
	r, err = j.refresh(ctx, r)
	if err == nil {
		err = receiptDiagnostic(r)
	}
	return r, false, err
}
func (j *Journal) reserve(ctx context.Context, k Key, normalized Intent, reserve func() (Binding, error), hooks acceptHooks) (Receipt, bool, error) {
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
		return r, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return r, false, err
	}
	return j.acceptNew(ctx, k, normalized, reserve, hooks)
}

// acceptHooks extend a reservation before its receipt becomes durable:
// decorate adds receipt evidence, and prepare adds files that must exist
// whenever the receipt does to the attempt's sync round, so they must not
// depend on one another. With dispatch, the new reservation is also
// dispatched in the same journal lock hold, once its reserved receipt is
// durable.
type acceptHooks struct {
	decorate func(*Receipt)
	prepare  func(Binding, *round) error
	dispatch bool
}

// acceptNew publishes a fresh reservation under the journal lock.
func (j *Journal) acceptNew(ctx context.Context, k Key, i Intent, reserve func() (Binding, error), hooks acceptHooks) (Receipt, bool, error) {
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
	r = Receipt{Binding: binding, Key: k, IntentDigest: digest(i), Owner: j.owner, Phase: phaseReserved, Acceptance: string(BeforePublish), Publication: pending, LaunchOutcome: notStarted, AcceptedAt: j.now().UTC(), Removal: removalPresent, Cwd: i.Cwd}
	if hooks.decorate != nil {
		hooks.decorate(&r)
	}
	if !validInternalNamespace(binding.Mode(), k.Class) {
		return r, false, fmt.Errorf("execution: invalid internal request namespace")
	}
	if binding.RequestHash != k.Hash() || binding.Mode() != i.Kind || binding.Stdin != i.Stdin {
		return r, false, fmt.Errorf("execution: reservation namespace mismatch")
	}
	if err := j.prepareAttempt(binding, hooks); err != nil {
		return r, false, err
	}
	if err := admitted(ctx); err != nil {
		return r, false, executionBoundaryError("execution admission canceled", err)
	}
	r.Acceptance = string(Durable)
	r, err = j.save(r)
	if err != nil || !hooks.dispatch {
		return r, err == nil, err
	}
	// The reserved receipt is durable on its own first, so a coordinator
	// that dies before the dispatch below still settles as not_started.
	// Holding the lock throughout means nothing changed the receipt since,
	// so it is neither read nor confirmed again.
	r, err = j.dispatchReserved(r, i)
	return r, true, err
}

// prepareAttempt stores the files a durable receipt relies on, in one sync
// round: none of them depends on another, and the receipt comes after. The
// attempt directory is new, so its own files are reachable only through it.
// The lifetime inode exists before acceptance and survives until receipt
// purge.
func (j *Journal) prepareAttempt(binding Binding, hooks acceptHooks) error {
	reference := attempt(binding, "reference.json")
	r := j.files.newRound()
	err := r.add(filepath.Dir(reference), []string{"lifetime.lock"}, []Record{{Name: reference, Value: binding, Exclusive: true}})
	if err == nil && hooks.prepare != nil {
		err = hooks.prepare(binding, r)
	}
	if err == nil {
		err = r.run()
	}
	return errors.Join(err, r.close())
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
	// Admission only counts active records; proving each one durable here made
	// every create pay a full fsync round per retained receipt.
	r, err := j.readRecord(name[:64])
	if err != nil {
		return 1
	}
	if r.Phase != phaseTerminal {
		return 1
	}
	if r.OutputRetainUntil != nil && !r.OutputExpired {
		return 1
	}
	return 0
}
func (j *Journal) Lookup(ctx context.Context, k Key) (Receipt, error) {
	if err := k.Validate(); err != nil {
		return Receipt{}, err
	}
	r, err := j.snapshot(ctx, k.Hash())
	if errors.Is(err, os.ErrNotExist) && r.Binding.RequestHash == "" {
		return Receipt{}, ErrNotFound
	}
	if err != nil {
		return r, err
	}
	if r.Key != k {
		return Receipt{}, ErrConflict
	}
	r, err = j.refresh(ctx, r)
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
	if r.Binding != b || r.Owner != j.owner || r.Phase != phaseReserved || r.launchIntentDigest() != digest(i) {
		return ErrUnknown
	}
	_, err = j.dispatchReserved(r, i)
	return err
}

// dispatchReserved records the launch intent for a reserved receipt that is
// durable, then saves the receipt as dispatched. The caller holds the lock.
// If either write fails, it returns the reserved receipt as last proven, with
// its acceptance, and the failure separately: a replacement that failed
// before or after its rename proves nothing about the job's acceptance.
func (j *Journal) dispatchReserved(r Receipt, i Intent) (Receipt, error) {
	if r.launchIntentDigest() != digest(i) {
		return r, ErrUnknown
	}
	d := dispatch{Binding: r.Binding, Key: r.Key, Intent: i}
	if _, err := j.files.Publish(attempt(r.Binding, "dispatch.json"), d, true); err != nil {
		return r, executionBoundaryError("record dispatch", err)
	}
	next := r
	next.Phase = phaseDispatched
	next.LaunchOutcome = outcomeUnknown
	saved, err := j.save(next)
	if err != nil {
		return r, executionBoundaryError("save dispatched receipt", err)
	}
	return saved, nil
}

// ReserveAndDispatch is Reserve for a caller that launches the job at once:
// a new reservation is also dispatched in the same journal lock hold, once
// its reserved receipt is durable. An existing key is returned as Reserve
// returns it, never dispatched.
func (j *Journal) ReserveAndDispatch(ctx context.Context, k Key, i Intent, reserve func() (Binding, error)) (Receipt, bool, error) {
	return j.reserveWith(ctx, k, i, reserve, acceptHooks{dispatch: true})
}

// Published records that the exact worker published the job and returns the
// receipt a lookup would. A job that already finished is saved once as
// terminal, rather than as live and then again by the next lookup. A result
// that cannot be read is reported after the publication is saved, as a later
// lookup would report it.
func (j *Journal) Published(ctx context.Context, b Binding) (Receipt, error) {
	// The result is confirmed under its stream fences before the journal lock,
	// in the same order refresh takes them.
	result, resultErr := j.result(ctx, b)
	gate, err := j.lock(ctx)
	if err != nil {
		return Receipt{}, err
	}
	defer func() { _ = gate.Close() }()
	r, err := j.readToReplace(b.RequestHash)
	if err != nil {
		return r, err
	}
	if r.Binding != b {
		return Receipt{}, ErrUnknown
	}
	if r.Phase == phaseTerminal {
		return j.publishedTerminal(r)
	}
	r.Publication = publicationPublished
	if resultErr == nil {
		return j.terminal(r, result)
	}
	r.Phase = phaseLive
	r.LaunchOutcome = outcomeStarted
	saved, err := j.save(r)
	if err == nil && !IsResultPending(resultErr) {
		err = resultErr
	}
	return saved, err
}

func (j *Journal) publishedTerminal(r Receipt) (Receipt, error) {
	if r.Publication == publicationPublished {
		return j.confirmedTerminalView(r)
	}
	r.Publication = publicationPublished
	saved, err := j.save(r)
	if err != nil {
		return saved, err
	}
	return j.terminalView(saved)
}

// Complete settles the exact receipt with a result the caller already read
// through FinalResult, which confirmed it and its final captures durable, so
// they are not confirmed a second time.
func (j *Journal) Complete(ctx context.Context, b Binding, result Result) (Receipt, error) {
	if err := result.validate(b); err != nil {
		return Receipt{}, err
	}
	gate, err := j.lock(ctx)
	if err != nil {
		return Receipt{}, err
	}
	defer func() { _ = gate.Close() }()
	r, err := j.readToReplace(b.RequestHash)
	if err != nil {
		return r, err
	}
	if r.Binding != b {
		return Receipt{}, ErrInstance
	}
	if r.Phase == phaseTerminal {
		return j.confirmedTerminalView(r)
	}
	return j.terminal(r, result)
}

// confirmedTerminalView returns a terminal receipt read with readToReplace,
// which is kept unchanged and so is confirmed first.
func (j *Journal) confirmedTerminalView(r Receipt) (Receipt, error) {
	r, err := j.confirmRead(r)
	if err != nil {
		return r, err
	}
	return j.terminalView(r)
}

// FinalResult reads the exact attempt's result and confirms it and its final
// captures durable, for Complete. Its stream stores are fresh, so a capture
// that an output reader saw before the job finished is confirmed again with
// all its barriers rather than trusted from that earlier read. A job still
// running reports IsResultPending.
func (j *Journal) FinalResult(ctx context.Context, b Binding) (Result, error) {
	return j.result(ctx, b)
}

// result reads and confirms the exact attempt's result and final captures.
func (j *Journal) result(ctx context.Context, b Binding) (Result, error) {
	output := &Output{files: j.files, streams: make(map[streamKey]*streamStore)}
	return output.Result(ctx, b)
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
	return j.terminal(r, Result{Binding: b, Termination: Termination{Kind: notStarted, Reason: reason}, Capture: CaptureNotRecorded})
}
func (j *Journal) snapshot(ctx context.Context, hash string) (Receipt, error) {
	gate, err := j.lock(ctx)
	if err != nil {
		return Receipt{}, err
	}
	defer func() { _ = gate.Close() }()
	return j.read(hash)
}
func (j *Journal) observe(ctx context.Context, hash string) (Receipt, error) {
	r, err := j.snapshot(ctx, hash)
	if err != nil {
		return r, err
	}
	return j.refresh(ctx, r)
}
func (j *Journal) refresh(ctx context.Context, snapshot Receipt) (Receipt, error) {
	var result Result
	resultErr := errResultPending
	if snapshot.Phase != phaseTerminal {
		result, resultErr = j.result(ctx, snapshot.Binding)
		if resultErr != nil && !IsResultPending(resultErr) {
			return snapshot, resultErr
		}
	}
	if IsResultPending(resultErr) && snapshot.Binding.Mode() != KindPTY && snapshot.Binding.Reservation.BootID != j.boot && j.startedBeforeReboot(snapshot) {
		result, resultErr = j.interruptedPipe(ctx, snapshot.Binding)
		if resultErr != nil {
			return snapshot, resultErr
		}
	}
	gate, err := j.lock(ctx)
	if err != nil {
		return snapshot, err
	}
	defer func() { _ = gate.Close() }()
	r, err := j.read(snapshot.Binding.RequestHash)
	if err != nil {
		return r, err
	}
	if r.Binding != snapshot.Binding {
		return Receipt{}, ErrInstance
	}
	if r.Phase == phaseTerminal {
		return j.terminalView(r)
	}
	if resultErr == nil {
		return j.terminal(r, result)
	}
	if r.Binding.Reservation.BootID != j.boot {
		r.Phase, r.LaunchOutcome = phaseUncertain, outcomeUnknown
		return j.save(r)
	}
	if r.Phase == phaseReserved {
		return j.abandonReserved(r)
	}
	return r, nil
}
func (j *Journal) startedBeforeReboot(r Receipt) bool {
	if r.LaunchOutcome == outcomeStarted {
		return true
	}
	name := attempt(r.Binding, "started.json")
	var started Binding
	return j.files.Read(name, &started) == nil && started == r.Binding && j.files.Confirm(name) == nil
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
	return j.terminal(r, Result{Binding: r.Binding, Termination: Termination{Kind: notStarted, Reason: "coordinator_replaced"}, Capture: CaptureNotRecorded})
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
	if len(result.Streams) > 0 || (r.Binding.Mode() != KindPTY && result.EndedAt != nil) {
		outputUntil := now.Add(7 * 24 * time.Hour)
		r.OutputRetainUntil = &outputUntil
	}
	if r.KillRequested && (result.Termination.Kind == "signal" || result.Termination.Kind == "exit") {
		result.Termination.Kind = "killed"
		r.Result = &result
	}
	r.RetainUntil = &until
	r.Anchor = &anchor
	if result.Termination.Kind == notStarted {
		r.LaunchOutcome = notStarted
	} else {
		r.LaunchOutcome = outcomeStarted
	}
	saved, err := j.save(r)
	if err != nil {
		return saved, err
	}
	return j.terminalView(saved)
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

func (j *Journal) initializeLock() error {
	if err := j.files.check("execution/journal.lock", false); err == nil {
		return j.files.Confirm("execution/journal.lock")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, dir := range []string{"execution/requests", "execution/attempts"} {
		entries, err := j.files.Names(dir)
		if err != nil {
			return err
		}
		if populatedJournalDirectory(entries) {
			return fmt.Errorf("execution: missing journal lock in populated state")
		}
	}
	_, err := j.files.Publish("execution/journal.lock", struct{}{}, true)
	if errors.Is(err, os.ErrExist) {
		return j.files.Confirm("execution/journal.lock")
	}
	return err
}

func populatedJournalDirectory(entries []os.DirEntry) bool {
	for _, entry := range entries {
		if entry.Name() != ".barrier" {
			return true
		}
	}
	return false
}

func (j *Journal) Exact(ctx context.Context, b Binding) (Receipt, error) {
	r, err := j.snapshot(ctx, b.RequestHash)
	if err != nil {
		return r, err
	}
	if r.Binding != b {
		return Receipt{}, ErrInstance
	}
	return j.refresh(ctx, r)
}
func (j *Journal) MarkKill(ctx context.Context, b Binding) error {
	gate, err := j.lock(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = gate.Close() }()
	r, err := j.read(b.RequestHash)
	if err != nil {
		return err
	}
	if r.Binding != b || r.Removal != removalPresent {
		return ErrInstance
	}
	r.KillRequested = true
	_, err = j.save(r)
	return err
}
