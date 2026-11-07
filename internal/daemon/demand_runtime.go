package daemon

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
	meshserve "github.com/shaul/mesh/internal/serve"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/storage"
)

type serviceRuntimeStore interface {
	ListServiceRuntime(context.Context) ([]storage.ServiceRuntime, error)
	GetServiceRuntime(context.Context, string) (storage.ServiceRuntime, error)
	CompareServiceRuntime(context.Context, *storage.ServiceRuntime, *storage.ServiceRuntime) error
}

type managedDemandSessions interface {
	reserveLabelled(context.Context) (session.Reservation, error)
	startReserved(context.Context, session.Reservation, meshserve.Service) error
	ownedFate(session.Reservation) ownedObservation
	stopOwned(context.Context, session.Reservation) error
}

type runtimeIntent struct {
	before, next *storage.ServiceRuntime
	done         chan struct{}
	uncertain    bool
	definition   bool
	finished     bool
	apply        func()
}

// The acceptance mutex protects only intent admission and brief snapshots. An
// admitted intent excludes later transitions while SQLite runs without the mutex.
type demandPersistence struct {
	mu                sync.Mutex
	pending           *runtimeIntent
	store             serviceRuntimeStore
	records           map[string]storage.ServiceRuntime
	phases            sync.WaitGroup
	sessions          managedDemandSessions
	definitionUnknown bool
}

func (m *demandManager) restoreRuntime(store serviceRuntimeStore) error {
	sessions, ok := m.sessions.(managedDemandSessions)
	if !ok {
		return fmt.Errorf("daemon: exact managed session lifecycle is unavailable")
	}
	records, err := store.ListServiceRuntime(m.ctx)
	if err != nil {
		return fmt.Errorf("daemon: restore service runtime: %w", err)
	}
	if err := confirmRestoredRuntime(m.ctx, store, records); err != nil {
		return err
	}
	p := &demandPersistence{store: store, records: make(map[string]storage.ServiceRuntime), sessions: sessions}
	for _, record := range records {
		p.records[record.Name] = record
	}
	m.persistence = p
	return nil
}

func confirmRestoredRuntime(parent context.Context, store serviceRuntimeStore, records []storage.ServiceRuntime) error {
	ctx, cancel := context.WithTimeout(parent, serviceReconcileTimeout)
	defer cancel()
	for _, record := range records {
		if err := store.CompareServiceRuntime(ctx, &record, &record); err != nil {
			return fmt.Errorf("daemon: confirm restored service runtime %s: %w", record.Name, err)
		}
	}
	return nil
}

func runtimeToken() string {
	var token [16]byte
	_, _ = rand.Read(token[:])
	return hex.EncodeToString(token[:])
}

func (m *demandManager) beginAcceptance(ctx context.Context, definition bool) (*runtimeIntent, error) {
	p := m.persistence
	for {
		p.mu.Lock()
		if m.closed.Load() || p.definitionUnknown && !definition {
			p.mu.Unlock()
			return nil, fmt.Errorf("daemon: service runtime is shut down")
		}
		if p.pending == nil {
			intent := &runtimeIntent{done: make(chan struct{})}
			p.pending = intent
			intent.definition = definition
			p.phases.Add(1)
			p.mu.Unlock()
			return intent, nil
		}
		pending := p.pending
		uncertain := pending.uncertain
		done := pending.done
		p.mu.Unlock()
		if uncertain {
			return nil, fmt.Errorf("daemon: service runtime persistence is uncertain")
		}
		select {
		case <-done:
		case <-ctx.Done():
			return nil, fmt.Errorf("daemon: wait for service acceptance: %w", ctx.Err())
		}
	}
}

func (m *demandManager) endAcceptance(intent *runtimeIntent, uncertain bool) {
	p := m.persistence
	p.mu.Lock()
	intent.uncertain = uncertain
	intent.finished = true
	intent.definition = false
	if !uncertain {
		p.pending = nil
	}
	close(intent.done)
	p.phases.Done()
	p.mu.Unlock()
}

