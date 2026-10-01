package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	meshserve "github.com/shaul/mesh/internal/serve"
)

const (
	demandStopTimeout = 30 * time.Second
	// demandCleanupRetry spaces out stopping again a session whose stop
	// failed, so a worker nobody can reach is retried steadily instead of by
	// every request, Sync and supervisor tick.
	demandCleanupRetry = 15 * time.Second
)

// demandOwnership is what a route knows about the session it launched. It
// is kept apart from the state the route shows because a failed route can
// still own a running worker, and only a route that owns nothing may launch.
type demandOwnership int

const (
	// ownsNothing: no session this route launched can still be running.
	ownsNothing demandOwnership = iota
	// ownsLive: sessionID is running and serves the route.
	ownsLive
	// ownsUncertain: sessionID may still be running, because stopping it
	// failed or its start was cut off before anyone saw it end. It is
	// stopped again before anything else launches.
	ownsUncertain
)

// demandRoute is one route's live state. Its mutex orders every state change;
// starting and stopping run outside it and report back through a transition.
type demandRoute struct {
	manager *demandManager

	mu      sync.Mutex
	service meshserve.Service
	removed bool
	restart bool
	state   string
	owned   demandOwnership
	retryAt time.Time // when an uncertain session is next stopped again
	// blocked is the logged answer a request gets until then, so a browser
	// retrying every second does not log the same failure every second.
	blocked     error
	sessionID   string
	failure     string
	connections int
	idleTimer   *time.Timer
	idleToken   uint64
	pending     *demandTransition
	unbound     map[uint16]string
}

// demandTransition is one start or stop in progress. err is written before
// done closes, so a waiter reads it after <-done without the route lock.
type demandTransition struct {
	starting bool
	done     chan struct{}
	err      error
}

func (r *demandRoute) definition() meshserve.Service {
	r.mu.Lock()
	defer r.unlock()
	return r.service
}

func (r *demandRoute) onDemand() bool {
	r.mu.Lock()
	defer r.unlock()
	return r.service.Demand != nil
}

// adopt takes over a session a previous daemon started for this route. The
// idle clock starts over at a full window: this daemon never saw the
// connections the old one counted.
func (r *demandRoute) adopt() {
	if r.service.Demand == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.manager.ctx, 5*time.Second)
	defer cancel()
	id, found, err := r.manager.sessions.findLabelled(ctx, r.service.Label())
	if err != nil {
		r.manager.report(fmt.Errorf("daemon: route %s: find its running session: %w", r.service.Route(), err))
		return
	}
	if !found {
		return
	}
	r.mu.Lock()
	defer r.unlock()
	r.state = protocol.DemandRunning
	r.owned = ownsLive
	r.sessionID = id
	r.armIdleLocked()
}

func (r *demandRoute) redefine(service meshserve.Service) {
	r.mu.Lock()
	defer r.unlock()
	previous := r.service
	r.service = service
	// A route added back while its removal was still stopping keeps the
	// session it owns.
	r.removed = false
	for port := range r.unbound {
		if !slices.ContainsFunc(service.Listens, func(listen meshserve.Listen) bool { return listen.Public == port }) {
			delete(r.unbound, port)
		}
	}
	if r.owned == ownsUncertain {
		if r.cleanupDueLocked() {
			r.beginStopLocked()
		}
		return
	}
	if sameLaunch(previous.Demand, service.Demand) && previous.Label() == service.Label() {
		// Only the timing changed. The idle clock picks up the new window
		// when it next starts; a running idle clock starts over with it.
		if r.idleTimer != nil {
			r.armIdleLocked()
		}
		return
	}
	// A new recipe takes effect on the next start; the running session was
	// started from the old one, so it stops now rather than lingering. A new
	// label counts too: the next daemon would not recognise the old one and
	// would leave it running unowned.
	switch {
	case r.state == protocol.DemandStarting:
		r.restart = true
	case r.pending == nil && r.owned == ownsLive:
		r.beginStopLocked()
	case r.state == protocol.DemandFailed:
		r.state = protocol.DemandStopped
		r.failure = ""
	}
}

// sameLaunch reports whether two recipes start the same process. Idle and
// ready timeouts govern when it runs, not what runs.
func sameLaunch(a, b *meshserve.Demand) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Command == b.Command && a.Cwd == b.Cwd && slices.Equal(a.Env, b.Env)
}

