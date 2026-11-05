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
	// ownsUnknown: discovery failed, so a prior daemon's worker may still be
	// running. A route in this state must resolve its label before launching.
	ownsUnknown
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

	mu           sync.Mutex
	service      meshserve.Service
	removed      bool
	restart      bool
	state        string
	owned        demandOwnership
	retryAt      time.Time // when an uncertain session is next stopped again
	adoptRetryAt time.Time
	adoptLabel   string
	// stopRequested is an accepted Stop the route still owes. It belongs to
	// the route, not to the caller that asked, so a start or adoption in
	// progress carries it out when it finishes even if that caller left.
	stopRequested bool
	// startRequested is an accepted Start waiting for a stop to finish. The
	// stop always completes first; the start follows it once.
	startRequested bool
	// controls numbers accepted Start and Stop controls in order; lastStart
	// and lastStop are the latest of each. A waiter whose control a later
	// opposite one replaced stops acting on it.
	controls, lastStart, lastStop uint64
	// held is a kept route stopped by `mesh serve stop`. It admits no start
	// until `mesh serve start`, a change of its definition, or a new daemon.
	held bool
	// stopCause is why the owned session is being stopped. Retries keep it,
	// and the log line reporting the outcome names it.
	stopCause demandCause
	// restartAt is when a kept route may start again after a failed start or
	// an exit. restartFailures counts those in a row and saturates at the cap.
	restartAt       time.Time
	restartFailures int
	// readyAt is when readyID became ready or was adopted. Only a run of that
	// exact session at least keptStableRun long resets the restart backoff.
	readyAt          time.Time
	readyID          string
	adoptIdlePending bool
	// blocked is the logged answer a request gets until then, so a browser
	// retrying every second does not log the same failure every second.
	blocked error
	// waiting is the shared answer while a kept route is held or waiting to
	// restart. It is logged outside the route lock, once per such state.
	waiting     *demandWaitingAnswer
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
	adopting bool
	// sessionID is the session a stop ends.
	sessionID string
	// superseded is a start that an accepted Stop ended, after which a later
	// Start asked again. Its waiters look at the route again and follow that
	// next start instead of taking this one as the answer.
	superseded bool
	done       chan struct{}
	err        error
}