func (m *demandManager) BeginDefinition(ctx context.Context) (func(), error) {
	if m.persistence == nil {
		return func() {}, nil
	}
	intent, err := m.beginAcceptance(ctx, true)
	if err != nil {
		return nil, err
	}
	return func() {
		m.persistence.mu.Lock()
		finished := intent.finished
		m.persistence.mu.Unlock()
		if !finished {
			m.endAcceptance(intent, false)
		}
	}, nil
}

func (m *demandManager) currentRuntimeRoute(r *demandRoute) bool {
	name := r.runtimeName()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.routes[name] == r || m.retiring[name] == r
}

func (r *demandRoute) runtimeName() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.service.Name
}

func cloneRuntime(record storage.ServiceRuntime) storage.ServiceRuntime {
	if record.Attempt != nil {
		attempt := *record.Attempt
		attempt.Launched = cloneRuntimeDefinition(attempt.Launched)
		record.Attempt = &attempt
	}
	if record.Definition != nil {
		definition := cloneRuntimeDefinition(*record.Definition)
		record.Definition = &definition
	}
	return record
}

func cloneRuntimeDefinition(service meshserve.Service) meshserve.Service {
	service.Listens = slices.Clone(service.Listens)
	if service.Demand != nil {
		recipe := *service.Demand
		recipe.Env = slices.Clone(recipe.Env)
		service.Demand = &recipe
	}
	return service
}

func (r *demandRoute) changeRuntime(change func(*storage.ServiceRuntime) error, apply func()) error {
	m := r.manager
	if m.persistence == nil {
		if apply != nil {
			apply()
		}
		return nil
	}
	intent, err := m.beginAcceptance(m.ctx, false)
	if err != nil {
		return err
	}
	if !m.currentRuntimeRoute(r) {
		m.endAcceptance(intent, false)
		return fmt.Errorf("daemon: stale service route")
	}
	p := m.persistence
	record, exists := p.records[r.runtimeName()]
	if exists {
		before := cloneRuntime(record)
		intent.before = &before
	}
	if !exists {
		definition := cloneRuntimeDefinition(r.definition())
		record = storage.ServiceRuntime{Name: definition.Name, Definition: &definition, Generation: runtimeToken(), Phase: storage.ServiceNone}
	}
	next := cloneRuntime(record)
	if err := change(&next); err != nil {
		m.endAcceptance(intent, false)
		return err
	}
	if exists && record.Equal(next) {
		if apply != nil {
			apply()
		}
		m.endAcceptance(intent, false)
		return nil
	}
	next.Version++
	intent.next, intent.apply = &next, apply
	return m.persistIntent(m.ctx, intent)
}

func (m *demandManager) persistIntent(parent context.Context, intent *runtimeIntent) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), serviceReconcileTimeout)
	defer cancel()
	err := m.persistence.store.CompareServiceRuntime(ctx, intent.before, intent.next)
	if err != nil {
		err = m.confirmIntent(ctx, intent, err)
	}
	if err != nil {
		m.endAcceptance(intent, true)
		m.report(fmt.Errorf("daemon: service runtime remains uncertain: %w", err))
		return err
	}
	p := m.persistence
	name := ""
	if intent.before != nil {
		name = intent.before.Name
	}
	p.mu.Lock()
	if intent.next != nil {
		name = intent.next.Name
		p.records[name] = *intent.next
	} else {
		delete(p.records, name)
	}
	p.mu.Unlock()
	if intent.apply != nil {
		intent.apply()
	}
	if !intent.definition {
		m.endAcceptance(intent, false)
	}
	return nil
}

func (m *demandManager) confirmIntent(ctx context.Context, intent *runtimeIntent, cause error) error {
	name := ""
	if intent.before != nil {
		name = intent.before.Name
	}
	if intent.next != nil {
		name = intent.next.Name
	}
	observed, err := m.persistence.store.GetServiceRuntime(ctx, name)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return errors.Join(cause, err)
	}
	matchesOld := err == nil && intent.before != nil && observed.Equal(*intent.before)
	matchesNew := err == nil && intent.next != nil && observed.Equal(*intent.next)
	absent := errors.Is(err, sql.ErrNoRows) && (intent.before == nil || intent.next == nil)
	if !matchesOld && !matchesNew && !absent {
		return errors.Join(cause, storage.ErrServiceRuntimeConflict)
	}
	// Readback alone never acknowledges a failed durability barrier.
	if err := m.persistence.store.CompareServiceRuntime(ctx, intent.before, intent.next); err != nil {
		return fmt.Errorf("daemon: runtime confirmation: %w", err)
	}
	return nil
}