// retire stops a removed route's session, or tries again when the last stop
// failed. It reports whether the route owns nothing any more.
func (r *demandRoute) retire() bool {
	r.mu.Lock()
	defer r.unlock()
	r.removed = true
	r.cancelIdleLocked()
	if r.pending == nil && r.owned == ownsLive || r.cleanupDueLocked() {
		r.beginStopLocked()
	}
	return r.pending == nil && r.owned == ownsNothing
}

// cleanupDueLocked reports whether a session that may still be running is
// due to be stopped again.
func (r *demandRoute) cleanupDueLocked() bool {
	return r.pending == nil && r.owned == ownsUncertain && !time.Now().Before(r.retryAt)
}

func (r *demandRoute) markUncertainLocked() {
	r.owned = ownsUncertain
	r.retryAt = time.Now().Add(r.manager.cleanupRetry)
	r.blocked = nil
}

// retryCleanup stops again a session whose stop failed, once that is due.
// A closed manager stops nothing: daemon shutdown keeps every session.
func (r *demandRoute) retryCleanup() {
	r.mu.Lock()
	defer r.unlock()
	if !r.manager.closed.Load() && r.cleanupDueLocked() {
		r.beginStopLocked()
	}
}

// hold counts one open connection until the returned release runs. A
// connection to a stopped or failed route starts it: this is the wake signal.
func (r *demandRoute) hold() func() {
	r.mu.Lock()
	r.connections++
	r.cancelIdleLocked()
	if r.service.Demand != nil && !r.removed && r.pending == nil && r.owned == ownsNothing {
		r.beginStartLocked()
	}
	r.unlock()
	var once sync.Once
	return func() { once.Do(r.release) }
}

func (r *demandRoute) release() {
	r.mu.Lock()
	defer r.unlock()
	r.connections--
	if r.connections == 0 {
		r.armIdleLocked()
	}
}

// ready waits until the route's session accepts on every upstream. A start
// that fails answers everyone who waited on it; the next call tries again,
// first stopping a session the failure may have left running.
func (r *demandRoute) ready(ctx context.Context) error {
	for {
		r.mu.Lock()
		route := r.service.Route()
		if r.removed {
			r.unlock()
			return fmt.Errorf("route %s was removed", route)
		}
		if r.service.Demand == nil || r.owned == ownsLive && r.pending == nil {
			r.unlock()
			return nil
		}
		transition, err := r.towardRunningLocked() //nolint:contextcheck // runs on the daemon's context, so a waiter that gives up never abandons a launch or its cleanup
		r.unlock()
		if err != nil {
			return err
		}
		select {
		case <-transition.done:
		case <-ctx.Done():
			return fmt.Errorf("waiting for route %s: %w", route, ctx.Err())
		}
		if transition.starting || transition.err != nil {
			return transition.err
		}
	}
}

// towardRunningLocked returns the transition in progress, or begins the next
// one on the way to running: a stop of a session a failure may have left
// behind, otherwise a start.
func (r *demandRoute) towardRunningLocked() (*demandTransition, error) {
	if r.pending == nil && r.owned == ownsUncertain {
		if !r.cleanupDueLocked() {
			if r.blocked == nil {
				r.blocked = meshserve.LogDemandFailure(fmt.Errorf("route %s did not start: session %s may still be running and is stopped again at %s: %s",
					r.service.Route(), r.sessionID, r.retryAt.Format(time.TimeOnly), r.failure), r.manager.logger)
			}
			return nil, r.blocked
		}
		r.beginStopLocked()
	}
	if r.pending == nil {
		r.beginStartLocked()
	}
	return r.pending, nil
}

func (r *demandRoute) stop(ctx context.Context) error {
	for {
		r.mu.Lock()
		route := r.service.Route()
		transition := r.pending
		if transition == nil {
			if r.owned == ownsNothing {
				r.unlock()
				return nil
			}
			r.cancelIdleLocked()
			r.beginStopLocked()
			transition = r.pending
		}
		r.unlock()
		select {
		case <-transition.done:
		case <-ctx.Done():
			return fmt.Errorf("stopping route %s: %w", route, ctx.Err())
		}
		if !transition.starting {
			return transition.err
		}
	}
}

