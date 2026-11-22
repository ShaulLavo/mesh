package mcphost

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/shaul/mesh/internal/mcpwire"
)

// Forward routes one validated request from caller to its instance, starting
// the instance on demand. A listen holds the instance only once the upstream
// acknowledged it.
func (s *Supervisor) Forward(ctx context.Context, caller Caller, target Target, request mcpwire.Message, emit Emit) error {
	if !caller.current() {
		return errRevoked
	}
	inst, d, err := s.pick(ctx, caller, target)
	if err != nil {
		return err
	}
	gen, err := s.acquire(ctx, inst)
	if err != nil {
		return err
	}
	done, err := s.admit(gen, request.Method() == mcpwire.MethodListen)
	if err != nil {
		s.release(inst, gen)
		return err
	}
	defer done()
	held := true
	if request.Method() == mcpwire.MethodListen {
		s.release(inst, gen)
		held = false
		inner := emit
		emit = func(raw []byte, terminal bool) error {
			if !held && !terminal {
				s.hold(inst, gen)
				held = true
			}
			return inner(raw, terminal)
		}
	}
	defer func() {
		if held {
			s.release(inst, gen)
		}
	}()
	route := Route{Caller: caller, Service: d.ID, Revision: gen.revision, Account: d.AccountLabel, Instance: inst.rec.ID, Generation: strconv.FormatInt(gen.number, 10), Upstream: gen.upstream, ServerInfo: gen.serverInfo}
	emitted := false
	counted := func(raw []byte, terminal bool) error {
		emitted = true
		return emit(raw, terminal)
	}
	err = s.router.Forward(ctx, route, request, counted)
	if !retryableHeaderMismatch(err, emitted, request) {
		return err
	}
	// The server rejected headers built from a stale schema. Refresh it once
	// in the same revision, generation and account scope, then retry.
	if refreshErr := s.refreshTools(ctx, gen); refreshErr != nil {
		return errors.Join(err, refreshErr)
	}
	return s.router.Forward(WithSchemaRefreshed(ctx), route, request, emit)
}

// retryableHeaderMismatch allows the single schema-refresh retry only when
// nothing reached the caller and no continuation was consumed.
func retryableHeaderMismatch(err error, emitted bool, request mcpwire.Message) bool {
	return errors.Is(err, ErrHeaderMismatch) && !emitted && member(request.Params(), requestStateKey) == nil
}

// Section 2.5: at most 16 ordinary requests and 16 upstream listens per
// instance, counted separately so listens never take request slots.
const (
	maxInstanceRequests = 16
	maxInstanceListens  = 16
)

func (s *Supervisor) admit(gen *generation, listen bool) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	counter, limit, what := &gen.requests, maxInstanceRequests, "requests"
	if listen {
		counter, limit, what = &gen.listens, maxInstanceListens, "listens"
	}
	if *counter >= limit {
		return nil, failure(codeResourceLimit, true, "the instance already has %d %s in progress", limit, what)
	}
	*counter++
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		*counter--
	}, nil
}

func (s *Supervisor) hold(inst *instance, gen *generation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	gen.active++
	stopTimer(inst.idle)
}

// InstanceView is an instance as one caller may see it. It never carries
// stderr, environment values or secret paths.
type InstanceView struct {
	ID          string          `json:"id"`
	Shared      bool            `json:"shared"`
	Mine        bool            `json:"mine"`
	Default     bool            `json:"default,omitempty"`
	State       string          `json:"state"`
	Reason      string          `json:"reason,omitempty"`
	ExitCode    *int            `json:"exitCode,omitempty"`
	Revision    int64           `json:"revision"`
	Generation  int64           `json:"generation"`
	Active      int             `json:"active"`
	Leases      int             `json:"leases"`
	ServerInfo  json.RawMessage `json:"serverInfo,omitempty"`
	LateReplies uint64          `json:"lateReplies,omitempty"`
	UpdatedAt   time.Time       `json:"updatedAt"`
}

