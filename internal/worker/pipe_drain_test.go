package worker

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/session/execution"
)

// hangCap only catches a stuck test; no assertion depends on it elapsing.
const hangCap = 30 * time.Second

// pipeDrainStream is one pipe reader feeding a real capture, with the pipe's
// write end standing in for the leader and its descendants.
type pipeDrainStream struct {
	reader *pipeReader
	writes *os.File
}

type pipeDrainFixture struct {
	root    string
	binding execution.Binding
	streams []*pipeDrainStream
	stop    context.CancelFunc
	running sync.WaitGroup
}

// spawn runs fn on a goroutine the fixture waits for before the test's
// temporary directory is removed.
func (f *pipeDrainFixture) spawn(fn func()) { f.running.Go(fn) }

// release stops everything the fixture started and waits for it, so nothing is
// still writing into the temporary directory when it is removed. Capture
// finishes after abort because its write loop exits on the abort and Finish
// waits for that exit.
func (f *pipeDrainFixture) release() {
	f.stop()
	for _, s := range f.streams {
		_ = s.reader.file.Close()
		_ = s.writes.Close()
		s.reader.capture.Abort()
	}
	f.running.Wait()
	for _, s := range f.streams {
		_, _ = s.reader.capture.Finish(context.Background(), "interrupted")
	}
}

func newPipeDrainFixture(t *testing.T, names ...string) *pipeDrainFixture {
	t.Helper()
	root := t.TempDir()
	ref, err := session.NewReservation("7ABC", "boot", session.PipeExecution)
	if err != nil {
		t.Fatal(err)
	}
	binding := execution.NewBinding(execution.Key{Host: "host", Class: "local-account", Principal: "account", Opaque: "pipe-drain"},
		execution.Intent{Kind: execution.KindPipe, Stdin: execution.StdinClosed}, ref, nil)
	ctx, stop := context.WithCancel(t.Context())
	f := &pipeDrainFixture{root: root, binding: binding, stop: stop}
	for _, name := range names {
		capture, err := execution.OpenCapture(root, binding, name)
		if err != nil {
			t.Fatal(err)
		}
		reads, writes, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		s := &pipeDrainStream{writes: writes, reader: &pipeReader{file: reads, capture: capture, done: make(chan string, 1)}}
		f.streams = append(f.streams, s)
		f.spawn(func() { s.reader.read(ctx) })
	}
	// Registered after the temporary directory, so it runs before its removal.
	t.Cleanup(f.release)
	return f
}

func (f *pipeDrainFixture) readers() []*pipeReader {
	readers := make([]*pipeReader, len(f.streams))
	for i, s := range f.streams {
		readers[i] = s.reader
	}
	return readers
}

func (f *pipeDrainFixture) drain(limits pipeDrainLimits) []string {
	return drainPipe(f.readers(), nil, nil, f.stop, limits)
}

// stallStream holds the stream's capture lock so its commits cannot finish.
func (f *pipeDrainFixture) stallStream(t *testing.T, stream string) *execution.Fence {
	t.Helper()
	files, err := execution.OpenFiles(f.root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = files.Close() })
	name := filepath.Join("execution", "attempts", f.binding.Reservation.Instance, stream, "stream.lock")
	stall, err := files.LockExisting(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	return stall
}

// A loaded disk makes the capture slower than the child, so the leader can exit
// while the reader still holds bytes the leader wrote. Those bytes belong in the
// result while the capture is making progress.
func TestPipeDrainKeepsOutputTheLeaderWroteWhileTheCaptureCatchesUp(t *testing.T) {
	f := newPipeDrainFixture(t, execution.StreamStdout)
	stall := f.stallStream(t, execution.StreamStdout)
	payload := bytes.Repeat([]byte("x"), 4<<20)
	f.spawn(func() {
		_, _ = f.streams[0].writes.Write(payload)
		_ = f.streams[0].writes.Close()
	})
	silence := 50 * time.Millisecond
	// The stall outlasts the silence window many times over, but stays inside
	// the capture's own lock wait so the commit succeeds once it ends.
	time.AfterFunc(8*silence, func() { _ = stall.Close() })
	reasons := f.drain(pipeDrainLimits{silence: silence, total: time.Minute})
	if reasons[0] != "eof" {
		t.Fatalf("drain reason = %q, want eof", reasons[0])
	}
	summary, err := f.streams[0].reader.capture.Finish(t.Context(), reasons[0])
	if err != nil {
		t.Fatal(err)
	}
	if !summary.EOF || summary.Loss != "" || summary.ObservedEnd != strconv.Itoa(len(payload)) {
		t.Fatalf("capture summary = %+v, want eof with %d bytes and no loss", summary, len(payload))
	}
}

