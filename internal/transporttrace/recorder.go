package transporttrace

import (
	"encoding/json"
	"os"
	"strconv"
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

type Recorder struct {
	file        *os.File
	queue       chan event
	stop        chan struct{}
	done        chan struct{}
	start       time.Time
	sequence    atomic.Uint64
	connections atomic.Uint64
	dropped     atomic.Uint64
	closing     atomic.Bool
	once        sync.Once
	written     int
}

// New owns one private regular file. Failure disables diagnostics, never the server.
func New(directory string, identity Identity) *Recorder {
	if identity.PID <= 0 || !safeMetadata(identity.Version, 32) || !hexMetadata(identity.Commit) || !hexMetadata(identity.Digest) {
		return nil
	}
	entries, readErr := os.ReadDir(directory)
	if readErr != nil || len(entries) >= 32 {
		return nil
	}
	file, err := os.CreateTemp(directory, "lifecycle-"+strconv.Itoa(identity.PID)+"-*.jsonl")
	if err != nil {
		return nil
	}
	r := &Recorder{file: file, queue: make(chan event, QueueSize), stop: make(chan struct{}), done: make(chan struct{}), start: time.Now()}
	first := r.stamp(event{Stage: "process.begin", Identity: &identity})
	if !r.write(first) {
		_ = file.Close()
		return nil
	}
	go r.run()
	return r
}

func safeMetadata(value string, maximum int) bool {
	if len(value) == 0 || len(value) > maximum {
		return false
	}
	for _, ch := range value {
		if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '.' || ch == '-') {
			return false
		}
	}
	return true
}

func hexMetadata(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, ch := range value {
		if !(ch >= 'a' && ch <= 'f' || ch >= '0' && ch <= '9') {
			return false
		}
	}
	return true
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

func (r *Recorder) write(value event) bool {
	data, err := json.Marshal(value)
	if err != nil || len(data) >= MaximumRecord || r.written+len(data)+1 > MaximumBytes {
		return false
	}
	data = append(data, '\n')
	n, err := r.file.Write(data)
	r.written += n
	return err == nil && n == len(data)
}

func (r *Recorder) run() {
	defer close(r.done)
	defer r.file.Close() //nolint:errcheck // observer failures only invalidate coverage
	defer func() { r.recordPanic(recover()) }()
	for {
		var value event
		select {
		case value = <-r.queue:
		case <-r.stop:
			r.drain()
			return
		}
		if !r.write(value) {
			r.dropped.Add(1)
			return
		}
	}
}

func (r *Recorder) recordPanic(value any) {
	if value != nil {
		r.dropped.Add(1)
	}
}

func (r *Recorder) drain() {
	for {
		var value event
		select {
		case value = <-r.queue:
		default:
			_ = r.write(r.stamp(event{Stage: "process.end"}))
			return
		}
		if !r.write(value) {
			r.dropped.Add(1)
			return
		}
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
