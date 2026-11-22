package worker

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/session/execution"
)

// pipeDrainFixture is one pipe reader feeding a real capture, with the pipe's
// write end standing in for the leader and its descendants.
type pipeDrainFixture struct {
	root    string
	binding execution.Binding
	reader  *pipeReader
	writes  *os.File
	stop    context.CancelFunc
}

func newPipeDrainFixture(t *testing.T) *pipeDrainFixture {
	t.Helper()
	root := t.TempDir()
	ref, err := session.NewReservation("7ABC", "boot", session.PipeExecution)
	if err != nil {
		t.Fatal(err)
	}
	binding := execution.NewBinding(execution.Key{Host: "host", Class: "local-account", Principal: "account", Opaque: "pipe-drain"},
		execution.Intent{Kind: execution.KindPipe, Stdin: execution.StdinClosed}, ref, nil)
	capture, err := execution.OpenCapture(root, binding, execution.StreamStdout)
	if err != nil {
		t.Fatal(err)
	}
	reads, writes, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(t.Context())
	f := &pipeDrainFixture{root: root, binding: binding, writes: writes, stop: stop,
		reader: &pipeReader{file: reads, capture: capture, done: make(chan string, 1)}}
	t.Cleanup(func() {
		stop()
		_ = reads.Close()
		_ = writes.Close()
		capture.Abort()
	})
	go f.reader.read(ctx)
	return f
}

func (f *pipeDrainFixture) drain(limits pipeDrainLimits) string {
	reasons := drainPipe([]*pipeReader{f.reader}, f.reader.file, nil, f.stop, limits)
	return reasons[0]
}

// A loaded disk makes the capture slower than the child, so the leader can exit
// while the reader still holds bytes the leader wrote. Those bytes belong in the
// result however long the capture takes to commit them.
func TestPipeDrainKeepsOutputTheLeaderWroteWhileTheCaptureCatchesUp(t *testing.T) {
	f := newPipeDrainFixture(t)
	files, err := execution.OpenFiles(f.root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = files.Close() }()
	name := filepath.Join("execution", "attempts", f.binding.Reservation.Instance, execution.StreamStdout, "stream.lock")
	stall, err := files.LockExisting(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("x"), 4<<20)
	go func() {
		_, _ = f.writes.Write(payload)
		_ = f.writes.Close()
	}()
	silence := 50 * time.Millisecond
	// The stall outlasts the silence window many times over, but stays inside
	// the capture's own lock wait so the commit succeeds once it ends.
	time.AfterFunc(8*silence, func() { _ = stall.Close() })
	reason := f.drain(pipeDrainLimits{silence: silence, total: time.Minute})
	if reason != "eof" {
		t.Fatalf("drain reason = %q, want eof", reason)
	}
	summary, err := f.reader.capture.Finish(t.Context(), reason)
	if err != nil {
		t.Fatal(err)
	}
	if !summary.EOF || summary.Loss != "" || summary.ObservedEnd != strconv.Itoa(len(payload)) {
		t.Fatalf("capture summary = %+v, want eof with %d bytes and no loss", summary, len(payload))
	}
}

func TestPipeDrainCutsOffADescendantThatStaysSilent(t *testing.T) {
	f := newPipeDrainFixture(t)
	if _, err := f.writes.Write([]byte("leader")); err != nil {
		t.Fatal(err)
	}
	// The write end stays open, as a descendant holding the descriptor would.
	if reason := f.drain(pipeDrainLimits{silence: 50 * time.Millisecond, total: time.Minute}); reason != pipeDrainCutoff {
		t.Fatalf("drain reason = %q, want %q", reason, pipeDrainCutoff)
	}
}

func TestPipeDrainCutsOffADescendantThatKeepsWriting(t *testing.T) {
	f := newPipeDrainFixture(t)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for {
			if _, err := f.writes.Write([]byte("x")); err != nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	if reason := f.drain(pipeDrainLimits{silence: time.Minute, total: 200 * time.Millisecond}); reason != pipeDrainCutoff {
		t.Fatalf("drain reason = %q, want %q", reason, pipeDrainCutoff)
	}
	_ = f.writes.Close()
	<-finished
}
