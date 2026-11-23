package execution

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Context errors. A context another caller owns reads as not found, so a
// guessed or swapped context ID reveals nothing about its owner.
var (
	ErrContextNotFound = errors.New("execution context not found")
	ErrContextClosed   = errors.New("execution context is closed")
	ErrContextConflict = errors.New("execution context revision changed")
	ErrContextLimit    = errors.New("execution context limit reached")
)

// Context limits. They mirror protocol's limits; this package does not import
// the wire format.
const (
	contextIdleExpiry     = 30 * 24 * time.Hour
	maxOpenPerOwner       = 64
	maxOpenPerHost        = 1024
	maxRetainedContexts   = 4096
	contextUseGranularity = time.Hour
	// Context records hold a 32 KiB overlay and 4 KiB paths. JSON may escape
	// any byte to six, so they get the largest bounded record.
	contextRecordLimit = MaxBoundedRecord
)

// ContextSpec is a resolved context: what the host accepted at open.
type ContextSpec struct {
	Cwd       string            `json:"cwd"`
	Shell     string            `json:"shell"`
	ShellPath string            `json:"shellPath,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Label     string            `json:"label,omitempty"`
}

// Context is one context record. Env holds values and never leaves the
// host; callers see names and a digest.
type Context struct {
	ID           string            `json:"id"`
	Host         string            `json:"host"`
	Owner        string            `json:"owner"`
	Key          Key               `json:"key"`
	IntentDigest string            `json:"intentDigest"`
	Cwd          string            `json:"cwd"`
	Shell        string            `json:"shell"`
	ShellPath    string            `json:"shellPath,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	Label        string            `json:"label,omitempty"`
	Revision     uint64            `json:"revision"`
	CreatedAt    time.Time         `json:"createdAt"`
	LastUsedAt   time.Time         `json:"lastUsedAt"`
	ClosedAt     *time.Time        `json:"closedAt,omitempty"`
	// Members are the keys of jobs whose receipts are not terminal yet. Any
	// of them, running or uncertain, keeps the context from expiring.
	Members []Key `json:"-"`
}

// ExpiresAt is when an unused context with no active job is purged.
func (c Context) ExpiresAt() time.Time { return c.LastUsedAt.Add(contextIdleExpiry) }

// contextRequest is the caller-keyed open receipt. It holds the accepted
// result, so a retry returns exactly what the first open created, and a crash
// between the receipt and the record still converges. Expired is the durable
// decision to purge; a receipt marked expired never reopens its context.
type contextRequest struct {
	Key          Key         `json:"key"`
	IntentDigest string      `json:"intentDigest"`
	ContextID    string      `json:"contextId"`
	Spec         ContextSpec `json:"spec"`
	AcceptedAt   time.Time   `json:"acceptedAt"`
	Expired      bool        `json:"expired,omitempty"`
}

// ContextChange edits a context. Nil fields keep their value.
type ContextChange struct {
	Cwd      *string
	SetEnv   map[string]string
	UnsetEnv []string
	Label    *string
}

const (
	contextDir        = "execution/contexts"
	contextRequestDir = "execution/context-requests"
	contextMemberDir  = "execution/context-jobs"
	recordExtension   = ".json"
	journalAnchor     = "execution/.barrier"
)

func contextName(id string) string { return filepath.Join(contextDir, id+recordExtension) }
func contextRequestName(hash string) string {
	return filepath.Join(contextRequestDir, hash+recordExtension)
}
func contextMembers(id string) string { return filepath.Join(contextMemberDir, id) }
func contextMember(id, hash string) string {
	return filepath.Join(contextMembers(id), hash+recordExtension)
}

func (j *Journal) ensureContextDirs() error {
	return errors.Join(j.files.Ensure(contextDir), j.files.Ensure(contextRequestDir), j.files.Ensure(contextMemberDir))
}

func cutJSON(name string) (string, bool) {
	id, ok := strings.CutSuffix(name, recordExtension)
	return id, ok
}

func validContextID(id string) bool {
	raw, err := hex.DecodeString(id)
	return err == nil && len(raw) == 16 && hex.EncodeToString(raw) == id
}

