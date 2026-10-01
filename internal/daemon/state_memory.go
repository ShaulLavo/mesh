package daemon

import (
	"context"
	"github.com/shaul/mesh/internal/protocol"
	"time"
)

func (b *stateBroker) runMemory(ctx context.Context, l *lifecycle) {
	var timer *time.Timer
	var tick <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	reset := func() {
		if !b.hasTopic(protocol.TopicSessions) {
			if timer != nil {
				timer.Stop()
			}
			timer = nil
			tick = nil
			return
		}
		if tick != nil {
			return
		}
		timer = time.NewTimer(memorySampleTTL)
		tick = timer.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.activity:
			reset()
		case <-tick:
			timer = nil
			tick = nil
			b.sampleMemory(l)
			reset()
		}
	}
}
func (b *stateBroker) sampleMemory(l *lifecycle) {
	if !b.hasTopic(protocol.TopicSessions) {
		return
	}
	rows := b.memoryRows()
	sizes := l.memory.sizesFor(l.sessionsDir, rows, time.Now())
	l.memory.mu.Lock()
	sampled := l.memory.sampled
	l.memory.mu.Unlock()
	b.memoryChanged(sizes, sampled)
}
