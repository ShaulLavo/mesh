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
)

// ContextSpec is the normalized creation intent of a context. The daemon
// validates and normalizes it; the journal stores it unchanged.
type ContextSpec struct {
	Cwd       string            `json:"cwd"`
	Shell     string            `json:"shell"`
	ShellPath string            `json:"shellPath,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Label     string            `json:"label,omitempty"`
}

// Context is one durable context record. Env holds values and never leaves
// the host; callers see names and a digest.
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
	// Jobs lists request hashes of jobs that may still run here. Terminal
	// ones are pruned as the list is read, so it stays short.
	Jobs []string `json:"jobs,omitempty"`
	// RunningJobs is computed on read, never stored.
	RunningJobs int `json:"-"`
}

// ExpiresAt is when an unused context with no running job is purged.
func (c Context) ExpiresAt() time.Time { return c.LastUsedAt.Add(contextIdleExpiry) }

// contextRequest is the caller-keyed open receipt. It holds the full spec,
// so a crash between the receipt and the record still converges to the same
// context on replay.
type contextRequest struct {
	Key          Key         `json:"key"`
	IntentDigest string      `json:"intentDigest"`
	ContextID    string      `json:"contextId"`
	Spec         ContextSpec `json:"spec"`
	AcceptedAt   time.Time   `json:"acceptedAt"`
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
	recordExtension   = ".json"
)

func contextName(id string) string { return filepath.Join(contextDir, id+recordExtension) }
func contextRequestName(hash string) string {
	return filepath.Join(contextRequestDir, hash+recordExtension)
}

func (j *Journal) ensureContextDirs() error {
	return errors.Join(j.files.Ensure(contextDir), j.files.Ensure(contextRequestDir))
}

// OpenContext stores a context under a caller key. The same key and intent
// return the original context; a changed intent fails with ErrConflict.
func (j *Journal) OpenContext(ctx context.Context, k Key, spec ContextSpec) (Context, bool, error) {
	if err := k.Validate(); err != nil || k.Operation != OperationContextOpen {
		return Context{}, false, errors.Join(fmt.Errorf("execution: invalid context open key"), err)
	}
	if err := j.ensureContextDirs(); err != nil {
		return Context{}, false, err
	}
	gate, err := j.lock(ctx)
	if err != nil {
		return Context{}, false, err
	}
	defer func() { _ = gate.Close() }()
	var request contextRequest
	err = j.files.Read(contextRequestName(k.Hash()), &request)
	if err == nil {
		c, err := j.replayOpen(k, spec, request)
		if !errors.Is(err, ErrContextNotFound) {
			return c, false, err
		}
		// The context expired and its receipt went with it: a fresh open.
	} else if !errors.Is(err, os.ErrNotExist) {
		return Context{}, false, err
	}
	return j.acceptContext(ctx, k, spec)
}

func (j *Journal) replayOpen(k Key, spec ContextSpec, request contextRequest) (Context, error) {
	if request.Key != k || request.IntentDigest != digest(spec) {
		return Context{}, ErrConflict
	}
	return j.requestContext(request)
}

// requestContext resolves an open receipt to its record. A record lost
// between the receipt and its first save is completed from the receipt.
func (j *Journal) requestContext(request contextRequest) (Context, error) {
	_, err := j.readContext(request.ContextID)
	if errors.Is(err, os.ErrNotExist) {
		c, err := j.saveContext(newContext(request, j.now()))
		return j.withRunning(c), err
	}
	if err != nil {
		return Context{}, err
	}
	return j.ownedContext(request.Key.Principal, request.ContextID)
}

func newContext(request contextRequest, now time.Time) Context {
	s := request.Spec
	return Context{ID: request.ContextID, Host: request.Key.Host, Owner: request.Key.Principal, Key: request.Key, IntentDigest: request.IntentDigest, Cwd: s.Cwd, Shell: s.Shell, ShellPath: s.ShellPath, Env: s.Env, Label: s.Label, Revision: 1, CreatedAt: request.AcceptedAt, LastUsedAt: now.UTC()}
}

func (j *Journal) acceptContext(ctx context.Context, k Key, spec ContextSpec) (Context, bool, error) {
	if err := j.contextCapacity(k.Principal); err != nil {
		return Context{}, false, err
	}
	if err := ctx.Err(); err != nil {
		return Context{}, false, executionBoundaryError("open context", err)
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return Context{}, false, executionBoundaryError("open context", err)
	}
	request := contextRequest{Key: k, IntentDigest: digest(spec), ContextID: hex.EncodeToString(raw[:]), Spec: spec, AcceptedAt: j.now().UTC()}
	// The receipt is the acceptance point; the record follows it.
	if _, err := j.files.Publish(contextRequestName(k.Hash()), request, true); err != nil {
		return Context{}, false, err
	}
	c, err := j.saveContext(newContext(request, j.now()))
	return c, err == nil, err
}

func (j *Journal) contextCapacity(owner string) error {
	entries, err := j.files.Names(contextDir)
	if err != nil {
		return err
	}
	if len(entries) >= maxRetainedContexts {
		return ErrContextLimit
	}
	host, mine := 0, 0
	for _, entry := range entries {
		c, ok := j.contextEntry(entry.Name())
		if !ok || c.ClosedAt != nil {
			continue
		}
		host++
		if c.Owner == owner {
			mine++
		}
	}
	if host >= maxOpenPerHost || mine >= maxOpenPerOwner {
		return ErrContextLimit
	}
	return nil
}

func (j *Journal) contextEntry(name string) (Context, bool) {
	id, ok := cutJSON(name)
	if !ok {
		return Context{}, false
	}
	c, err := j.readContext(id)
	return c, err == nil
}

func cutJSON(name string) (string, bool) {
	id, ok := strings.CutSuffix(name, recordExtension)
	return id, ok && validContextID(id)
}

func validContextID(id string) bool {
	raw, err := hex.DecodeString(id)
	return err == nil && len(raw) == 16 && hex.EncodeToString(raw) == id
}

func (j *Journal) readContext(id string) (Context, error) {
	if !validContextID(id) {
		return Context{}, ErrContextNotFound
	}
	var c Context
	if err := j.files.Read(contextName(id), &c); err != nil {
		return Context{}, err
	}
	if c.ID != id || c.Revision == 0 || c.Key.Validate() != nil {
		return Context{}, fmt.Errorf("execution: damaged context %s", id)
	}
	return c, nil
}

func (j *Journal) saveContext(c Context) (Context, error) {
	_, err := j.files.Publish(contextName(c.ID), c, false)
	return c, err
}

// ownedContext reads a context for its owner under the journal lock. Expired
// records are purged here, so a stale context never comes back.
func (j *Journal) ownedContext(owner, id string) (Context, error) {
	c, err := j.readContext(id)
	if errors.Is(err, os.ErrNotExist) {
		return Context{}, ErrContextNotFound
	}
	if err != nil {
		return Context{}, err
	}
	if c.Owner != owner {
		return Context{}, ErrContextNotFound
	}
	c = j.withRunning(c)
	if c.RunningJobs == 0 && !j.now().Before(c.ExpiresAt()) {
		return Context{}, errors.Join(ErrContextNotFound, j.purgeContext(c))
	}
	return c, nil
}

// withRunning prunes finished jobs from the list and counts the rest. A job
// reserved before this boot cannot still run.
func (j *Journal) withRunning(c Context) Context {
	kept := c.Jobs[:0:0]
	for _, hash := range c.Jobs {
		r, err := j.read(hash)
		if err != nil || r.Phase == phaseTerminal || r.Binding.Reservation.BootID != j.boot {
			continue
		}
		kept = append(kept, hash)
	}
	c.Jobs, c.RunningJobs = kept, len(kept)
	return c
}

func (j *Journal) purgeContext(c Context) error {
	err := j.files.Remove(contextName(c.ID), "execution/.barrier")
	return errors.Join(err, j.files.Remove(contextRequestName(c.Key.Hash()), "execution/.barrier"))
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
	gate, err := j.lock(ctx)
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

// ContextReceipt looks up an open receipt by its caller key, including after
// the context closed.
func (j *Journal) ContextReceipt(ctx context.Context, k Key) (Context, error) {
	if err := k.Validate(); err != nil || k.Operation != OperationContextOpen {
		return Context{}, errors.Join(fmt.Errorf("execution: invalid context open key"), err)
	}
	gate, err := j.lock(ctx)
	if err != nil {
		return Context{}, err
	}
	defer func() { _ = gate.Close() }()
	var request contextRequest
	err = j.files.Read(contextRequestName(k.Hash()), &request)
	if errors.Is(err, os.ErrNotExist) {
		return Context{}, ErrNotFound
	}
	if err != nil {
		return Context{}, err
	}
	if request.Key != k {
		return Context{}, ErrNotFound
	}
	c, err := j.requestContext(request)
	if errors.Is(err, ErrContextNotFound) {
		return Context{}, ErrNotFound
	}
	return c, err
}

// ListContexts returns the owner's retained contexts, newest first.
func (j *Journal) ListContexts(ctx context.Context, owner string) ([]Context, error) {
	if err := j.ensureContextDirs(); err != nil {
		return nil, err
	}
	gate, err := j.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = gate.Close() }()
	entries, err := j.files.Names(contextDir)
	if err != nil {
		return nil, err
	}
	out := make([]Context, 0)
	for _, entry := range entries {
		id, ok := cutJSON(entry.Name())
		if !ok {
			continue
		}
		c, err := j.ownedContext(owner, id)
		if err == nil {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].CreatedAt.After(out[b].CreatedAt) })
	return out, nil
}

// UpdateContext applies a change at the expected revision.
func (j *Journal) UpdateContext(ctx context.Context, owner, id string, expected uint64, change ContextChange, check func(ContextSpec) error) (Context, error) {
	gate, err := j.lock(ctx)
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
	gate, err := j.lock(ctx)
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

// ReserveInContext reserves a job against the current revision of an open
// context, all under the journal lock. An existing receipt for the key is
// returned before the context is read, so replays survive later edits and
// closure. callerDigest covers the caller-supplied request; resolve turns the
// context and that request into the launch intent.
func (j *Journal) ReserveInContext(ctx context.Context, k Key, callerDigest string, resolve func(Context) (Intent, error), reserve func(Intent) (Binding, error)) (Receipt, bool, error) {
	if err := k.Validate(); err != nil || k.Operation != OperationExec || !validDigest(callerDigest) {
		return Receipt{}, false, errors.Join(fmt.Errorf("execution: invalid context job key"), err)
	}
	if err := j.prepareOutputAdmission(ctx, k); err != nil {
		return Receipt{}, false, err
	}
	r, owner, err := j.reserveInContext(ctx, k, callerDigest, resolve, reserve)
	if err != nil || owner {
		return r, owner, err
	}
	r, err = j.refresh(ctx, r)
	if err == nil {
		err = receiptDiagnostic(r)
	}
	return r, false, err
}

func (j *Journal) reserveInContext(ctx context.Context, k Key, callerDigest string, resolve func(Context) (Intent, error), reserve func(Intent) (Binding, error)) (Receipt, bool, error) {
	gate, err := j.lock(ctx)
	if err != nil {
		return Receipt{}, false, err
	}
	defer func() { _ = gate.Close() }()
	r, err := j.read(k.Hash())
	if err == nil {
		if r.Key != k || r.IntentDigest != callerDigest {
			return Receipt{}, false, ErrConflict
		}
		return r, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return r, false, err
	}
	c, err := j.ownedContext(k.Principal, k.Context)
	if err != nil {
		return Receipt{}, false, err
	}
	if c.ClosedAt != nil {
		return Receipt{}, false, ErrContextClosed
	}
	intent, err := resolve(c)
	if err != nil {
		return Receipt{}, false, err
	}
	normalized, err := Normalize(intent)
	if err != nil {
		return Receipt{}, false, err
	}
	use := ContextUse{ID: c.ID, Revision: c.Revision}
	decorate := func(r *Receipt) {
		r.IntentDigest, r.LaunchIntentDigest, r.Context = callerDigest, digest(normalized), &use
	}
	r, owner, err := j.acceptNew(ctx, k, normalized, func() (Binding, error) { return reserve(normalized) }, decorate)
	if err != nil || !owner {
		return r, owner, err
	}
	// The receipt is already durable and this caller is its sole launcher.
	// Failing to note the job on the context only risks early expiry of an
	// idle-looking record, which is better than an unlaunched reservation.
	c.Jobs = append(c.Jobs, r.Binding.RequestHash)
	_, _ = j.touch(c, true)
	return r, true, nil
}

// MaintainContexts purges expired contexts in one bounded pass.
func (j *Journal) MaintainContexts(ctx context.Context) error {
	if err := j.ensureContextDirs(); err != nil {
		return err
	}
	gate, err := j.lock(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = gate.Close() }()
	entries, err := j.files.Names(contextDir)
	if err != nil {
		return err
	}
	var failures []error
	for _, entry := range entries {
		c, ok := j.contextEntry(entry.Name())
		if !ok {
			continue
		}
		// ownedContext purges an expired record as a side effect.
		if _, err := j.ownedContext(c.Owner, c.ID); err != nil && !errors.Is(err, ErrContextNotFound) {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