func (m *demandManager) reconcileRuntime() {
	p := m.persistence
	if p == nil || m.closed.Load() {
		return
	}
	p.mu.Lock()
	intent := p.pending
	if m.closed.Load() || intent == nil || !intent.uncertain {
		p.mu.Unlock()
		return
	}
	intent.uncertain = false
	intent.finished = false
	intent.done = make(chan struct{})
	p.phases.Add(1)
	p.mu.Unlock()
	_ = m.persistIntent(m.ctx, intent)
}

func (r *demandRoute) runtimeControl(stop bool, apply func()) error {
	return r.changeRuntime(func(record *storage.ServiceRuntime) error {
		r.mu.Lock()
		unknown := r.owned == ownsUnknown
		unavailable := r.unavailableLocked()
		r.mu.Unlock()
		if unavailable != nil {
			return unavailable
		}
		if record.Version == 0 && unknown {
			return fmt.Errorf("daemon: prior service launch is unresolved")
		}
		if stop && record.Attempt != nil {
			oweRuntimeStop(record, causeStopRequested)
		}
		if !stop {
			record.Failures = 0
			record.RestartAt = time.Time{}
			record.Restarts = 0
		}
		return nil
	}, apply)
}

func oweRuntimeStop(record *storage.ServiceRuntime, cause demandCause) {
	if record.Attempt == nil {
		return
	}
	if record.Phase != storage.ServiceCleanup {
		record.Attempt.StopToken = runtimeToken()
		record.Attempt.StopCause = string(cause)
	}
	record.Phase = storage.ServiceCleanup
}

func (r *demandRoute) runtimeRecord() (storage.ServiceRuntime, bool) {
	p := r.manager.persistence
	p.mu.Lock()
	defer p.mu.Unlock()
	record, ok := p.records[r.runtimeName()]
	return cloneRuntime(record), ok
}

func (r *demandRoute) prepareRuntimeStart(transition *demandTransition, service meshserve.Service, cause demandCause) (session.Reservation, error) {
	var reference session.Reservation
	err := r.changeRuntime(func(record *storage.ServiceRuntime) error {
		r.mu.Lock()
		allowed := r.pending == transition && !r.removed && !r.stopRequested && sameLaunch(r.service.Demand, service.Demand) && r.service.Label() == service.Label()
		r.mu.Unlock()
		if !allowed || record.Attempt != nil {
			return fmt.Errorf("daemon: launch superseded or still owns an attempt")
		}
		var err error
		reference, err = r.manager.persistence.sessions.reserveLabelled(r.manager.ctx)
		if err != nil {
			return err
		}
		if err := reference.Validate(); err != nil {
			return fmt.Errorf("daemon: invalid managed reference: %w", err)
		}
		now := r.manager.now()
		record.Phase = storage.ServiceStarting
		record.Attempt = &storage.ServiceAttempt{Reference: reference, Launched: cloneRuntimeDefinition(service), StartedAt: now, Deadline: now.Add(service.Demand.ReadyTimeout)}
		if cause == causeKeptRestart {
			record.Restarts++
		}
		return nil
	}, nil)
	return reference, err
}

func (r *demandRoute) runtimePhase(ref session.Reservation, phase storage.ServicePhase) error {
	return r.changeRuntime(func(record *storage.ServiceRuntime) error {
		if record.Attempt == nil || record.Attempt.Reference != ref {
			return fmt.Errorf("daemon: stale managed attempt")
		}
		if record.Phase == storage.ServiceCleanup {
			return fmt.Errorf("daemon: managed attempt owes cleanup")
		}
		record.Phase = phase
		if phase == storage.ServiceReady {
			record.Attempt.ReadyAt = r.manager.now()
		}
		return nil
	}, nil)
}