func validOpenKey(k Key) error {
	if err := k.Validate(); err != nil || k.Operation != OperationContextOpen {
		return errors.Join(fmt.Errorf("execution: invalid context open key"), err)
	}
	return nil
}

// OpenContext opens a context under a caller key. callerDigest covers the
// normalized caller-supplied request only. The receipt is checked before
// resolve runs, so a retry does not depend on the host's current shell or
// directories, and it returns the result the first open accepted. A changed
// request under the same key fails with ErrConflict.
func (j *Journal) OpenContext(ctx context.Context, k Key, callerDigest string, resolve func() (ContextSpec, error)) (Context, bool, error) {
	if err := validOpenKey(k); err != nil || !validDigest(callerDigest) {
		return Context{}, false, errors.Join(fmt.Errorf("execution: invalid context open"), err)
	}
	if err := j.ensureContextDirs(); err != nil {
		return Context{}, false, err
	}
	gate, err := j.lockAdmitted(ctx)
	if err != nil {
		return Context{}, false, err
	}
	defer func() { _ = gate.Close() }()
	request, found, err := j.readRequest(k.Hash())
	if err != nil {
		return Context{}, false, err
	}
	if found {
		c, err := j.replayOpen(k, callerDigest, request)
		if !errors.Is(err, ErrContextNotFound) {
			return c, false, err
		}
		// The context expired and its receipt went with it: a fresh open.
	}
	return j.acceptContext(k, callerDigest, resolve)
}

func (j *Journal) readRequest(hash string) (contextRequest, bool, error) {
	var request contextRequest
	err := j.files.ReadBounded(contextRequestName(hash), &request, contextRecordLimit)
	if errors.Is(err, os.ErrNotExist) {
		return contextRequest{}, false, nil
	}
	if err != nil {
		return contextRequest{}, false, err
	}
	if request.Key.Hash() != hash || validOpenKey(request.Key) != nil || !validContextID(request.ContextID) {
		return contextRequest{}, false, fmt.Errorf("execution: damaged context receipt %s", hash)
	}
	return request, true, nil
}

func (j *Journal) replayOpen(k Key, callerDigest string, request contextRequest) (Context, error) {
	if request.Key != k || request.IntentDigest != callerDigest {
		return Context{}, ErrConflict
	}
	if _, err := j.settleRequest(request); err != nil {
		return Context{}, err
	}
	return openResult(request), nil
}

// settleRequest brings one receipt and its record to a consistent state: an
// expiry decision is finished, a record lost after its receipt is completed,
// and an idle record past its expiry is expired. It returns the live record.
func (j *Journal) settleRequest(request contextRequest) (Context, error) {
	if request.Expired {
		return Context{}, errors.Join(ErrContextNotFound, j.finishExpiry(request))
	}
	c, err := j.readContext(request.ContextID)
	if errors.Is(err, os.ErrNotExist) {
		return j.saveContext(newRecord(request, j.now()))
	}
	if err != nil {
		return Context{}, err
	}
	c = j.withMembers(c)
	if j.idle(c) {
		return Context{}, errors.Join(ErrContextNotFound, j.expire(request))
	}
	return c, nil
}

// openResult is the result the first open returned.
func openResult(request contextRequest) Context {
	c := newRecord(request, request.AcceptedAt)
	return c
}

func newRecord(request contextRequest, now time.Time) Context {
	s := request.Spec
	return Context{ID: request.ContextID, Host: request.Key.Host, Owner: request.Key.Principal, Key: request.Key, IntentDigest: request.IntentDigest, Cwd: s.Cwd, Shell: s.Shell, ShellPath: s.ShellPath, Env: s.Env, Label: s.Label, Revision: 1, CreatedAt: request.AcceptedAt, LastUsedAt: now.UTC()}
}

