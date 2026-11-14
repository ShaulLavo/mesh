package transporttrace

import (
	"sync"
	"sync/atomic"
	"time"
)

const MaximumEvents = 4096
const MaximumBytes = 2 << 20
const MaximumRecord = 1024
const QueueSize = 256

type Identity struct {
	PID     int    `json:"pid"`
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Digest  string `json:"digest"`
}

type event struct {
	Schema        int       `json:"schema"`
	Sequence      uint64    `json:"sequence"`
	UnixNano      int64     `json:"unixNano"`
	MonotonicNano int64     `json:"monotonicNano"`
	Stage         Stage     `json:"stage"`
	Connection    uint64    `json:"connection,omitempty"`
	Remote        string    `json:"remote,omitempty"`
	Local         string    `json:"local,omitempty"`
	Session       string    `json:"session,omitempty"`
	Error         string    `json:"error,omitempty"`
	Context       string    `json:"context,omitempty"`
	Identity      *Identity `json:"identity,omitempty"`
	Dropped       uint64    `json:"dropped"`
}

type recorderState struct {
	queue       chan event
	stop        chan struct{}
	done        chan struct{}
	start       time.Time
	sequence    atomic.Uint64
	connections atomic.Uint64
	dropped     atomic.Uint64
	closing     atomic.Bool
	once        sync.Once
}

func (r *Recorder) stamp(value event) event {
	value.Schema, value.Sequence = 1, r.sequence.Add(1)
	value.UnixNano, value.MonotonicNano = timestamp(), time.Since(r.start).Nanoseconds()
	value.Dropped = r.dropped.Load()
	return value
}

func (r *Recorder) emit(value event) {
	if r.closing.Load() {
		return
	}
	value = r.stamp(value)
	if value.Sequence >= MaximumEvents {
		r.dropped.Add(1)
		return
	}
	select {
	case r.queue <- value:
	default:
		r.dropped.Add(1)
	}
}

func (r *Recorder) recordPanic(value any) {
	if value != nil {
		r.dropped.Add(1)
	}
}

// Close runs after server teardown. A stuck diagnostic disk cannot hold shutdown.
func (r *Recorder) Close() {
	if r == nil {
		return
	}
	r.once.Do(func() { r.closing.Store(true); close(r.stop) })
	timer := time.NewTimer(20 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-r.done:
	case <-timer.C:
	}
}

// Done closes after the owned file has stopped writing.
func (r *Recorder) Done() <-chan struct{} { return r.done }
