package oneshot

import (
	"syscall"
	"time"
)

// cleanup is the run's single event loop. The deadline and cancellation stay
// live until the group is cleaned up, not only until the leader exits: a
// descendant holding a pipe after the leader is gone still meets the
// deadline. Once an escalation starts it always runs its full Grace before
// SIGKILL, because closed pipes and an exited leader say nothing about
// members that redirected their output.
type cleanup struct {
	group    int
	streams  []*stream
	exited   <-chan error
	deadline <-chan time.Time
	cancel   <-chan struct{}
	grace    <-chan time.Time
	drained  <-chan struct{}
	// stopDrain ends the drain early once the run is cancelled.
	stopDrain chan struct{}

	leaderGone, drainDone, escalated, killed, timedOut bool
	graceTimer                                         *time.Timer
}

func (c *cleanup) run() {
	defer c.stopGrace()
	for !c.finished() {
		select {
		case err := <-c.exited:
			c.leaderExited(err)
		case <-c.deadline:
			c.deadline, c.timedOut = nil, true
			c.escalate(syscall.SIGTERM)
		case <-c.cancel:
			c.cancel = nil
			close(c.stopDrain)
			c.escalate(syscall.SIGHUP)
		case <-c.grace:
			c.grace, c.killed = nil, true
			signalGroup(c.group, syscall.SIGKILL)
		case <-c.drained:
			c.drained, c.drainDone = nil, true
		}
	}
}

func (c *cleanup) finished() bool {
	return c.leaderGone && c.drainDone && (!c.escalated || c.killed)
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
	c.graceTimer = time.NewTimer(Grace)
	c.grace = c.graceTimer.C
}

func (c *cleanup) stopGrace() {
	if c.graceTimer != nil {
		c.graceTimer.Stop()
	}
}