func (r *demandRoute) runRuntimeStart(transition *demandTransition, service meshserve.Service, cause demandCause) {
	m := r.manager
	defer m.goroutines.Done()
	ref, err := r.prepareRuntimeStart(transition, service, cause)
	if err != nil {
		m.finishPhase()
		r.runtimeFailedAdmission()
		r.finishStart(transition, "", err, cause)
		return
	}
	err = m.persistence.sessions.startReserved(m.ctx, ref, service)
	m.finishPhase()
	if errors.As(err, new(publicationError)) {
		m.report(err)
		err = nil
	}
	if err != nil {
		var noStart *recovery.LaunchFailure
		if errors.As(err, &noStart) {
			_ = r.consumeRuntime(ref, ownedObservation{fate: sessionEnded}, true)
			err = demandFailure{summary: err.Error(), message: err.Error(), ended: true}
		}
	} else {
		err = r.runtimePhase(ref, storage.ServiceReadiness)
		if err == nil {
			err = r.awaitRuntimeReady(service, ref)
		}
	}
	if err == nil {
		err = r.runtimePhase(ref, storage.ServiceReady)
	}
	r.finishStart(transition, ref.ID, err, cause)
	if err == nil {
		m.sessions.pruneLabelled(m.ctx, service.Label(), ref.ID)
	}
}

func (r *demandRoute) awaitRuntimeReady(service meshserve.Service, ref session.Reservation) error {
	ctx := r.manager.ctx
	record, ok := r.runtimeRecord()
	if !ok || record.Attempt == nil || record.Attempt.Reference != ref {
		return fmt.Errorf("daemon: readiness lost its exact attempt")
	}
	deadline := record.Attempt.Deadline
	wallDeadline := time.Now().Add(min(max(deadline.Sub(r.manager.now()), 0), service.Demand.ReadyTimeout))
	for {
		observation := r.manager.persistence.sessions.ownedFate(ref)
		if observation.fate == sessionEnded {
			return r.terminalRuntimeFailure(service, ref, observation)
		}
		if !r.manager.now().Before(deadline) || !time.Now().Before(wallDeadline) {
			return r.runtimeReadinessFailure(ctx, ref, service)
		}
		if observation.fate == sessionLive && r.manager.allAccept(ctx, service.UpstreamPorts()) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("route %s: readiness: %w", service.Route(), ctx.Err())
		case <-time.After(r.manager.poll):
		}
	}
}

func (r *demandRoute) terminalRuntimeFailure(service meshserve.Service, ref session.Reservation, observation ownedObservation) error {
	if err := r.consumeRuntime(ref, observation, true); err != nil {
		return err
	}
	return r.manager.startFailure(r.manager.ctx, service, ref.ID, exitDescription(observation.code), false)
}

func (r *demandRoute) runtimeReadinessFailure(ctx context.Context, ref session.Reservation, service meshserve.Service) error {
	err := r.changeRuntime(func(record *storage.ServiceRuntime) error {
		if record.Attempt == nil || record.Attempt.Reference != ref {
			return fmt.Errorf("daemon: stale readiness result")
		}
		oweRuntimeStop(record, causeReadiness)
		return nil
	}, nil)
	if err != nil {
		return err
	}
	stopCtx, cancel := context.WithTimeout(ctx, demandStopTimeout)
	stopErr := r.stopRuntime(stopCtx, causeReadiness)
	cancel()
	reason := fmt.Sprintf("%s did not accept before its original readiness deadline", portList(service.UpstreamPorts()))
	reason, ended := readinessCleanupReason(reason, stopErr)
	diagnostic := r.manager.startFailure(ctx, service, ref.ID, reason, false)
	return demandFailure{summary: reason, message: diagnostic.Error(), ended: ended}
}

func (r *demandRoute) consumeRuntime(ref session.Reservation, observation ownedObservation, failure bool) error {
	return r.changeRuntime(func(record *storage.ServiceRuntime) error {
		if record.Attempt == nil {
			return nil
		}
		if record.Attempt.Reference != ref {
			return fmt.Errorf("daemon: stale terminal attempt")
		}
		if failure && record.Attempt.Launched.Demand.Kept() {
			scheduleRuntimeFailure(record, observation, r.manager.now())
		}
		record.Phase, record.Attempt = storage.ServiceNone, nil
		return nil
	}, func() { r.mirrorRuntimeSchedule() })
}