// waitingFor names what a waiter is waiting for, so a cancelled wait says
// whether a start, a stop of one exact session, or discovery held it up.
func (t *demandTransition) waitingFor(route string) string {
	switch {
	case t.starting:
		return "route " + route + " to start"
	case t.adopting:
		return "route " + route + " to find its running session"
	default:
		return "route " + route + " to stop session " + t.sessionID
	}
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
// connections the old one counted. A kept route that finds nothing starts.
func (r *demandRoute) adopt() {
	if r.service.Demand == nil {
		return
	}
	label := r.service.Label()
	ctx, cancel := context.WithTimeout(r.manager.ctx, 5*time.Second)
	defer cancel()
	id, found, err := r.manager.sessions.findLabelled(ctx, label)
	r.mu.Lock()
	if err != nil {
		r.markAdoptionUnknownLocked(label, err)
		r.unlock()
		return
	}
	adopted := r.finishAdoptionLocked(label, id, found)
	r.armAdoptedIdleLocked()
	r.continueLocked()
	route := r.service.Route()
	r.unlock()
	if adopted {
		r.manager.logEvent("route %s: adopted session %s from an earlier daemon", route, id)
	}
}

func (r *demandRoute) redefine(service meshserve.Service) {
	r.mu.Lock()
	defer r.unlock()
	previous := r.service
	r.service = service
	// A route added back while its removal was still stopping keeps the
	// session it owns.
	r.removed = false
	r.forgetUnboundLocked(service)
	if !operationalChange(previous, service) {
		// Sync runs for every catalog change. A route whose definition did
		// not change, or changed only its display name, keeps its hold, its
		// restart schedule and whatever is in progress.
		if r.owned == ownsUncertain && r.cleanupDueLocked() {
			r.beginStopLocked(r.cleanupCauseLocked())
		}
		return
	}
	// A real change of this route is the owner's new intent for it: a hold
	// ends and the restart schedule starts over. Cleanup already owed stays.
	r.held = false
	r.resetRestartLocked()
	if r.owned == ownsUncertain {
		if r.cleanupDueLocked() {
			r.beginStopLocked(r.cleanupCauseLocked())
		}
		return
	}
	if r.owned == ownsUnknown {
		r.restart = r.restart || !sameLaunch(previous.Demand, service.Demand) || previous.Label() != service.Label()
		return
	}
	if sameLaunch(previous.Demand, service.Demand) && previous.Label() == service.Label() {
		r.applyLifetimeLocked(previous)
		return
	}
	r.replaceRecipeLocked()
}

// replaceRecipeLocked takes up a new recipe or label. It takes effect on the
// next start; the running session was started from the old one, so it stops
// now rather than lingering. A new label counts too: the next daemon would
// not recognise the old one and would leave it running unowned.
func (r *demandRoute) replaceRecipeLocked() {
	switch {
	case r.state == protocol.DemandStarting:
		r.restart = true
	case r.pending == nil && r.owned == ownsLive:
		r.cancelIdleLocked()
		r.beginStopLocked(causeRedefined)
	case r.state == protocol.DemandFailed:
		r.state = protocol.DemandStopped
		r.failure = ""
	}
	if r.service.Demand.Kept() {
		r.beginStartLocked(causeKeptStartup)
	}
}

// forgetUnboundLocked drops bind failures of listeners the route no longer has.
func (r *demandRoute) forgetUnboundLocked(service meshserve.Service) {
	for port := range r.unbound {
		if !slices.ContainsFunc(service.Listens, func(listen meshserve.Listen) bool { return listen.Public == port }) {
			delete(r.unbound, port)
		}
	}
}

// operationalChange reports whether a route's definition changed in anything
// beyond its display name.
func operationalChange(previous, next meshserve.Service) bool {
	previous.DisplayName = next.DisplayName
	return !previous.Equal(next)
}

// applyLifetimeLocked takes up a new lifetime or timing for the same launch.
// The running session keeps its ID: lifetime is not what runs.
func (r *demandRoute) applyLifetimeLocked(previous meshserve.Service) {
	if r.service.Demand.Kept() {
		// A queued idle callback finds a new token and a kept policy.
		r.cancelIdleLocked()
		r.beginStartLocked(causeKeptStartup)
		return
	}
	if previous.Demand.Kept() && r.pending == nil && r.owned == ownsNothing && r.state == protocol.DemandFailed {
		// The restart this failure announced no longer happens.
		r.state = protocol.DemandStopped
		r.failure = ""
	}
	// A running idle clock starts over with the new window, and a live
	// session that was kept starts one now.
	if r.owned == ownsLive && r.pending == nil && r.connections == 0 {
		r.armIdleLocked()
	}
}

// sameLaunch reports whether two recipes start the same process. Lifetime
// and ready timeout govern when it runs, not what runs.
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
	r.startRequested = false
	r.cancelIdleLocked()
	switch {
	case r.pending == nil && r.owned == ownsLive:
		r.beginStopLocked(causeRemoved)
	case r.cleanupDueLocked():
		r.beginStopLocked(r.cleanupCauseLocked())
	}
	if r.pending == nil && r.owned == ownsUnknown && r.adoptionDueLocked() {
		r.beginAdoptLocked()
	}
	return r.pending == nil && r.owned == ownsNothing
}

// cleanupDueLocked reports whether a session that may still be running is
// due to be stopped again.
func (r *demandRoute) cleanupDueLocked() bool {
	return r.pending == nil && r.owned == ownsUncertain && !time.Now().Before(r.retryAt)
}

// cleanupCauseLocked is why an uncertain session is stopped again: the
// reason its first stop was for.
func (r *demandRoute) cleanupCauseLocked() demandCause {
	if r.stopCause != "" {
		return r.stopCause
	}
	return causeCleanup
}

func (r *demandRoute) markUncertainLocked() {
	r.owned = ownsUncertain
	r.retryAt = time.Now().Add(r.manager.cleanupRetry)
	r.blocked = nil
}

func (r *demandRoute) markAdoptionUnknownLocked(label string, err error) {
	r.owned = ownsUnknown
	r.adoptLabel = label
	r.adoptRetryAt = time.Now().Add(r.manager.adoptionRetry)
	r.blocked = nil
	r.state = protocol.DemandFailed
	r.failure = fmt.Sprintf("could not find a running session: %v", err)
	r.manager.report(fmt.Errorf("daemon: route %s: find its running session: %w", r.service.Route(), err))
}

