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
	for _, blocked := range []string{execution.StreamStdout, execution.StreamStderr} {
		other := execution.StreamStderr
		if blocked == execution.StreamStderr {
			other = execution.StreamStdout
		}
		t.Run(blocked, func(t *testing.T) {
			f := newPipeDrainFixture(t, execution.StreamStdout, execution.StreamStderr)
			stall := f.stallStream(t, blocked)
			t.Cleanup(func() { _ = stall.Close() })
			for _, s := range f.streams {
				_ = s.writes.Close()
			}
			reasons := f.drain(pipeDrainLimits{silence: time.Second, total: hangCap})
			result := execution.Result{Capture: captureComplete}
			finishPipeCapture(f.readers(), reasons, &result, 300*time.Millisecond)
			if result.Capture != execution.CaptureNotRecorded || len(result.Streams) != 0 {
				t.Fatalf("result = capture %q with %d streams, want %q with none", result.Capture, len(result.Streams), execution.CaptureNotRecorded)
			}
			output, err := execution.OpenOutput(f.root)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = output.Close() }()
			page, err := output.Page(t.Context(), f.binding, other, "", execution.MaxPage)
			if err != nil {
				t.Fatal(err)
			}
			if !page.StreamClosed || !page.EOF {
				t.Fatalf("%s waited for the blocked %s: closed=%v eof=%v", other, blocked, page.StreamClosed, page.EOF)
			}
		})
	}
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
