package worker

import (
	"bytes"
	"strconv"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/session/execution"
)

// A busy pipe job can exit while its last capture batch waits on a slow disk.
// The final save must get the pipe capture budget, not the shorter attachment
// flush, so the tail and EOF are kept (#318). The held stream lock stands in
// for the slow disk.
func TestPipeFinalSaveOutlastsTheAttachmentFlush(t *testing.T) {
	savedFlush, savedBudget := finalFlushTimeout, pipeCaptureFlushBudget
	finalFlushTimeout, pipeCaptureFlushBudget = 100*time.Millisecond, hangCap
	t.Cleanup(func() { finalFlushTimeout, pipeCaptureFlushBudget = savedFlush, savedBudget })

	f := newPipeDrainFixture(t, execution.StreamStdout)
	stall := f.stallStream(t, execution.StreamStdout)
	payload := bytes.Repeat([]byte("tail"), 64<<10)
	f.spawn(func() {
		_, _ = f.streams[0].writes.Write(payload)
		_ = f.streams[0].writes.Close()
	})
	reasons := f.drain(pipeDrainLimits{silence: time.Minute, total: hangCap})
	if reasons[0] != "eof" {
		t.Fatalf("drain reason = %q, want eof", reasons[0])
	}
	// Hold the final save past the attachment flush budget, then let it finish.
	time.AfterFunc(5*finalFlushTimeout, func() { _ = stall.Close() })
	result := execution.Result{Capture: captureComplete}
	finishPipeCaptures(f.readers(), reasons, &result)

	if result.Capture != captureComplete || len(result.Streams) != 1 {
		t.Fatalf("result = capture %q with %d streams, want complete with one", result.Capture, len(result.Streams))
	}
	stream := result.Streams[0]
	if !stream.EOF || stream.Loss != "" || stream.End != strconv.Itoa(len(payload)) || stream.ObservedEnd != stream.End {
		t.Fatalf("stream = %+v, want eof with all %d bytes", stream, len(payload))
	}
	output, err := execution.OpenOutput(f.root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = output.Close() }()
	got := readAll(t, output, f.binding)
	if !bytes.Equal(got, payload) {
		t.Fatalf("saved %d bytes, want the exact %d the job wrote", len(got), len(payload))
	}
}

func readAll(t *testing.T, output *execution.Output, b execution.Binding) []byte {
	t.Helper()
	var all []byte
	cursor := ""
	for {
		page, err := output.Page(t.Context(), b, execution.StreamStdout, cursor, execution.MaxPage)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, page.Bytes...)
		if page.EOF {
			return all
		}
		if len(page.Bytes) == 0 {
			t.Fatalf("stream stopped at %d bytes without EOF", len(all))
		}
		cursor = page.NextCursor
	}
}