func (r *demandRoute) adoptionDueLocked() bool {
	return !time.Now().Before(r.adoptRetryAt)
}

// retryCleanup stops again a session whose stop failed, once that is due.
// A closed manager stops nothing: daemon shutdown keeps every session.
func (r *demandRoute) retryCleanup() {
	r.mu.Lock()
	defer r.unlock()
	if !r.manager.shuttingDown() && r.cleanupDueLocked() {
		r.beginStopLocked(r.cleanupCauseLocked())
	}
}

func (r *demandRoute) retryAdoption() {
	r.mu.Lock()
	defer r.unlock()
	if r.manager.closed.Load() {
		return
	}
	r.armAdoptedIdleLocked()
	if r.pending != nil || r.owned != ownsUnknown || !r.adoptionDueLocked() {
		return
	}
	r.beginAdoptLocked()
}

// keepRunning starts a kept route that owns nothing once its restart delay
// has passed. It is the supervisor's half of the same admission traffic uses.
func (r *demandRoute) keepRunning() {
	r.mu.Lock()
	defer r.unlock()
	if r.service.Demand.Kept() {
		r.beginStartLocked(r.keptCauseLocked())
	}
}

// hold counts one open connection until the returned release runs. A
// connection to a stopped or failed route starts it: this is the wake signal.
// It also returns the start it began, if any.
func (r *demandRoute) hold(cause demandCause) (func(), *demandTransition) {
	r.mu.Lock()
	r.connections++
	r.cancelIdleLocked()
	var started *demandTransition
	switch {
	case r.beginStartLocked(cause):
		started = r.pending
	case r.owned == ownsUnknown && r.service.Demand != nil && !r.removed && !r.manager.shuttingDown() && r.pending == nil:
		r.beginAdoptLocked()
		started = r.pending
	}
	r.unlock()
	var once sync.Once
	return func() { once.Do(r.release) }, started
}

func (r *demandRoute) release() {
	r.mu.Lock()
	defer r.unlock()
	r.connections--
	if r.connections == 0 {
		r.adoptIdlePending = false
		r.armIdleLocked()
	}
}

// start is `mesh serve start`: the one control that may override a hold and
// the restart delay. A stop already owed still finishes first; this start
// follows it even when its caller stops waiting.
func (r *demandRoute) start(ctx context.Context) error {
	r.mu.Lock()
	r.controls++
	control := r.controls
	r.lastStart = control
	r.held = false
	r.resetRestartLocked()
	if r.owned == ownsUncertain || r.stopRequested || r.pending != nil && !r.pending.starting && !r.pending.adopting {
		r.startRequested = true
	}
	r.unlock()
	return r.readyFor(ctx, causeExplicitStart, control)
}

// ready waits until the route's session accepts on every upstream. A start
// that fails answers everyone who waited on it; the next call tries again,
// first stopping a session the failure may have left running.
func (r *demandRoute) ready(ctx context.Context, cause demandCause) error {
	return r.readyFor(ctx, cause, 0)
}

// readyFor is ready for `mesh serve start` number control, which gives up
// once a later Stop is accepted: that Stop is the owner's latest intent.
func (r *demandRoute) readyFor(ctx context.Context, cause demandCause, control uint64) error {
	for {
		r.mu.Lock()
		route := r.service.Route()
		if err := r.readyRefusalLocked(control); err != nil {
			r.unlock()
			return err
		}
		transition, waiting, err := r.towardRunningLocked(cause) //nolint:contextcheck // runs on the daemon's context, so a waiter that gives up never abandons a launch or its cleanup
		r.unlock()
		if waiting != nil {
			return waiting.answer(r.manager.logger)
		}
		if err != nil || transition == nil {
			return err
		}
		if err := waitForTransition(ctx, route, transition); err != nil {
			return err
		}
		if !transition.superseded && (transition.starting || transition.err != nil) {
			return transition.err
		}
	}
}