func (j *Journal) acceptContext(k Key, callerDigest string, resolve func() (ContextSpec, error)) (Context, bool, error) {
	if err := j.contextCapacity(k.Principal); err != nil {
		return Context{}, false, err
	}
	spec, err := resolve()
	if err != nil {
		return Context{}, false, err
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return Context{}, false, executionBoundaryError("open context", err)
	}
	request := contextRequest{Key: k, IntentDigest: callerDigest, ContextID: hex.EncodeToString(raw[:]), Spec: spec, AcceptedAt: j.now().UTC()}
	// The receipt is the acceptance point and counts against quota from here.
	if _, err := j.files.PublishBounded(contextRequestName(k.Hash()), request, true, contextRecordLimit); err != nil {
		return Context{}, false, err
	}
	if _, err := j.saveContext(newRecord(request, request.AcceptedAt)); err != nil {
		return Context{}, false, err
	}
	return openResult(request), true, nil
}

// contextCapacity counts accepted receipts, not records, so an open whose
// record write was interrupted still holds its slot. Expired slots are
// reclaimed first.
func (j *Journal) contextCapacity(owner string) error {
	entries, err := j.files.Names(contextRequestDir)
	if err != nil {
		return err
	}
	total, host, mine := 0, 0, 0
	for _, entry := range entries {
		open, retained, principal := j.requestSlot(entry.Name())
		if !retained {
			continue
		}
		total++
		if !open {
			continue
		}
		host++
		if SameOwner(principal, owner) {
			mine++
		}
	}
	if total >= j.contextRetainLimit || host >= j.contextHostLimit || mine >= j.contextOwnerLimit {
		return ErrContextLimit
	}
	return nil
}

// requestSlot reports whether a receipt still holds a slot and whether that
// slot is an open context. Unreadable receipts hold an open slot, so damage
// never frees capacity.
func (j *Journal) requestSlot(name string) (open, retained bool, principal string) {
	hash, ok := cutJSON(name)
	if !ok || !validDigest(hash) {
		return false, false, ""
	}
	request, found, err := j.readRequest(hash)
	if err != nil {
		return true, true, ""
	}
	if !found {
		return false, false, ""
	}
	c, err := j.settleRequest(request)
	if errors.Is(err, ErrContextNotFound) {
		return false, false, ""
	}
	if err != nil {
		return true, true, request.Key.Principal
	}
	return c.ClosedAt == nil, true, request.Key.Principal
}

func (j *Journal) readContext(id string) (Context, error) {
	if !validContextID(id) {
		return Context{}, ErrContextNotFound
	}
	var c Context
	if err := j.files.ReadBounded(contextName(id), &c, contextRecordLimit); err != nil {
		return Context{}, err
	}
	if c.ID != id || c.Revision == 0 || validOpenKey(c.Key) != nil {
		return Context{}, fmt.Errorf("execution: damaged context %s", id)
	}
	return c, nil
}

func (j *Journal) saveContext(c Context) (Context, error) {
	_, err := j.files.PublishBounded(contextName(c.ID), c, false, contextRecordLimit)
	return c, err
}

// ownedContext reads a context for its owner under the journal lock. The
// owner check comes first, so another caller's request causes no effect.
func (j *Journal) ownedContext(owner, id string) (Context, error) {
	c, err := j.readContext(id)
	if errors.Is(err, os.ErrNotExist) {
		return Context{}, ErrContextNotFound
	}
	if err != nil {
		return Context{}, err
	}
	if !SameOwner(c.Owner, owner) {
		return Context{}, ErrContextNotFound
	}
	request, found, err := j.readRequest(c.Key.Hash())
	if err != nil {
		return Context{}, err
	}
	if !found {
		// Receipts are written first and removed last; a record without one
		// is a purge that stopped between the two.
		return Context{}, errors.Join(ErrContextNotFound, j.files.Remove(contextName(id), journalAnchor))
	}
	return j.settleRequest(request)
}

// withMembers lists the context's jobs whose receipts are not terminal.
// Markers are written before a job receipt becomes durable, so a failed
// context write never hides a running job. Markers for terminal or never
// accepted jobs are pruned.
func (j *Journal) withMembers(c Context) Context {
	c.Members = nil
	entries, err := j.files.Names(contextMembers(c.ID))
	if err != nil {
		return c
	}
	for _, entry := range entries {
		hash, ok := cutJSON(entry.Name())
		if !ok || !validDigest(hash) {
			continue
		}
		r, err := j.read(hash)
		if errors.Is(err, os.ErrNotExist) || (err == nil && r.Key.Context != c.ID) {
			_ = j.files.Remove(contextMember(c.ID, hash), journalAnchor)
			continue
		}
		if err == nil && r.Phase == phaseTerminal {
			c = j.pruneFinishedMember(c, hash, r)
			continue
		}
		// A damaged receipt counts as active, so damage never expires a context.
		c.Members = append(c.Members, r.Key)
	}
	return c
}

