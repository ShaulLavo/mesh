package execution

import (
	"bytes"
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/session"
)

func pipeTestBinding(t *testing.T) (string, Binding) {
	t.Helper()
	root := t.TempDir()
	ref, err := session.NewReservation("7ABD", "boot", session.PipeExecution)
	if err != nil {
		t.Fatal(err)
	}
	b := NewBinding(Key{Host: "host", Class: "local-account", Principal: "account", Opaque: "pipe"}, Intent{Kind: KindPipe, Stdin: StdinClosed}, ref, nil)
	if err := os.MkdirAll(root+"/execution/attempts/"+ref.Instance, 0700); err != nil {
		t.Fatal(err)
	}
	return root, b
}

// saturatedCommits is how many back-to-back commits the busy writer makes.
// A reader that takes a turn between commits needs about one commit per
// read; one that only lands in the microsecond gap between commits needs
// hundreds, because it polls every 10 ms.
const saturatedCommits = 40

// A pipe writer whose queue stays full commits back to back, so the gap
// between its commits is microseconds. A reader still has to get its pages
// while that writer is busy, not after it goes idle.
func TestCaptureReaderProgressesWhileWriterIsSaturated(t *testing.T) {
	root, b := pipeTestBinding(t)
	files, err := OpenFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = files.Close() }()
	writer := &Capture{store: newStreamStore(files, b, "stdout")}
	if err := writer.initialize(); err != nil {
		t.Fatal(err)
	}
	// A loaded disk makes each commit long compared with the gap between commits.
	files.fault = func(step string) error {
		if step == "data_sync" {
			time.Sleep(5 * time.Millisecond)
		}
		return nil
	}
	reader, err := OpenOutput(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()

	var commits atomic.Int64
	written := make(chan error, 1)
	go func() { written <- saturate(writer, &commits) }()
	for hang := time.Now().Add(time.Minute); commits.Load() < 3; time.Sleep(time.Millisecond) {
		if time.Now().After(hang) {
			t.Fatal("writer never got busy")
		}
	}
	read := readDuringCommits(t, reader, b, &commits)
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if read < 5 {
		t.Fatalf("reader starved: %d of 5 pages while the writer made %d commits", read, saturatedCommits)
	}
}

// readDuringCommits reads pages until it has five or the writer has finished.
func readDuringCommits(t *testing.T, reader *Output, b Binding, commits *atomic.Int64) int {
	t.Helper()
	cursor, read := "", 0
	for read < 5 && commits.Load() < saturatedCommits {
		got, err := reader.Page(t.Context(), b, "stdout", cursor, DefaultPage)
		if errors.Is(err, ErrCursorExpired) {
			cursor = got.EarliestCursor
			continue
		}
		if err != nil {
			t.Errorf("read %d after %d commits: %v", read, commits.Load(), err)
			return read
		}
		if len(got.Bytes) == 0 || !bytes.Equal(got.Bytes, bytes.Repeat([]byte{'a'}, len(got.Bytes))) {
			t.Errorf("read %d returned %d unexpected bytes", read, len(got.Bytes))
			return read
		}
		if commits.Load() < saturatedCommits {
			read++
		}
		cursor = got.NextCursor
		// A remote reader spends a round trip between pages, long enough for
		// the polling writer to take the stream back.
		time.Sleep(20 * time.Millisecond)
	}
	return read
}

// A read that states its caller's wait waits that long for a busy stream;
// one that does not keeps the short default and reports the busy lock.
func TestCaptureReadWaitsForItsCaller(t *testing.T) {
	root, b := pipeTestBinding(t)
	c, err := OpenCapture(root, b, "stdout")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Finish(t.Context(), "eof"); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenOutput(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	saved := lockWaitLimit
	lockWaitLimit = 50 * time.Millisecond
	defer func() { lockWaitLimit = saved }()

	held, err := reader.files.LockExisting(t.Context(), streamPath(b, "stdout", "stream.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Page(t.Context(), b, "stdout", "", 1); !errors.Is(err, context.DeadlineExceeded) {
		_ = held.Close()
		t.Fatalf("read without a caller wait outlasted the default lock wait: %v", err)
	}
	released := make(chan struct{})
	time.AfterFunc(4*lockWaitLimit, func() { _ = held.Close(); close(released) })
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	page, err := reader.Page(WithCallerWait(ctx), b, "stdout", "", 1)
	<-released
	if err != nil || !page.EOF {
		t.Fatalf("read with a caller wait gave up before the lock was free: %+v %v", page, err)
	}
}

func saturate(c *Capture, commits *atomic.Int64) error {
	data := bytes.Repeat([]byte{'a'}, int(segmentCapacity(c.store.binding)))
	end := uint64(0)
	for range saturatedCommits {
		next := end + uint64(len(data))
		if _, err := c.commit(captureBatch{data: data, start: end, observed: next}); err != nil {
			return err
		}
		end = next
		commits.Add(1)
	}
	return nil
}