// readyRefusalLocked is why a request may not move the route toward running
// at all: it was removed, the daemon is shutting down, or, for `mesh serve
// start` number control, a later Stop was accepted.
func (r *demandRoute) readyRefusalLocked(control uint64) error {
	if err := r.unavailableLocked(); err != nil {
		return err
	}
	if control != 0 && r.lastStop > control {
		return fmt.Errorf("route %s was stopped by a later mesh serve stop", r.service.Route())
	}
	return nil
}

// waitForTransition waits for transition, or names what it waited for when
// the caller gives up first.
func waitForTransition(ctx context.Context, route string, transition *demandTransition) error {
	select {
	case <-transition.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("waiting for %s: %w", transition.waitingFor(route), ctx.Err())
	}
}

// awaitStart is ready for a caller that already began started, whose outcome
// is its answer even when it finished before the caller got here.
func (r *demandRoute) awaitStart(ctx context.Context, started *demandTransition, cause demandCause) error {
	if started == nil {
		return r.ready(ctx, cause)
	}
	select {
	case <-started.done:
		if !started.superseded && (started.err != nil || started.starting) {
			return started.err
		}
		return r.ready(ctx, cause)
	case <-ctx.Done():
		return fmt.Errorf("waiting for %s: %w", started.waitingFor(r.definition().Route()), ctx.Err())
	}
}

// towardRunningLocked returns the transition in progress, or begins the next
// one on the way to running: a stop of a session a failure may have left
// behind, otherwise a start. A kept route that is held or waiting to restart
// returns the shared answer for that state instead.
func (r *demandRoute) towardRunningLocked(cause demandCause) (*demandTransition, *demandWaitingAnswer, error) {
	if r.service.Demand == nil || r.owned == ownsLive && r.pending == nil {
		r.armAdoptedIdleLocked()
		return nil, nil, nil
	}
	if r.pending != nil {
		return r.pending, nil, nil
	}
	if r.owned == ownsUnknown {
		if !r.adoptionDueLocked() {
			return nil, nil, r.adoptionBlockedLocked()
		}
		r.beginAdoptLocked()
		return r.pending, nil, nil
	}
	if r.owned == ownsUncertain && !r.cleanupDueLocked() {
		if r.blocked == nil {
			r.blocked = meshserve.LogDemandFailure(fmt.Errorf("route %s did not start: session %s may still be running and is stopped again at %s: %s",
				r.service.Route(), r.sessionID, r.retryAt.Format(time.TimeOnly), r.failure), r.manager.logger)
		}
		return nil, nil, r.blocked
	}
	if r.owned == ownsUncertain {
		r.beginStopLocked(r.cleanupCauseLocked())
		return r.pending, nil, r.shutDownLocked(r.pending)
	}
	if waiting := r.waitingLocked(); waiting != nil {
		return nil, waiting, nil
	}
	r.beginStartLocked(cause)
	return r.pending, nil, r.shutDownLocked(r.pending)
}

// shutDownLocked is the answer when shutdown refused the transition a
// request needed.
func (r *demandRoute) shutDownLocked(began *demandTransition) error {
	if began != nil {
		return nil
	}
	return fmt.Errorf("route %s is shut down", r.service.Route())
}

func (r *demandRoute) adoptionBlockedLocked() error {
	if r.blocked == nil {
		r.blocked = meshserve.LogDemandFailure(fmt.Errorf("route %s did not start: session with label %s may still be running; discovery retries at %s: %s",
			r.service.Route(), r.adoptLabel, r.adoptRetryAt.Format(time.TimeOnly), r.failure), r.manager.logger)
	}
	return r.blocked
}

func (r *demandRoute) unavailableLocked() error {
	if r.removed {
		return fmt.Errorf("route %s was removed", r.service.Route())
	}
	if r.manager.shuttingDown() {
		return fmt.Errorf("route %s is shut down", r.service.Route())
	}
	return nil
}