// pruneFinishedMember removes a finished job's marker once the context's
// last use covers the job's start. A job start records use only at the
// context's granularity, and that write can fail, so until the record holds
// the receipt's durable acceptance time the marker is the only evidence of
// that use: a record that cannot be saved keeps the marker and counts the
// job as active, which defers expiry instead of ending it early.
func (j *Journal) pruneFinishedMember(c Context, hash string, r Receipt) Context {
	if c.LastUsedAt.Before(r.AcceptedAt) {
		used := c
		used.LastUsedAt = r.AcceptedAt.UTC()
		saved, err := j.saveContext(used)
		if err != nil {
			c.Members = append(c.Members, r.Key)
			return c
		}
		c = saved
	}
	_ = j.files.Remove(contextMember(c.ID, hash), journalAnchor)
	return c
}

func (j *Journal) idle(c Context) bool {
	return len(c.Members) == 0 && !j.now().Before(c.ExpiresAt())
}

// expire records the decision before removing anything, so an interrupted
// purge resumes instead of reading as an interrupted open.
func (j *Journal) expire(request contextRequest) error {
	request.Expired = true
	if _, err := j.files.PublishBounded(contextRequestName(request.Key.Hash()), request, false, contextRecordLimit); err != nil {
		return err
	}
	return j.finishExpiry(request)
}

func (j *Journal) finishExpiry(request contextRequest) error {
	err := j.files.Remove(contextName(request.ContextID), journalAnchor)
	if err == nil {
		err = j.files.RemoveTree(contextMembers(request.ContextID), journalAnchor)
	}
	if err == nil || errors.Is(err, os.ErrNotExist) {
		err = j.files.Remove(contextRequestName(request.Key.Hash()), journalAnchor)
	}
	return err
}

// touch records use at most once per granularity, so reads do not write on
// every call.
func (j *Journal) touch(c Context, force bool) (Context, error) {
	now := j.now().UTC()
	if !force && now.Sub(c.LastUsedAt) < contextUseGranularity {
		return c, nil
	}
	c.LastUsedAt = now
	return j.saveContext(c)
}

// GetContext returns the owner's context, closed or open.
func (j *Journal) GetContext(ctx context.Context, owner, id string) (Context, error) {
	gate, err := j.lockAdmitted(ctx)
	if err != nil {
		return Context{}, err
	}
	defer func() { _ = gate.Close() }()
	c, err := j.ownedContext(owner, id)
	if err != nil {
		return Context{}, err
	}
	return j.touch(c, false)
}

// ContextReceipt looks up an open by its caller key, including after the
// context closed. It returns the result the open accepted.
func (j *Journal) ContextReceipt(ctx context.Context, k Key) (Context, error) {
	if err := validOpenKey(k); err != nil {
		return Context{}, err
	}
	if err := j.ensureContextDirs(); err != nil {
		return Context{}, err
	}
	gate, err := j.lockAdmitted(ctx)
	if err != nil {
		return Context{}, err
	}
	defer func() { _ = gate.Close() }()
	request, found, err := j.readRequest(k.Hash())
	if err != nil {
		return Context{}, err
	}
	if !found || request.Key != k {
		return Context{}, ErrNotFound
	}
	if _, err := j.settleRequest(request); err != nil {
		if errors.Is(err, ErrContextNotFound) {
			return Context{}, ErrNotFound
		}
		return Context{}, err
	}
	return openResult(request), nil
}

