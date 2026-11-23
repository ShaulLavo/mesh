package oneshot

import (
	"syscall"
	"time"
)

// grace is how long the group has after the first signal before SIGKILL. A
// variable so tests can shorten it.
var grace = Grace

// drainStarting runs when the leader has exited and the drain begins; tests
// use it as a sync point.
var drainStarting = func() {}

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
	c.graceC, c.killed = nil, true
	signalGroup(c.group, syscall.SIGKILL)
	if c.cancelled {
		c.releaseDrain()
	}
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
		signalGroup(c.group, syscall.SIGKILL)
		c.drainDone, c.killed = true, true
		return
	}
	drainStarting()
	drained := make(chan struct{})
	c.drained = drained
	go func() {
		drain(c.streams, c.stopDrain)
		close(drained)
	}()
}

// escalate sends first to the group once and starts the grace.
func (c *cleanup) escalate(first syscall.Signal) {
	if c.escalated {
		return
	}
	c.escalated = true
	signalGroup(c.group, first)
	c.graceTimer = time.NewTimer(grace)
	c.graceC = c.graceTimer.C
}

func (c *cleanup) stopGrace() {
	if c.graceTimer != nil {
		c.graceTimer.Stop()
	}
}