func scheduleRuntimeFailure(record *storage.ServiceRuntime, observation ownedObservation, now time.Time) {
	stable := record.Phase == storage.ServiceReady && !observation.exitedAt.IsZero() && !record.Attempt.ReadyAt.IsZero() && observation.exitedAt.Sub(record.Attempt.ReadyAt) >= keptStableRun
	if stable {
		record.Failures = 0
	}
	record.Failures = min(record.Failures+1, keptRestartSteps)
	origin := observation.exitedAt
	if origin.IsZero() || observation.interrupted {
		origin = now
	}
	record.RestartAt = origin.Add(keptRestartDelay(record.Failures))
}

func (r *demandRoute) mirrorRuntimeSchedule() {
	// Called while an acceptance owns the map, before admitting its successor.
	record := r.manager.persistence.records[r.runtimeName()]
	r.mu.Lock()
	r.restartFailures, r.restartAt = record.Failures, record.RestartAt
	r.mu.Unlock()
}

func (r *demandRoute) prepareRuntimeStop(cause demandCause) (session.Reservation, string, error) {
	var reference session.Reservation
	var token string
	err := r.changeRuntime(func(record *storage.ServiceRuntime) error {
		if record.Attempt == nil {
			return fmt.Errorf("daemon: no exact attempt to stop")
		}
		oweRuntimeStop(record, cause)
		reference, token = record.Attempt.Reference, record.Attempt.StopToken
		return nil
	}, nil)
	return reference, token, err
}

func (r *demandRoute) stopRuntime(ctx context.Context, cause demandCause) error {
	ref, token, err := r.prepareRuntimeStop(cause)
	if err != nil {
		return err
	}
	if err := r.manager.persistence.sessions.stopOwned(ctx, ref); err != nil {
		return err
	}
	return r.changeRuntime(func(record *storage.ServiceRuntime) error {
		if record.Attempt == nil {
			return nil
		}
		if record.Attempt.Reference != ref || record.Attempt.StopToken != token {
			return fmt.Errorf("daemon: stale managed stop result")
		}
		if record.Attempt.Launched.Demand.Kept() && (record.Attempt.StopCause == string(causeFailedStart) || record.Attempt.StopCause == string(causeReadiness)) {
			record.Failures = min(record.Failures+1, keptRestartSteps)
			record.RestartAt = r.manager.now().Add(keptRestartDelay(record.Failures))
		}
		record.Phase, record.Attempt = storage.ServiceNone, nil
		return nil
	}, r.mirrorRuntimeSchedule)
}

func (r *demandRoute) runtimeFailedAdmission() {
	_ = r.changeRuntime(func(record *storage.ServiceRuntime) error {
		if record.Attempt != nil {
			return nil
		}
		r.mu.Lock()
		kept := r.service.Demand.Kept()
		r.mu.Unlock()
		if kept {
			record.Failures = min(record.Failures+1, keptRestartSteps)
			record.RestartAt = r.manager.now().Add(keptRestartDelay(record.Failures))
		}
		return nil
	}, r.mirrorRuntimeSchedule)
}

func (r *demandRoute) adoptRuntime(ctx context.Context) {
	record, exists := r.runtimeRecord()
	if !exists {
		r.discoverRuntime(ctx)
		return
	}
	if record.Attempt == nil {
		r.runtimeAbsent()
		return
	}
	ref := record.Attempt.Reference
	observation := r.manager.persistence.sessions.ownedFate(ref)
	if observation.fate == sessionEnded {
		if err := r.consumeRuntime(ref, observation, record.Phase != storage.ServiceCleanup); err != nil {
			r.runtimeAdoptionUnknown(err)
			return
		}
		r.mu.Lock()
		r.pending = nil
		r.owned, r.state = ownsNothing, protocol.DemandStopped
		r.finishStopWithoutWorkerLocked()
		r.continueLocked()
		r.unlock()
		return
	}
	if observation.fate == sessionUnknown {
		r.runtimeAdoptionUnknown(fmt.Errorf("session %s exact instance is unresolved", ref.ID))
		return
	}
	r.restoreLiveRuntime(record)
}