// ServiceView is a definition with secrets reduced to their names.
type ServiceView struct {
	ID             string         `json:"id"`
	Name           string         `json:"name"`
	Revision       int64          `json:"revision"`
	Transport      string         `json:"transport"`
	Command        []string       `json:"command,omitempty"`
	Cwd            string         `json:"cwd,omitempty"`
	URL            string         `json:"url,omitempty"`
	Lifecycle      string         `json:"lifecycle"`
	IdleSeconds    int64          `json:"idleSeconds,omitempty"`
	ReadySeconds   int64          `json:"readySeconds"`
	InstancePolicy string         `json:"instancePolicy"`
	AccountMode    string         `json:"accountMode"`
	AccountLabel   string         `json:"accountLabel,omitempty"`
	Disabled       bool           `json:"disabled,omitempty"`
	EnvNames       []string       `json:"env,omitempty"`
	SecretNames    []string       `json:"secretEnv,omitempty"`
	HeaderNames    []string       `json:"secretHeaders,omitempty"`
	Instances      []InstanceView `json:"instances"`
}

// Services lists every definition with the instances caller may see.
// Command lines are included only for administrators.
func (s *Supervisor) Services(caller Caller) []ServiceView {
	s.mu.Lock()
	defer s.mu.Unlock()
	views := make([]ServiceView, 0, len(s.services))
	for _, d := range s.services {
		views = append(views, s.viewLocked(caller, d))
	}
	slices.SortFunc(views, func(a, b ServiceView) int { return compareStrings(a.Name, b.Name) })
	return views
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// Service shows one definition.
func (s *Supervisor) Service(caller Caller, ref string) (ServiceView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.serviceLocked(ref)
	if err != nil {
		return ServiceView{}, err
	}
	return s.viewLocked(caller, d), nil
}

func (s *Supervisor) viewLocked(caller Caller, d Definition) ServiceView {
	v := ServiceView{ID: d.ID, Name: d.Name, Revision: d.Revision, Transport: d.Transport, URL: d.URL, Lifecycle: d.Lifecycle,
		IdleSeconds: int64(d.Idle / time.Second), ReadySeconds: int64(d.ReadyTimeout / time.Second), InstancePolicy: d.InstancePolicy,
		AccountMode: d.AccountMode, AccountLabel: d.AccountLabel, Disabled: d.Disabled,
		EnvNames: slices.Sorted(maps.Keys(d.Env)), SecretNames: slices.Sorted(maps.Keys(d.SecretEnv)), HeaderNames: slices.Sorted(maps.Keys(d.HeaderSecrets)),
		Instances: []InstanceView{}}
	if caller.Admin {
		v.Command, v.Cwd = d.Argv, d.Cwd
	}
	for _, inst := range s.instances {
		if inst.rec.ServiceID != d.ID || !visible(caller, inst) {
			continue
		}
		v.Instances = append(v.Instances, s.instanceViewLocked(caller, inst))
	}
	slices.SortFunc(v.Instances, func(a, b InstanceView) int { return compareStrings(a.ID, b.ID) })
	return v
}

func visible(caller Caller, inst *instance) bool {
	return caller.Admin || inst.rec.Owner == "" || caller.owns(inst.rec.Owner)
}

func (s *Supervisor) instanceViewLocked(caller Caller, inst *instance) InstanceView {
	r := inst.rec
	v := InstanceView{ID: r.ID, Shared: r.Owner == "", Mine: r.Owner != "" && caller.owns(r.Owner), Default: r.Default, State: r.State, Reason: r.Reason,
		ExitCode: r.ExitCode, Revision: r.Revision, Generation: r.Generation, Leases: len(r.Leases), UpdatedAt: r.UpdatedAt}
	if gen := inst.current; gen != nil {
		v.Active, v.ServerInfo = gen.active, gen.serverInfo
		if stdio, ok := gen.upstream.(*StdioUpstream); ok {
			v.LateReplies = stdio.LateReplies()
		}
	}
	return v
}

// manageable returns the instances an operation acts on. An administrator
// naming no instance acts on all of them.
func (s *Supervisor) manageable(ctx context.Context, caller Caller, target Target) ([]*instance, error) {
	if target.Instance == "" && caller.Admin {
		d, err := s.service(target.Service)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		var all []*instance
		for _, inst := range s.instances {
			if inst.rec.ServiceID == d.ID {
				all = append(all, inst)
			}
		}
		return all, nil
	}
	if caller.Admin {
		return s.adminInstance(target)
	}
	inst, _, err := s.pick(ctx, caller, target)
	if err != nil {
		return nil, err
	}
	return []*instance{inst}, nil
}

func (s *Supervisor) adminInstance(target Target) ([]*instance, error) {
	d, err := s.service(target.Service)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	inst := s.instances[target.Instance]
	if inst == nil || inst.rec.ServiceID != d.ID {
		return nil, fmt.Errorf("instance %s of %s: %w", target.Instance, d.Name, ErrNotFound)
	}
	return []*instance{inst}, nil
}

// StartInstance starts the selected instance now, clearing a recorded failure.
func (s *Supervisor) StartInstance(ctx context.Context, caller Caller, target Target) (InstanceView, error) {
	if caller.Admin && target.Instance == "" {
		caller.Admin = false
	}
	instances, err := s.manageable(ctx, caller, target)
	if err != nil {
		return InstanceView{}, err
	}
	inst := instances[0]
	s.resettle(ctx, inst)
	gen, err := s.acquire(ctx, inst)
	if err != nil {
		return s.viewOf(caller, inst), err
	}
	s.release(inst, gen)
	return s.viewOf(caller, inst), nil
}

// resettle lets an explicit start retry a failed or incompatible instance.
// An uncertain one starts only once recovery can see the previous worker.
func (s *Supervisor) resettle(ctx context.Context, inst *instance) {
	s.mu.Lock()
	state := inst.rec.State
	if state == StateFailed || state == StateIncompatible {
		inst.rec.State = StateStopped
		stopTimer(inst.retry)
		inst.retry, inst.backoff = nil, 0
	}
	s.mu.Unlock()
	if state == StateUncertain && s.claimStart(inst) {
		defer s.finishStart(inst)
		s.recover(ctx, inst)
	}
}

func (s *Supervisor) viewOf(caller Caller, inst *instance) InstanceView {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.instanceViewLocked(caller, inst)
}

// StopInstance stops the selected instances' processes and keeps their
// definitions and identities.
func (s *Supervisor) StopInstance(ctx context.Context, caller Caller, target Target) ([]InstanceView, error) {
	instances, err := s.manageable(ctx, caller, target)
	if err != nil {
		return nil, err
	}
	views := make([]InstanceView, 0, len(instances))
	for _, inst := range instances {
		if err := s.stopInstance(ctx, inst, "stopped on request"); err != nil {
			return views, err
		}
		views = append(views, s.viewOf(caller, inst))
	}
	return views, nil
}

// Restart replaces the selected instance's process with a new generation.
func (s *Supervisor) Restart(ctx context.Context, caller Caller, target Target) (InstanceView, error) {
	if caller.Admin && target.Instance == "" {
		caller.Admin = false
	}
	instances, err := s.manageable(ctx, caller, target)
	if err != nil {
		return InstanceView{}, err
	}
	if err := s.stopInstance(ctx, instances[0], "restarting"); err != nil {
		return InstanceView{}, err
	}
	target.Instance = instances[0].rec.ID
	return s.StartInstance(ctx, caller, target)
}

// Logs returns the bounded private stderr ring of the selected instance.
func (s *Supervisor) Logs(ctx context.Context, caller Caller, target Target, limit int) ([]byte, error) {
	if limit <= 0 || limit > stderrLimit {
		limit = stderrLimit
	}
	if caller.Admin && target.Instance == "" {
		caller.Admin = false
	}
	instances, err := s.manageable(ctx, caller, target)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	ref := instances[0].rec.Worker
	s.mu.Unlock()
	if ref == nil {
		return nil, nil
	}
	out, err := s.cfg.Launcher.Stderr(ctx, *ref, limit)
	if err != nil {
		return nil, fmt.Errorf("read instance %s diagnostics: %w", instances[0].rec.ID, err)
	}
	return out, nil
}

// Lease holds the selected instance for ttl, renewing a lease with the same
// ID. A zero ttl releases it. A lease naming job also ends with that job.
func (s *Supervisor) Lease(ctx context.Context, caller Caller, target Target, leaseID string, ttl time.Duration, job string) (Lease, InstanceView, error) {
	if ttl < 0 || ttl > maxLeaseTTL {
		return Lease{}, InstanceView{}, invalid("lease TTL must be between 0 and 24h")
	}
	if job != "" && !s.jobActive(ctx, caller.Account, caller.App, job) {
		return Lease{}, InstanceView{}, failure(codeNotFound, false, "job %s is not a running job of this caller", job)
	}
	inst, _, err := s.pick(ctx, caller, target)
	if err != nil {
		return Lease{}, InstanceView{}, err
	}
	if leaseID == "" {
		if leaseID, err = randomID(); err != nil {
			return Lease{}, InstanceView{}, err
		}
	}
	lease := Lease{ID: leaseID, Caller: caller.Key(), Account: caller.Account, App: caller.App, Grant: caller.Grant, Job: job, Expires: s.cfg.Now().Add(ttl)}
	if err := s.putLease(ctx, inst, lease, ttl == 0); err != nil {
		return Lease{}, InstanceView{}, err
	}
	if ttl > 0 {
		gen, err := s.acquire(ctx, inst)
		if err != nil {
			return lease, s.viewOf(caller, inst), err
		}
		s.release(inst, gen)
	}
	return lease, s.viewOf(caller, inst), nil
}

func (s *Supervisor) putLease(ctx context.Context, inst *instance, lease Lease, remove bool) error {
	s.mu.Lock()
	index := slices.IndexFunc(inst.rec.Leases, func(l Lease) bool { return l.ID == lease.ID })
	if index >= 0 && subtle.ConstantTimeCompare([]byte(inst.rec.Leases[index].Caller), []byte(lease.Caller)) != 1 {
		s.mu.Unlock()
		return fmt.Errorf("lease %s: %w", lease.ID, ErrDenied)
	}
	switch {
	case index >= 0 && remove:
		inst.rec.Leases = slices.Delete(inst.rec.Leases, index, index+1)
	case index >= 0:
		inst.rec.Leases[index] = lease
	case !remove:
		inst.rec.Leases = append(inst.rec.Leases, lease)
	}
	if d, err := s.serviceLocked(inst.rec.ServiceID); err == nil {
		s.armIdleLocked(inst, d)
	}
	s.mu.Unlock()
	return s.persist(ctx, inst)
}

func (s *Supervisor) leasedLocked(inst *instance) bool {
	now := s.cfg.Now()
	for _, lease := range inst.rec.Leases {
		if now.Before(lease.Expires) {
			return true
		}
	}
	return false
}

func (s *Supervisor) sweepLoop(ctx context.Context) {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.Sweep(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// Sweep expires leases, releases job leases whose job ended, and retires the
// leases, continuations and isolated instances of accounts whose grant was
// revoked. Shared instances and native jobs keep running.
func (s *Supervisor) Sweep(ctx context.Context) {
	s.mu.Lock()
	instances := slices.Collect(maps.Values(s.instances))
	s.mu.Unlock()
	retired := map[string]bool{}
	for _, inst := range instances {
		s.sweepInstance(ctx, inst, retired)
	}
	for account := range retired {
		s.router.continuations.retireAccount(account)
	}
}

func (s *Supervisor) sweepInstance(ctx context.Context, inst *instance, retired map[string]bool) {
	s.mu.Lock()
	owner := inst.rec.OwnerAccount
	leases := slices.Clone(inst.rec.Leases)
	s.mu.Unlock()
	if owner != "" && !s.accountCurrent(owner, retired) {
		_ = s.stopInstance(ctx, inst, "the owning device's access was revoked")
	}
	now := s.cfg.Now()
	kept := slices.DeleteFunc(leases, func(l Lease) bool {
		return !now.Before(l.Expires) || !s.accountCurrent(l.Account, retired) || (l.Job != "" && !s.jobActive(ctx, l.Account, l.App, l.Job))
	})
	s.mu.Lock()
	changed := len(kept) != len(inst.rec.Leases)
	if changed {
		inst.rec.Leases = kept
		if d, err := s.serviceLocked(inst.rec.ServiceID); err == nil {
			s.armIdleLocked(inst, d)
		}
	}
	s.mu.Unlock()
	if changed {
		_ = s.persist(ctx, inst)
	}
}

func (s *Supervisor) jobActive(ctx context.Context, account, app, job string) bool {
	return s.cfg.JobActive != nil && s.cfg.JobActive(ctx, account, app, job)
}

func (s *Supervisor) accountCurrent(account string, retired map[string]bool) bool {
	if retired[account] {
		return false
	}
	if s.cfg.AccountCurrent(account) {
		return true
	}
	retired[account] = true
	return false
}