// stop is `mesh serve stop`. It records the stop as the route's own intent
// first, so a caller that stops waiting cannot abandon it.
func (r *demandRoute) stop(ctx context.Context) error {
	control, err := r.acceptStop() //nolint:contextcheck // accepted stop work outlives its waiter and is bounded by the daemon's lifetime
	if err != nil {
		return err
	}
	for {
		r.manager.beforeStopLook()
		r.mu.Lock()
		route := r.service.Route()
		if r.lastStart > control {
			// A later Start replaced this Stop. The stop it already owed
			// still finishes; nothing more is stopped on its behalf.
			transition := r.pending
			r.unlock()
			return awaitOwedStop(ctx, route, transition)
		}
		if err := r.prepareAdoptionStopLocked(); err != nil { //nolint:contextcheck // adoption and cleanup must outlive a cancelled Stop waiter, within the daemon's lifetime
			r.unlock()
			return err
		}
		if r.pending == nil && r.owned != ownsNothing {
			r.cancelIdleLocked()
			r.beginStopLocked(causeStopRequested)
		}
		transition, err := r.pending, r.stopAnswerLocked()
		r.unlock()
		if transition == nil {
			return err
		}
		if err := waitForTransition(ctx, route, transition); err != nil {
			return fmt.Errorf("stopping route %s: %w", route, err)
		}
		if !stopContinuesAfter(transition) {
			return transition.err
		}
	}
}

// acceptStop records a Stop under the route lock and returns its number.
func (r *demandRoute) acceptStop() (uint64, error) {
	r.mu.Lock()
	defer r.unlock()
	if err := r.unavailableLocked(); err != nil {
		return 0, err
	}
	return r.acceptStopLocked(), nil
}

// stopAnswerLocked is a Stop's answer when nothing is left to wait for: done
// once the route owns nothing, refused when shutdown kept it from stopping.
func (r *demandRoute) stopAnswerLocked() error {
	if r.pending != nil || r.owned == ownsNothing {
		return nil
	}
	return r.shutDownLocked(nil)
}

// stopContinuesAfter reports whether a Stop looks at the route again after
// transition: a start or a successful adoption leaves something to stop.
func stopContinuesAfter(transition *demandTransition) bool {
	return transition.starting || transition.adopting && transition.err == nil
}

// awaitOwedStop waits for a stop already in progress, if that is what the
// route is doing.
func awaitOwedStop(ctx context.Context, route string, transition *demandTransition) error {
	if transition == nil || transition.starting || transition.adopting {
		return nil
	}
	if err := waitForTransition(ctx, route, transition); err != nil {
		return fmt.Errorf("stopping route %s: %w", route, err)
	}
	return transition.err
}

// acceptStopLocked records an accepted Stop and returns its number. A start
// or adoption in progress owes the stop when it finishes. A kept route is
// also held, and one waiting to restart has nothing to stop: it settles
// stopped at once, without the restart it announced and without claiming to
// have stopped anything.
func (r *demandRoute) acceptStopLocked() uint64 {
	r.controls++
	r.lastStop = r.controls
	r.startRequested = false
	if r.owned != ownsNothing || r.pending != nil && (r.pending.starting || r.pending.adopting) {
		r.stopRequested = true
	}
	r.holdKeptLocked()
	if r.pending == nil && (r.owned == ownsLive || r.owned == ownsUncertain) {
		r.cancelIdleLocked()
		r.beginStopLocked(causeStopRequested)
	}
	return r.lastStop
}

// holdKeptLocked holds a kept route, so nothing but `mesh serve start` or a
// change of its definition starts it again.
func (r *demandRoute) holdKeptLocked() {
	if !r.service.Demand.Kept() {
		return
	}
	r.held = true
	r.waiting = nil
	r.restartAt = time.Time{}
	if r.pending == nil && r.owned == ownsNothing {
		r.state = protocol.DemandStopped
		r.failure = ""
	}
}

func (r *demandRoute) prepareAdoptionStopLocked() error {
	if r.manager.shuttingDown() {
		return fmt.Errorf("route %s is shut down", r.service.Route())
	}
	if r.owned != ownsUnknown {
		return nil
	}
	r.stopRequested = true
	if r.pending != nil {
		return nil
	}
	if !r.adoptionDueLocked() {
		return r.adoptionBlockedLocked()
	}
	r.beginAdoptLocked()
	return nil
}

func (r *demandRoute) beginAdoptLocked() {
	if r.manager.closed.Load() || r.pending != nil || r.owned != ownsUnknown || !r.adoptionDueLocked() {
		return
	}
	transition := &demandTransition{adopting: true, done: make(chan struct{})}
	r.pending = transition
	r.state = protocol.DemandStarting
	label := r.adoptLabel
	r.manager.goroutines.Add(1)
	go r.runAdopt(transition, label)
}