func (r *demandRoute) beginStartLocked() {
	transition := &demandTransition{starting: true, done: make(chan struct{})}
	r.pending = transition
	r.state = protocol.DemandStarting
	r.failure = ""
	r.restart = false
	service := r.service
	go r.runStart(transition, service)
}

func (r *demandRoute) runStart(transition *demandTransition, service meshserve.Service) {
	manager := r.manager
	command := []string{hostShell(), "-lc", service.Demand.Command}
	id, err := manager.sessions.startLabelled(manager.ctx, service.Label(), command, service.Demand.Cwd, service.Demand.Env)
	if errors.As(err, new(publicationError)) {
		// Only publication failed. The worker is ready and belongs to this
		// route, so the start carries on; launching again would run it twice.
		manager.report(fmt.Errorf("daemon: route %s: %w", service.Route(), err))
		err = nil
	}
	if err != nil {
		err = demandFailure{
			summary: fmt.Sprintf("could not start a session: %v", err),
			message: fmt.Sprintf("route %s did not start: could not start a session for %q: %v", service.Route(), service.Demand.Command, err),
		}
	} else {
		err = manager.awaitReady(manager.ctx, service, id)
	}
	r.finishStart(transition, id, err)
	if id != "" {
		// Each start is a new session. Earlier ones for this route are over
		// and would otherwise pile up in `mesh ls --all` and on disk, which a
		// browser retrying a broken server would do once a second. The latest
		// stays, failed or not: its output is what explains the failure.
		manager.sessions.forgetLabelled(manager.ctx, service.Label(), id)
	}
}

func (r *demandRoute) finishStart(transition *demandTransition, id string, err error) {
	r.mu.Lock()
	r.pending = nil
	if id != "" {
		r.sessionID = id
	}
	switch {
	case err != nil:
		r.state = protocol.DemandFailed
		r.failure = err.Error()
		if failedStartOwnership(id, err) == ownsUncertain {
			r.markUncertainLocked()
		} else {
			r.owned = ownsNothing
		}
		var failure demandFailure
		if errors.As(err, &failure) {
			r.failure = failure.summary
		}
		transition.err = err
	case r.removed || r.restart:
		r.state = protocol.DemandRunning
		r.owned = ownsLive
		r.beginStopLocked()
		if r.removed {
			transition.err = fmt.Errorf("route %s was removed while it started", r.service.Route())
		} else {
			transition.err = fmt.Errorf("route %s changed while it started; retry", r.service.Route())
		}
	default:
		r.state = protocol.DemandRunning
		r.owned = ownsLive
		if r.connections == 0 {
			r.armIdleLocked()
		}
	}
	r.unlock()
	if transition.err != nil {
		transition.err = meshserve.LogDemandFailure(transition.err, r.manager.logger)
	}
	close(transition.done)
}

// failedStartOwnership is what a start that failed leaves the route owning.
// A session that launched stays owned until it is seen to end: a cancelled
// wait or a failed cleanup proves nothing about it.
func failedStartOwnership(id string, err error) demandOwnership {
	var failure demandFailure
	if id == "" || errors.As(err, &failure) && failure.ended {
		return ownsNothing
	}
	return ownsUncertain
}

func (r *demandRoute) beginStopLocked() {
	transition := &demandTransition{done: make(chan struct{})}
	r.pending = transition
	r.state = protocol.DemandStopping
	id := r.sessionID
	route := r.service.Route()
	go func() {
		ctx, cancel := context.WithTimeout(r.manager.ctx, demandStopTimeout)
		err := r.manager.sessions.stopSession(ctx, id)
		cancel()
		if err != nil {
			err = fmt.Errorf("daemon: route %s: stop session %s: %w", route, id, err)
			r.manager.report(err)
		}
		r.mu.Lock()
		r.pending = nil
		r.state = protocol.DemandStopped
		r.owned = ownsNothing
		if err != nil {
			r.state = protocol.DemandFailed
			r.failure = err.Error()
			r.markUncertainLocked()
		}
		transition.err = err
		r.unlock()
		close(transition.done)
	}()
}

func (r *demandRoute) armIdleLocked() {
	r.cancelIdleLocked()
	if r.service.Demand == nil || r.state != protocol.DemandRunning || r.pending != nil || r.manager.closed.Load() {
		return
	}
	token := r.idleToken
	r.idleTimer = time.AfterFunc(r.service.Demand.Idle, func() { r.idleExpired(token) })
}