// A capture that stays blocked past the drain bound loses the bytes the reader
// holds, and the final flush reports that instead of claiming a result.
func TestPipeDrainReportsNotRecordedWhileTheCaptureStaysBlocked(t *testing.T) {
	f := newPipeDrainFixture(t, execution.StreamStdout)
	// The handle must stay referenced: a collected Fence closes its file, which
	// releases the lock and lets the commit through.
	stall := f.stallStream(t, execution.StreamStdout)
	t.Cleanup(func() { _ = stall.Close() })
	runtime.GC() // a lost reference would release the lock here, on every platform
	f.spawn(func() { _, _ = f.streams[0].writes.Write(bytes.Repeat([]byte("x"), 4<<20)) })
	reasons := f.drain(pipeDrainLimits{silence: time.Minute, total: 200 * time.Millisecond})
	if reasons[0] != pipeDrainCutoff {
		t.Fatalf("drain reason = %q, want %q", reasons[0], pipeDrainCutoff)
	}
	result := execution.Result{Capture: captureComplete}
	finishPipeCapture(f.readers(), reasons, &result, 200*time.Millisecond)
	if result.Capture != execution.CaptureNotRecorded || len(result.Streams) != 0 {
		t.Fatalf("result = capture %q with %d streams, want %q with none", result.Capture, len(result.Streams), execution.CaptureNotRecorded)
	}
}

func TestPipeDrainCutsOffADescendantThatStaysSilent(t *testing.T) {
	f := newPipeDrainFixture(t, execution.StreamStdout)
	if _, err := f.streams[0].writes.Write([]byte("leader")); err != nil {
		t.Fatal(err)
	}
	// The write end stays open, as a descendant holding the descriptor would.
	if reasons := f.drain(pipeDrainLimits{silence: 50 * time.Millisecond, total: time.Minute}); reasons[0] != pipeDrainCutoff {
		t.Fatalf("drain reason = %q, want %q", reasons[0], pipeDrainCutoff)
	}
}

func TestPipeDrainCutsOffADescendantThatKeepsWriting(t *testing.T) {
	f := newPipeDrainFixture(t, execution.StreamStdout)
	f.spawn(func() {
		for {
			if _, err := f.streams[0].writes.Write([]byte("x")); err != nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
	})
	if reasons := f.drain(pipeDrainLimits{silence: time.Minute, total: 200 * time.Millisecond}); reasons[0] != pipeDrainCutoff {
		t.Fatalf("drain reason = %q, want %q", reasons[0], pipeDrainCutoff)
	}
}

// keepBusy writes a byte every few milliseconds until stop closes or the
// descriptor does, then closes the write end like a finished descendant.
func (f *pipeDrainFixture) keepBusy(s *pipeDrainStream, stop <-chan struct{}) {
	f.spawn(func() {
		defer func() { _ = s.writes.Close() }()
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
			if _, err := s.writes.Write([]byte("x")); err != nil {
				return
			}
		}
	})
}

// closed reports whether the descriptor has been closed by its cutoff.
func closed(file *os.File) bool {
	_, err := file.Stat()
	return errors.Is(err, os.ErrClosed)
}

func awaitClosed(t *testing.T, file *os.File, what string) {
	t.Helper()
	deadline := time.Now().Add(hangCap)
	for !closed(file) {
		if time.Now().After(deadline) {
			t.Fatalf("%s was not cut off while the other stream stayed busy", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Each descriptor has its own silence window, whichever stream is silent and
// whichever the other stream keeps busy.
func TestPipeDrainCutsOffASilentStreamWhileTheOtherStaysBusy(t *testing.T) {
	for _, silent := range []string{execution.StreamStdout, execution.StreamStderr} {
		t.Run(silent+" silent", func(t *testing.T) {
			busy := execution.StreamStderr
			if silent == execution.StreamStderr {
				busy = execution.StreamStdout
			}
			f := newPipeDrainFixture(t, silent, busy)
			quiet, active := f.streams[0], f.streams[1]
			stopWriting := make(chan struct{})
			f.keepBusy(active, stopWriting)
			reasons := make(chan []string, 1)
			f.spawn(func() { reasons <- f.drain(pipeDrainLimits{silence: 200 * time.Millisecond, total: time.Minute}) })
			awaitClosed(t, quiet.reader.file, silent)
			if _, err := quiet.writes.Write([]byte("late")); err == nil {
				t.Fatal("a silent stream accepted bytes after its cutoff")
			}
			close(stopWriting)
			select {
			case got := <-reasons:
				if got[0] != pipeDrainCutoff {
					t.Fatalf("%s reason = %q, want %q", silent, got[0], pipeDrainCutoff)
				}
			case <-time.After(hangCap):
				t.Fatal("drain did not finish")
			}
		})
	}
}

// Line mode watches stdout through the line reader, which has its own window.
func TestPipeDrainCutsOffSilentLineStdoutWhileStderrStaysBusy(t *testing.T) {
	f := newPipeDrainFixture(t, execution.StreamStderr)
	active := f.streams[0]
	stdout, held, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdout.Close(); _ = held.Close() })
	stopWriting := make(chan struct{})
	f.keepBusy(active, stopWriting)
	lineDone := make(chan struct{})
	finished := make(chan struct{})
	f.spawn(func() {
		defer close(finished)
		drainPipe(f.readers(), stdout, lineDone, f.stop, pipeDrainLimits{silence: 200 * time.Millisecond, total: time.Minute})
	})
	awaitClosed(t, stdout, "line stdout")
	close(stopWriting)
	select {
	case <-finished:
	case <-time.After(hangCap):
		t.Fatal("drain did not finish")
	}
}
