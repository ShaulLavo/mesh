package oneshot

import (
	"sync/atomic"
	"syscall"
	"time"
)

// grace is how long the group has after the first signal before SIGKILL. A
// variable so tests can shorten it.
var grace = Grace

// drainStartingHook runs when the leader has exited and the drain begins;
// tests install it as a sync point. Atomic, since another run's cleanup may
// be reading it while a test installs it.
var drainStartingHook atomic.Pointer[func()]

func drainStarting() {
	if hook := drainStartingHook.Load(); hook != nil {
		(*hook)()
	}
}

// cleanup is the run's single event loop. The deadline and cancellation stay
// live until the group is cleaned up, not only until the leader exits: a
// descendant holding a pipe after the leader is gone still meets the
// deadline. Once an escalation starts it always runs its full grace before
// SIGKILL, because closed pipes and an exited leader say nothing about
// members that redirected their output.
type cleanup struct {
	group    int
	streams  []*stream
	exited   <-chan error
	deadline <-chan time.Time
	cancel   <-chan struct{}
	graceC   <-chan time.Time
	drained  <-chan struct{}
	// stopDrain ends the drain of a cancelled run, but only once SIGKILL has
	// gone out: closing the pipes earlier would kill members with SIGPIPE
	// before their hang-up.
	stopDrain chan struct{}
	// gate holds the drain's pipe cutoffs from the first signal until
	// SIGKILL.
	gate *cutGate

	leaderGone, drainDone, escalated, killed, timedOut, cancelled, drainStopped bool
	graceTimer                                                                  *time.Timer
}

func (c *cleanup) run() {
	defer c.stopGrace()
	for !c.finished() || c.stopDue() {
		select {
		case err := <-c.exited:
			c.leaderExited(err)
		case <-c.deadline:
			c.onDeadline()
		case <-c.cancel:
			c.onCancel()
		case <-c.graceC:
			c.onGraceEnd()
		case <-c.drained:
			c.drained, c.drainDone = nil, true
		}
	}
}

func (c *cleanup) finished() bool {
	return c.leaderGone && c.drainDone && (!c.escalated || c.killed)
}

// stopDue handles a deadline or cancellation that is already due when the
// run would otherwise finish, so one that coincides with the end of the
// drain is still reported. It reports whether it found one.
func (c *cleanup) stopDue() bool {
	select {
	case <-c.deadline:
		c.onDeadline()
		return true
	case <-c.cancel:
		c.onCancel()
		return true
	default:
		return false
	}
}

func (c *cleanup) onDeadline() {
	c.deadline, c.timedOut = nil, true
	c.escalate(syscall.SIGTERM)
}

func (c *cleanup) onCancel() {
	c.cancel, c.cancelled = nil, true
	c.escalate(syscall.SIGHUP)
	if c.killed {
		c.releaseDrain()
	}
}

func (c *cleanup) onGraceEnd() {
	c.graceC = nil
	c.kill()
	if c.cancelled {
		c.releaseDrain()
	}
}

// kill sends SIGKILL to the group and hands the pipes back to the drain.
func (c *cleanup) kill() {
	c.killed = true
	signalGroup(c.group, syscall.SIGKILL)
	c.gate.kill()
}

func (c *cleanup) releaseDrain() {
	if !c.drainStopped {
		c.drainStopped = true
		close(c.stopDrain)
	}
}

// leaderExited starts the drain. If the leader cannot be observed the group
// is killed at once, since nothing else can tell when it ends.
func (c *cleanup) leaderExited(err error) {
	c.exited, c.leaderGone = nil, true
	if err != nil {
		c.kill()
		c.drainDone = true
		return
	}
	drainStarting()
	drained := make(chan struct{})
	c.drained = drained
	go func() {
		drain(c.streams, c.stopDrain, c.gate)
		close(drained)
	}()
}

// escalate sends first to the group once and starts the grace. The gate
// takes the pipes before the signal goes out.
func (c *cleanup) escalate(first syscall.Signal) {
	if c.escalated {
		return
	}
	c.escalated = true
	c.gate.escalate()
	signalGroup(c.group, first)
	c.graceTimer = time.NewTimer(grace)
	c.graceC = c.graceTimer.C
}

func (c *cleanup) stopGrace() {
	if c.graceTimer != nil {
		c.graceTimer.Stop()
	}
}