func (r *demandRoute) runAdopt(transition *demandTransition, label string) {
	defer r.manager.goroutines.Done()
	ctx, cancel := context.WithTimeout(r.manager.ctx, 5*time.Second)
	defer cancel()
	id, found, err := r.manager.sessions.findLabelled(ctx, label)
	r.mu.Lock()
	if r.pending != transition {
		transition.err = fmt.Errorf("route %s adoption was cancelled", r.service.Route())
		r.unlock()
		close(transition.done)
		return
	}
	if r.manager.closed.Load() {
		r.pending = nil
		if err == nil {
			r.finishAdoptionLocked(label, id, found)
		}
		transition.err = fmt.Errorf("route %s adoption was cancelled", r.service.Route())
		r.unlock()
		close(transition.done)
		return
	}
	if err != nil {
		r.pending = nil
		r.markAdoptionUnknownLocked(label, err)
		transition.err = fmt.Errorf("route %s: find its running session: %w", r.service.Route(), err)
		r.unlock()
		close(transition.done)
		return
	}
	adopted := r.finishAdoptionLocked(label, id, found)
	r.continueLocked()
	route := r.service.Route()
	r.unlock()
	if adopted {
		r.manager.logEvent("route %s: adopted session %s from an earlier daemon", route, id)
	}
	close(transition.done)
}

// finishAdoptionLocked records what discovery found. It reports whether the
// route adopted a live session to keep serving.
func (r *demandRoute) finishAdoptionLocked(label, id string, found bool) bool {
	r.pending = nil
	r.adoptIdlePending = false
	r.failure = ""
	if !found {
		r.owned = ownsNothing
		r.stopRequested = false
		r.adoptLabel = ""
		r.adoptRetryAt = time.Time{}
		r.state = protocol.DemandStopped
		return false
	}
	r.state = protocol.DemandRunning
	r.owned = ownsLive
	r.sessionID = id
	r.adoptRetryAt = time.Time{}
	if r.removed || r.restart || r.stopRequested || r.service.Demand == nil || r.service.Label() != label {
		r.adoptLabel = label
		r.owned = ownsUncertain
		r.retryAt = time.Time{}
		r.blocked = nil
		r.state = protocol.DemandStopping
		r.stopCause = adoptedStopCause(r.removed, r.stopRequested)
		return false
	}
	r.adoptLabel = ""
	r.adoptIdlePending = true
	// The stability clock of an adopted session starts now: this daemon has
	// not seen how long it ran before.
	r.readyAt = r.manager.now()
	r.readyID = id
	return true
}

func adoptedStopCause(removed, stopRequested bool) demandCause {
	switch {
	case removed:
		return causeRemoved
	case stopRequested:
		return causeStopRequested
	default:
		return causeRedefined
	}
}

func (r *demandRoute) armAdoptedIdleLocked() {
	if !r.adoptIdlePending || r.manager.closed.Load() {
		return
	}
	r.adoptIdlePending = false
	if r.owned == ownsLive && r.pending == nil && !r.removed && !r.stopRequested && r.connections == 0 {
		r.armIdleLocked()
	}
}

// startableLocked is the one admission every start passes, whether traffic,
// the kept supervisor or `mesh serve start` asked: the route owns nothing,
// nothing is in progress, it is neither removed nor held, the daemon is not
// shutting down, and a kept route's restart delay has passed.
func (r *demandRoute) startableLocked() bool {
	return r.service.Demand != nil && !r.removed && !r.held && r.pending == nil && r.owned == ownsNothing &&
		!r.manager.shuttingDown() && !r.manager.now().Before(r.restartAt)
}

// beginStartLocked starts the route when admission allows it, and reports
// whether it did.
func (r *demandRoute) beginStartLocked(cause demandCause) bool {
	if !r.startableLocked() || !r.manager.admit() {
		return false
	}
	r.adoptIdlePending = false
	r.startRequested = false
	r.waiting = nil
	transition := &demandTransition{starting: true, done: make(chan struct{})}
	r.pending = transition
	r.state = protocol.DemandStarting
	r.failure = ""
	r.restart = false
	service := r.service
	r.manager.goroutines.Add(1)
	go r.runStart(transition, service, cause)
	return true
}

