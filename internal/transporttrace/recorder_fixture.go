//go:build mesh_installed_fixture

package transporttrace

import (
	"encoding/json"
	"os"
	"strconv"
	"time"
)

type Recorder struct {
	recorderState
	file    *os.File
	written int
}

// Configure is used only by the explicitly enabled installed-fixture entry point.
func Configure(recorder *Recorder) { configured.Store(recorder) }

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
	r := &Recorder{file: file, recorderState: recorderState{queue: make(chan event, QueueSize), stop: make(chan struct{}), done: make(chan struct{}), start: time.Now()}}
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