func (r *demandRoute) cancelIdleLocked() {
	if r.idleTimer != nil {
		r.idleTimer.Stop()
		r.idleTimer = nil
	}
	r.idleToken++
}

func (r *demandRoute) idleExpired(token uint64) {
	r.mu.Lock()
	defer r.unlock()
	if token != r.idleToken || r.connections > 0 || r.state != protocol.DemandRunning || r.pending != nil {
		return
	}
	r.idleTimer = nil
	r.beginStopLocked()
}

// checkSession notices a running session that ended on its own: a crash, or
// someone running `mesh kill`. Its connections are already gone; the next
// one starts it again.
func (r *demandRoute) checkSession() {
	r.mu.Lock()
	id := r.sessionID
	running := r.state == protocol.DemandRunning && r.pending == nil
	r.unlock()
	if !running || id == "" {
		return
	}
	code, ended := r.manager.sessions.sessionExit(id)
	if !ended {
		return
	}
	r.mu.Lock()
	defer r.unlock()
	if r.state != protocol.DemandRunning || r.pending != nil || r.sessionID != id {
		return
	}
	r.cancelIdleLocked()
	r.state = protocol.DemandStopped
	r.owned = ownsNothing
	if code == nil || *code != 0 {
		r.state = protocol.DemandFailed
		r.failure = fmt.Sprintf("%s while serving (session %s)", exitDescription(code), id)
	}
}

func (r *demandRoute) markUnbound(port uint16, err error) bool {
	r.mu.Lock()
	defer r.unlock()
	message := err.Error()
	changed := r.unbound[port] != message
	r.unbound[port] = message
	return changed
}

func (r *demandRoute) isUnbound(port uint16) bool {
	r.mu.Lock()
	defer r.unlock()
	_, unbound := r.unbound[port]
	return unbound
}

func (r *demandRoute) markBound(port uint16) {
	r.mu.Lock()
	defer r.unlock()
	delete(r.unbound, port)
}

func (r *demandRoute) status() *protocol.ServiceDemand {
	r.mu.Lock()
	defer r.unlock()
	return r.statusLocked()
}

// retiringInfo describes a removed route that may still own a session.
func (r *demandRoute) retiringInfo() (protocol.ServiceInfo, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.retiringInfoLocked()
}
func (r *demandRoute) retiringInfoLocked() (protocol.ServiceInfo, bool) {
	if r.owned == ownsNothing {
		return protocol.ServiceInfo{}, false
	}
	info := protocol.ServiceDefinitionInfo(r.service)
	info.Demand = r.statusLocked()
	problem := fmt.Sprintf("removed; stopping its session %s", r.sessionID)
	if r.state == protocol.DemandFailed {
		problem = fmt.Sprintf("removed, but its session %s may still be running: %s", r.sessionID, r.failure)
	}
	info.Problem = boundedServiceProblem(problem)
	return info, true
}

func (r *demandRoute) statusLocked() *protocol.ServiceDemand {
	status := &protocol.ServiceDemand{State: r.state, Connections: r.connections, SessionID: r.sessionID}
	if r.service.Demand == nil {
		status.State = ""
		status.SessionID = ""
	}
	if r.state == protocol.DemandFailed {
		status.Failure = boundedServiceProblem(r.failure)
	}
	ports := make([]uint16, 0, len(r.unbound))
	for port := range r.unbound {
		ports = append(ports, port)
	}
	slices.Sort(ports)
	for _, port := range ports {
		status.Unbound = append(status.Unbound, boundedServiceProblem(r.unbound[port]))
	}
	return status
}

func (r *demandRoute) unlock() {
	if r.manager.onChange != nil {
		r.manager.onChange(r.changeInfoLocked(), r.removed)
	}
	r.mu.Unlock()
}
func (r *demandRoute) changeInfoLocked() protocol.ServiceInfo {
	info := protocol.ServiceDefinitionInfo(r.service)
	info.Demand = r.statusLocked()
	if !r.removed {
		return info
	}
	if retained, owning := r.retiringInfoLocked(); owning {
		return retained
	}
	info.Demand = nil
	return info
}