func (r *demandRoute) runStart(transition *demandTransition, service meshserve.Service, cause demandCause) {
	manager := r.manager
	defer manager.goroutines.Done()
	command := []string{hostShell(), "-lc", service.Demand.Command}
	id, err := manager.sessions.startLabelled(manager.ctx, service.Label(), command, service.Demand.Cwd, service.Demand.Env)
	// The launch phase ends once the worker's identity is known: Close waits
	// for that, never for readiness.
	manager.finishPhase()
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
	r.finishStart(transition, id, err, cause)
	// Each start is a new session. Earlier ones for this route are pruned to
	// a bounded history, keeping the latest attempts and a run that lasted;
	// their output is what explains an outage.
	if removed := manager.sessions.pruneLabelled(manager.ctx, service.Label(), id); len(removed) > 0 {
		manager.logEvent("route %s: removed ended sessions %s (%s)", service.Route(), joinSessionIDs(removed), labelledHistoryPolicy)
	}
}

func (r *demandRoute) finishStart(transition *demandTransition, id string, err error, cause demandCause) {
	r.mu.Lock()
	r.pending = nil
	if id != "" {
		r.sessionID = id
	}
	route := r.service.Route()
	started := false
	switch {
	case err != nil:
		r.failStartLocked(id, err)
		transition.err = err
	case r.removed || r.stopRequested || r.restart:
		// The new session is stopped at once, unless the daemon is shutting
		// down, when it stays for the next daemon.
		r.state = protocol.DemandRunning
		r.owned = ownsLive
		started = true
		cause, superseded, err := r.supersededStartLocked()
		r.beginStopLocked(cause)
		transition.superseded, transition.err = superseded, err
	default:
		r.state = protocol.DemandRunning
		r.owned = ownsLive
		r.readyAt = r.manager.now()
		r.readyID = id
		started = true
		if r.connections == 0 {
			r.armIdleLocked()
		}
	}
	r.unlock()
	if started {
		r.manager.logEvent("route %s: started session %s (%s)", route, id, cause)
	}
	if transition.err != nil {
		transition.err = meshserve.LogDemandFailure(transition.err, r.manager.logger)
	}
	close(transition.done)
}

// failStartLocked records a failed start. A kept route schedules its restart
// unless the daemon is shutting down.
func (r *demandRoute) failStartLocked(id string, err error) {
	r.state = protocol.DemandFailed
	r.failure = err.Error()
	r.readyID = ""
	if failedStartOwnership(id, err) == ownsUncertain {
		r.markUncertainLocked()
		r.stopCause = causeFailedStart
	} else {
		r.owned = ownsNothing
		r.stopRequested = false
	}
	var failure demandFailure
	if errors.As(err, &failure) {
		r.failure = failure.summary
	}
	if r.service.Demand.Kept() && !r.held && !r.manager.shuttingDown() {
		r.scheduleRestartLocked(false)
		r.failure += "; restarts at " + r.restartAt.Format(time.TimeOnly)
	}
}