func (r *demandRoute) runtimeAbsent() {
	r.mu.Lock()
	r.pending = nil
	r.owned, r.state = ownsNothing, protocol.DemandStopped
	r.finishStopWithoutWorkerLocked()
	r.continueLocked()
	r.unlock()
}

func (r *demandRoute) discoverRuntime(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	candidate, found, err := r.manager.sessions.findLabelled(ctx, r.service.Label())
	if err == nil && found {
		err = fmt.Errorf("session %s may run this route; its complete launched environment is unknown", candidate.ID)
	}
	if err != nil {
		r.runtimeAdoptionUnknown(err)
		return
	}
	r.mu.Lock()
	r.pending = nil
	r.owned, r.state = ownsNothing, protocol.DemandStopped
	r.finishStopWithoutWorkerLocked()
	r.continueLocked()
	r.unlock()
}

func (r *demandRoute) runtimeAdoptionUnknown(err error) {
	r.mu.Lock()
	r.pending = nil
	r.markAdoptionUnknownLocked(r.service.Label(), err)
	r.unlock()
}

func (r *demandRoute) restoreLiveRuntime(observed storage.ServiceRuntime) {
	var current storage.ServiceRuntime
	err := r.changeRuntime(func(record *storage.ServiceRuntime) error {
		if record.Attempt == nil || record.Attempt.Reference != observed.Attempt.Reference {
			return fmt.Errorf("daemon: stale adoption result")
		}
		current = cloneRuntime(*record)
		return nil
	}, func() { r.applyLiveRuntime(current) })
	if err != nil {
		r.runtimeAdoptionUnknown(err)
	}
}

func (r *demandRoute) applyLiveRuntime(record storage.ServiceRuntime) {
	a := record.Attempt
	r.mu.Lock()
	r.pending = nil
	r.sessionID = a.Reference.ID
	r.restartFailures, r.restartAt = record.Failures, record.RestartAt
	changed := !sameLaunch(r.service.Demand, a.Launched.Demand) || r.service.Label() != a.Launched.Label()
	if r.removed || changed || r.restart || r.stopRequested || record.Phase == storage.ServiceCleanup {
		r.owned, r.state = ownsUncertain, protocol.DemandStopping
		r.stopCause, r.retryAt = demandCause(a.StopCause), time.Time{}
		if r.stopCause == "" {
			r.stopCause = adoptedStopCause(r.removed, r.stopRequested)
		}
		r.beginStopLocked(r.stopCause)
		r.unlock()
		return
	}
	if record.Phase == storage.ServiceReady {
		r.owned, r.state = ownsLive, protocol.DemandRunning
		r.readyAt, r.readyID = a.ReadyAt, a.Reference.ID
		r.armIdleLocked()
		route := r.service.Route()
		r.unlock()
		r.manager.logEvent("route %s: adopted session %s from an earlier daemon", route, a.Reference.ID)
		return
	}
	transition := &demandTransition{starting: true, done: make(chan struct{})}
	r.pending, r.state = transition, protocol.DemandStarting
	r.manager.goroutines.Go(func() {
		err := r.awaitRuntimeReady(a.Launched, a.Reference)
		if err == nil {
			err = r.runtimePhase(a.Reference, storage.ServiceReady)
		}
		r.finishStart(transition, a.Reference.ID, err, causeKeptStartup)
	})
	r.unlock()
}

func (m *demandManager) syncRuntimeBindings(services []meshserve.Service) {
	p := m.persistence
	if p == nil {
		return
	}
	p.mu.Lock()
	intent := p.pending
	active := intent != nil && intent.definition && !intent.finished
	p.mu.Unlock()
	if !active {
		return
	}
	wanted := make(map[string]meshserve.Service)
	for _, service := range services {
		wanted[service.Name] = service
	}
	for name, record := range p.records {
		service, exists := wanted[name]
		var definition *meshserve.Service
		if exists {
			definition = &service
		}
		next := runtimeBinding(record, definition)
		if next == nil {
			continue
		}
		intent.before, intent.next, intent.apply = &record, next, nil
		if err := m.persistIntent(m.ctx, intent); err != nil {
			return
		}
	}

	intent.before, intent.next = nil, nil
}

