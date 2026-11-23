package worker

import (
	"testing"
	"time"

	"github.com/shaul/mesh/internal/session/execution"
)

// The final save of each stream runs side by side: a stream whose capture is
// blocked does not hold the other stream's final save behind it, whichever
// stream is blocked.
func TestFinishPipeCaptureClosesStreamsSideBySide(t *testing.T) {
	t.Run(execution.StreamStdout, func(t *testing.T) { testStreamsSideBySide(t, execution.StreamStdout, execution.StreamStderr) })
	t.Run(execution.StreamStderr, func(t *testing.T) { testStreamsSideBySide(t, execution.StreamStderr, execution.StreamStdout) })
}

// testStreamsSideBySide holds blocked's capture lock and waits, under the hang
// cap only, for other's final save to close its stream. Then it releases the
// lock and expects both final saves in the result.
func testStreamsSideBySide(t *testing.T, blocked, other string) {
	f := newPipeDrainFixture(t, execution.StreamStdout, execution.StreamStderr)
	stall := f.stallStream(t, blocked)
	t.Cleanup(func() { _ = stall.Close() })
	for _, s := range f.streams {
		_ = s.writes.Close()
	}
	reasons := f.drain(pipeDrainLimits{silence: time.Second, total: hangCap})
	result := execution.Result{Capture: captureComplete}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		finishPipeCapture(f.readers(), reasons, &result, hangCap)
	}()
	output, err := execution.OpenOutput(f.root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = output.Close() }()
	if !streamClosesWithin(t, output, f.binding, other, hangCap) {
		t.Fatalf("%s waited for the blocked %s", other, blocked)
	}
	_ = stall.Close()
	<-finished
	if result.Capture != captureComplete || len(result.Streams) != 2 {
		t.Fatalf("result = capture %q with %d streams, want complete with both", result.Capture, len(result.Streams))
	}
}

// streamClosesWithin polls one stream until its capture is closed.
func streamClosesWithin(t *testing.T, output *execution.Output, b execution.Binding, stream string, limit time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		page, err := output.Page(t.Context(), b, stream, "", execution.MaxPage)
		if err != nil {
			t.Fatal(err)
		}
		if page.StreamClosed && page.EOF {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// Both streams are listed in reader order when every final save succeeds.
func TestFinishPipeCaptureListsStreamsInReaderOrder(t *testing.T) {
	f := newPipeDrainFixture(t, execution.StreamStdout, execution.StreamStderr)
	for _, s := range f.streams {
		_ = s.writes.Close()
	}
	reasons := f.drain(pipeDrainLimits{silence: time.Second, total: hangCap})
	result := execution.Result{Capture: captureComplete}
	finishPipeCapture(f.readers(), reasons, &result, hangCap)
	if result.Capture != captureComplete || len(result.Streams) != 2 {
		t.Fatalf("result = capture %q with %d streams", result.Capture, len(result.Streams))
	}
	if result.Streams[0].Stream != execution.StreamStdout || result.Streams[1].Stream != execution.StreamStderr {
		t.Fatalf("streams out of order: %s, %s", result.Streams[0].Stream, result.Streams[1].Stream)
	}
}
