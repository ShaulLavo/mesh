package daemon

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	meshserve "github.com/shaul/mesh/internal/serve"
)

const (
	// keptRestartFirst and keptRestartMax bound how soon a kept route starts
	// again after an exit or a failed start: 1s, doubling, at most 5m.
	keptRestartFirst = time.Second
	keptRestartMax   = 5 * time.Minute
	// keptRestartSteps is the failure count whose delay first reaches the
	// cap. The count saturates there, so the shift below stays small and a
	// crash loop of any length waits the full cap.
	keptRestartSteps = 10
	// keptStableRun is how long a session must have run for its exit to
	// start the backoff over.
	keptStableRun = time.Minute
)

// keptRestartDelay is the wait before restart number failures in a row.
func keptRestartDelay(failures int) time.Duration {
	failures = min(max(failures, 1), keptRestartSteps)
	return min(keptRestartFirst<<(failures-1), keptRestartMax)
}

// scheduleRestartLocked records one more failure of a kept route and when it
// may start again. A stable run before the failure starts the count over.
func (r *demandRoute) scheduleRestartLocked(stable bool) time.Duration {
	if stable {
		r.restartFailures = 0
	}
	r.restartFailures = min(r.restartFailures+1, keptRestartSteps)
	delay := keptRestartDelay(r.restartFailures)
	r.restartAt = r.manager.now().Add(delay)
	r.waiting = nil
	return delay
}

// resetRestartLocked forgets a kept route's failures: `mesh serve start` and
// a change of the route's definition start over.
func (r *demandRoute) resetRestartLocked() {
	r.restartAt = time.Time{}
	r.restartFailures = 0
	r.waiting = nil
}

func (r *demandRoute) keptCauseLocked() demandCause {
	if r.restartFailures > 0 {
		return causeKeptRestart
	}
	return causeKeptStartup
}

// demandCause is the fixed reason a route started or stopped a session. It
// never carries a command, its output, its environment or a peer, so the
// lifecycle log is safe to read and share.
type demandCause string

const (
	causeTailnetRequest demandCause = "a tailnet request"
	causeExplicitStart  demandCause = "mesh serve start"
	causeKeptStartup    demandCause = "kept route startup"
	causeKeptRestart    demandCause = "kept route restart"
	causeStopRequested  demandCause = "mesh serve stop"
	causeRedefined      demandCause = "route definition changed"
	causeRemoved        demandCause = "route removed"
	causeFailedStart    demandCause = "cleanup after a failed start"
	causeReadiness      demandCause = "readiness failure cleanup"
	causeCleanup        demandCause = "cleanup of a session that may still be running"
)

func listenerCause(port uint16) demandCause {
	return demandCause(fmt.Sprintf("a connection to listener port %d", port))
}

func idleCause(window time.Duration) demandCause {
	return demandCause(fmt.Sprintf("idle for %s with no Mesh connections", window))
}

// labelledHistoryLimit and labelledLongRun bound what a route keeps of its
// ended sessions: the newest few attempts, and the newest one that ran long
// enough to have served, which a crash loop would otherwise push out.
const (
	labelledHistoryLimit  = 10
	labelledLongRun       = time.Minute
	labelledHistoryPolicy = "history beyond the newest 10 attempts and the newest run of 1m0s or longer"
)

func joinSessionIDs(ids []string) string {
	return strings.Join(ids, " ")
}

// demandWaitingAnswer is the one answer requests to a kept route get while
// it is held or waiting to restart. It is logged once, outside the route
// lock, and every request in that state shares its reference.
type demandWaitingAnswer struct {
	once   sync.Once
	cause  error
	logged error
}

func (a *demandWaitingAnswer) answer(logger *log.Logger) error {
	a.once.Do(func() { a.logged = meshserve.LogDemandFailure(a.cause, logger) })
	return a.logged
}

// waitingLocked is the answer for a kept route that may not start now:
// stopped by `mesh serve stop`, or within its restart delay. Traffic neither
// clears nor extends either; only `mesh serve start` overrides them.
func (r *demandRoute) waitingLocked() *demandWaitingAnswer {
	if !r.service.Demand.Kept() {
		return nil
	}
	route := r.service.Route()
	var cause error
	switch {
	case r.held:
		cause = fmt.Errorf("route %s was stopped by mesh serve stop; start it with mesh serve start %s", route, route)
	case r.manager.now().Before(r.restartAt):
		cause = fmt.Errorf("route %s is waiting to restart its command; it restarts at %s", route, r.restartAt.Format(time.TimeOnly))
	default:
		return nil
	}
	if r.waiting == nil {
		r.waiting = &demandWaitingAnswer{cause: cause}
	}
	return r.waiting
}

// demandAdmission is the shutdown fence for launches and kills. Each launch
// or kill phase runs only once admitted, and Close waits for every admitted
// phase. Its mutex is a leaf: route and manager locks may be held when it is
// taken, and nothing waits while holding it.
type demandAdmission struct {
	mu     sync.Mutex
	closed bool
	phases sync.WaitGroup
}

// admit opens one launch or kill phase, unless the daemon is shutting down.
func (m *demandManager) admit() bool {
	m.admission.mu.Lock()
	defer m.admission.mu.Unlock()
	if m.admission.closed || m.ctx.Err() != nil {
		return false
	}
	m.admission.phases.Add(1)
	return true
}

func (m *demandManager) finishPhase() { m.admission.phases.Done() }

// shuttingDown reports whether Close ran or the daemon context ended. Either
// one forbids new launches and kills; the workers keep running.
func (m *demandManager) shuttingDown() bool {
	return m.closed.Load() || m.ctx.Err() != nil
}

// logEvent writes one completed route transition to the daemon log. Callers
// hold no route lock: a slow log must not stall the route or its listeners.
func (m *demandManager) logEvent(format string, args ...any) {
	m.logger.Printf("daemon: "+format, args...)
}

// logStop reports a stop's outcome once stopSession has answered. Success
// is claimed only when the session was confirmed gone.
func (m *demandManager) logStop(route, id string, cause demandCause, err error) {
	if err != nil {
		m.logEvent("route %s: stopping session %s failed (%s); it may still be running and is stopped again later", route, id, cause)
		return
	}
	m.logEvent("route %s: stopped session %s (%s)", route, id, cause)
}