// ListContexts returns up to limit of the owner's contexts with IDs after
// the cursor, in ID order. more reports that entries remain after the page.
func (j *Journal) ListContexts(ctx context.Context, owner, after string, limit int) ([]Context, bool, error) {
	if err := j.ensureContextDirs(); err != nil {
		return nil, false, err
	}
	gate, err := j.lockAdmitted(ctx)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = gate.Close() }()
	entries, err := j.files.Names(contextDir)
	if err != nil {
		return nil, false, err
	}
	sort.Slice(entries, func(a, b int) bool { return entries[a].Name() < entries[b].Name() })
	out := make([]Context, 0, min(limit, len(entries)))
	for index, entry := range entries {
		id, ok := cutJSON(entry.Name())
		if !ok || !validContextID(id) || id <= after {
			continue
		}
		if len(out) == limit {
			return out, index < len(entries), nil
		}
		if c, err := j.ownedContext(owner, id); err == nil {
			out = append(out, c)
		}
	}
	return out, false, nil
}

// UpdateContext applies a change at the expected revision.
func (j *Journal) UpdateContext(ctx context.Context, owner, id string, expected uint64, change ContextChange, check func(ContextSpec) error) (Context, error) {
	gate, err := j.lockAdmitted(ctx)
	if err != nil {
		return Context{}, err
	}
	defer func() { _ = gate.Close() }()
	c, err := j.ownedContext(owner, id)
	if err != nil {
		return Context{}, err
	}
	if c.ClosedAt != nil {
		return c, ErrContextClosed
	}
	if c.Revision != expected {
		return c, ErrContextConflict
	}
	next := applyChange(c, change)
	if err := check(ContextSpec{Cwd: next.Cwd, Shell: next.Shell, ShellPath: next.ShellPath, Env: next.Env, Label: next.Label}); err != nil {
		return c, err
	}
	next.Revision++
	return j.touch(next, true)
}

func applyChange(c Context, change ContextChange) Context {
	env := make(map[string]string, len(c.Env)+len(change.SetEnv))
	for name, value := range c.Env {
		env[name] = value
	}
	for name, value := range change.SetEnv {
		env[name] = value
	}
	for _, name := range change.UnsetEnv {
		delete(env, name)
	}
	if len(env) == 0 {
		env = nil
	}
	c.Env = env
	if change.Cwd != nil {
		c.Cwd = *change.Cwd
	}
	if change.Label != nil {
		c.Label = *change.Label
	}
	return c
}

// CloseContext stops new jobs. Running jobs keep their handles. Closing a
// closed context returns it unchanged; a stale expected revision conflicts.
func (j *Journal) CloseContext(ctx context.Context, owner, id string, expected *uint64) (Context, error) {
	gate, err := j.lockAdmitted(ctx)
	if err != nil {
		return Context{}, err
	}
	defer func() { _ = gate.Close() }()
	c, err := j.ownedContext(owner, id)
	if err != nil {
		return Context{}, err
	}
	if c.ClosedAt != nil {
		return c, nil
	}
	if expected != nil && *expected != c.Revision {
		return c, ErrContextConflict
	}
	now := j.now().UTC()
	c.ClosedAt = &now
	c.Revision++
	return j.touch(c, true)
}

// MaintainContexts reclaims expired contexts. Jobs of idle-looking contexts
// are observed outside the lock first, so a finished job no longer protects
// its context only because nothing read its receipt since it ended.
func (j *Journal) MaintainContexts(ctx context.Context) error {
	if err := j.ensureContextDirs(); err != nil {
		return err
	}
	due, err := j.dueMembers(ctx)
	if err != nil {
		return err
	}
	for _, k := range due {
		_, _ = j.observe(ctx, k.Hash())
	}
	gate, err := j.lock(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = gate.Close() }()
	entries, err := j.files.Names(contextRequestDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		j.requestSlot(entry.Name())
	}
	return nil
}

// dueMembers lists the active jobs of contexts past their idle deadline.
func (j *Journal) dueMembers(ctx context.Context) ([]Key, error) {
	gate, err := j.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = gate.Close() }()
	entries, err := j.files.Names(contextDir)
	if err != nil {
		return nil, err
	}
	var due []Key
	for _, entry := range entries {
		id, ok := cutJSON(entry.Name())
		c, err := j.readContext(id)
		if !ok || err != nil || j.now().Before(c.ExpiresAt()) {
			continue
		}
		due = append(due, j.withMembers(c).Members...)
	}
	return due, nil
}