// supersededStartLocked is why a start that succeeded after its route was
// removed, stopped or redefined is stopped, and what its waiters get.
func (r *demandRoute) supersededStartLocked() (demandCause, bool, error) {
	switch {
	case r.removed:
		return causeRemoved, false, fmt.Errorf("route %s was removed while it started", r.service.Route())
	case r.stopRequested && r.startRequested:
		// The Stop finishes first. A Start accepted after it is the latest
		// intent, so this start's waiters follow the start that comes next.
		return causeStopRequested, true, nil
	case r.stopRequested:
		return causeStopRequested, false, fmt.Errorf("route %s was stopped while it started", r.service.Route())
	default:
		return causeRedefined, false, fmt.Errorf("route %s changed while it started; retry", r.service.Route())
	}
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

// beginStopLocked stops the owned session, unless shutdown has fenced off
// new kills; then the session stays owned for the next daemon.
func (r *demandRoute) beginStopLocked(cause demandCause) {
	if !r.manager.admit() {
		return
	}
	r.adoptIdlePending = false
	id := r.sessionID
	transition := &demandTransition{sessionID: id, done: make(chan struct{})}
	r.pending = transition
	r.state = protocol.DemandStopping
	r.stopCause = cause
	route := r.service.Route()
	r.manager.goroutines.Go(func() {
		ctx, cancel := context.WithTimeout(r.manager.ctx, demandStopTimeout)
		err := r.manager.sessions.stopSession(ctx, id)
		cancel()
		r.manager.finishPhase()
		if err != nil {
			err = fmt.Errorf("daemon: route %s: stop session %s: %w", route, id, err)
			r.manager.report(err)
		}
		r.mu.Lock()
		r.finishStopLocked(err)
		transition.err = err
		r.unlock()
		r.manager.logStop(route, id, cause, err)
		close(transition.done)
	})
}

// finishStopLocked records how a stop ended. Only a confirmed stop proves
// the session gone; a failed one keeps it owned and retries it later.
func (r *demandRoute) finishStopLocked(err error) {
	r.pending = nil
	r.stopRequested = false
	r.state = protocol.DemandStopped
	r.owned = ownsNothing
	if err != nil {
		r.state = protocol.DemandFailed
		r.failure = err.Error()
		r.markUncertainLocked()
		return
	}
	r.adoptLabel = ""
	r.stopCause = ""
	r.readyID = ""
	r.continueLocked()
}

// continueLocked begins what the route still wants once it owns nothing: a
// Start that waited for a stop, or a kept route's next run.
func (r *demandRoute) continueLocked() {
	if r.owned != ownsNothing || r.pending != nil {
		return
	}
	if r.startRequested {
		r.beginStartLocked(causeExplicitStart)
		return
	}
	if r.service.Demand.Kept() {
		r.beginStartLocked(r.keptCauseLocked())
	}
}

func (r *demandRoute) armIdleLocked() {
	r.cancelIdleLocked()
	if r.service.Demand == nil || r.service.Demand.Kept() || r.state != protocol.DemandRunning || r.pending != nil || r.manager.shuttingDown() {
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

// idleExpired stops a finite route whose idle window passed with no counted
// connection. The token, the policy and the daemon are all checked again: a
// callback queued before a change must not stop a route that is now kept.
func (r *demandRoute) idleExpired(token uint64) {
	r.mu.Lock()
	defer r.unlock()
	if token != r.idleToken || r.connections > 0 || r.state != protocol.DemandRunning || r.pending != nil || r.owned != ownsLive {
		return
	}
	if r.service.Demand == nil || r.service.Demand.Kept() || r.manager.shuttingDown() {
		return
	}
	r.idleTimer = nil
	r.beginStopLocked(idleCause(r.service.Demand.Idle))
}

// checkSession notices a running session that ended on its own: a crash, or
// someone running `mesh kill`. A finite route starts again on its next
// connection; a kept route restarts after its backoff delay.
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
	if r.state != protocol.DemandRunning || r.pending != nil || r.sessionID != id {
		r.unlock()
		return
	}
	r.cancelIdleLocked()
	r.state = protocol.DemandStopped
	r.owned = ownsNothing
	description := exitDescription(code)
	next := "the next connection through Mesh starts it again"
	if r.service.Demand.Kept() && !r.manager.shuttingDown() {
		stable := r.readyID == id && !r.readyAt.IsZero() && r.manager.now().Sub(r.readyAt) >= keptStableRun
		delay := r.scheduleRestartLocked(stable)
		r.state = protocol.DemandFailed
		r.failure = fmt.Sprintf("%s while serving (session %s); restarts at %s", description, id, r.restartAt.Format(time.TimeOnly))
		next = "restarting in " + delay.String()
	} else if code == nil || *code != 0 {
		r.state = protocol.DemandFailed
		r.failure = fmt.Sprintf("%s while serving (session %s)", description, id)
	}
	r.readyID = ""
	route := r.service.Route()
	r.unlock()
	r.manager.logEvent("route %s: session %s ended (%s); %s", route, id, description, next)
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
	status := &protocol.ServiceDemand{State: r.state, Connections: r.connections, SessionID: r.sessionID, Held: r.held}
	if r.service.Demand == nil {
		status.State = ""
		status.SessionID = ""
		status.Held = false
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