func runtimeBinding(record storage.ServiceRuntime, definition *meshserve.Service) *storage.ServiceRuntime {
	if sameRuntimeDefinition(record.Definition, definition) {
		return nil
	}
	next := cloneRuntime(record)
	next.Definition = definition
	if definition != nil {
		copied := cloneRuntimeDefinition(*definition)
		next.Definition = &copied
	}
	if record.Definition != nil && definition != nil && operationalChange(*record.Definition, *definition) {
		next.Failures, next.Restarts, next.RestartAt = 0, 0, time.Time{}
	}
	if definition == nil {
		oweRuntimeStop(&next, causeRemoved)
	}
	next.Version++
	return &next
}

func sameRuntimeDefinition(a, b *meshserve.Service) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

func (m *demandManager) restoreRetiringRuntime() {
	p := m.persistence
	if p == nil {
		return
	}
	for name, record := range p.records {
		if m.routes[name] != nil || m.retiring[name] != nil {
			continue
		}
		service := meshserve.Service{Name: name}
		if record.Attempt != nil {
			service = record.Attempt.Launched
		}
		m.retiring[name] = &demandRoute{manager: m, service: service, removed: true, state: protocol.DemandStopped, unbound: map[uint16]string{}}
	}
}

func (r *demandRoute) initRuntime() {
	if r.manager.persistence == nil {
		r.adopt(r.manager.ctx)
		return
	}
	record, exists := r.runtimeRecord()
	r.mu.Lock()
	if exists {
		r.restartFailures, r.restartAt = record.Failures, record.RestartAt
		if record.RestartAt.After(r.manager.now().Add(keptRestartMax)) {
			r.restartAt = r.manager.now().Add(keptRestartMax)
		}
	}
	transition := &demandTransition{adopting: true, done: make(chan struct{})}
	r.pending, r.owned = transition, ownsUnknown
	r.unlock()
	r.manager.goroutines.Go(func() { r.adoptRuntime(r.manager.ctx); close(transition.done) })
}

func (m *demandManager) deleteRuntimeRoute(r *demandRoute) {
	if m.persistence == nil {
		return
	}
	m.goroutines.Go(func() { m.deleteRuntime(r) })
}

func (m *demandManager) deleteRuntime(r *demandRoute) {
	intent, err := m.beginAcceptance(m.ctx, false)
	if err != nil {
		return
	}
	m.mu.Lock()
	removable := m.retiring[r.runtimeName()] == r && m.routes[r.runtimeName()] == nil
	m.mu.Unlock()
	record, exists := m.persistence.records[r.runtimeName()]
	if !removable || !exists || record.Attempt != nil {
		m.endAcceptance(intent, false)
		return
	}
	intent.before = &record
	intent.apply = func() {
		m.mu.Lock()
		if m.retiring[r.runtimeName()] == r {
			delete(m.retiring, r.runtimeName())
		}
		m.mu.Unlock()
	}
	_ = m.persistIntent(m.ctx, intent)
}

func (r *demandRoute) checkRuntimeSession() {
	record, exists := r.runtimeRecord()
	if !exists || record.Phase != storage.ServiceReady {
		return
	}
	r.mu.Lock()
	running := r.pending == nil && r.owned == ownsLive
	r.mu.Unlock()
	if !running {
		return
	}
	observation := r.manager.persistence.sessions.ownedFate(record.Attempt.Reference)
	if observation.fate != sessionEnded {
		return
	}
	if err := r.consumeRuntime(record.Attempt.Reference, observation, true); err != nil {
		return
	}
	r.mu.Lock()
	if r.sessionID == record.Attempt.Reference.ID && r.pending == nil {
		r.owned, r.state = ownsNothing, protocol.DemandFailed
		r.readyID = ""
		r.failure = exitDescription(observation.code)
	}
	route := r.service.Route()
	next := "the next connection through Mesh starts it again"
	if r.service.Demand.Kept() {
		next = "restarting in " + keptRestartDelay(r.restartFailures).String()
	}
	r.unlock()
	r.manager.logEvent("route %s: session %s ended (%s); %s", route, record.Attempt.Reference.ID, exitDescription(observation.code), next)
}
