package execution

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A reader whose page outlasts the writer's lock limit, alongside readers
// whose shared locks overlap, must not cost the capture a batch: the writer
// waits its turn and saves every byte, in order, through EOF.
func TestCaptureKeepsEveryByteWithStalledAndOverlappingReaders(t *testing.T) {
	saved := lockWaitLimit
	lockWaitLimit = 200 * time.Millisecond
	defer func() { lockWaitLimit = saved }()
	root, b := pipeTestBinding(t)
	c, err := OpenCapture(root, b, StreamStdout)
	if err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 2<<20)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	stop := make(chan struct{})
	var readers sync.WaitGroup
	readers.Go(func() { pageUntilStopped(t, stop, root, b, 4*lockWaitLimit) })
	for range 2 {
		readers.Go(func() { pageUntilStopped(t, stop, root, b, 0) })
	}
	for chunk := range slices.Chunk(payload, 64<<10) {
		if !c.OfferExact(t.Context(), chunk) {
			t.Fatal("capture refused a chunk")
		}
	}
	summary, err := c.Finish(t.Context(), "eof")
	close(stop)
	readers.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if !summary.EOF || summary.Loss != "" || summary.End != strconv.Itoa(len(payload)) {
		t.Fatalf("capture = %+v, want every one of %d bytes and EOF", summary, len(payload))
	}
	if got := readStream(t, root, b); !bytes.Equal(got, payload) {
		t.Fatalf("saved %d bytes, want the exact %d offered", len(got), len(payload))
	}
}

// Readers whose shared locks keep overlapping leave the stream never free. The
// writer still gets its turn after readerYieldLimit and saves every byte.
func TestCaptureWriterGetsATurnPastOverlappingReaders(t *testing.T) {
	saved := readerYieldLimit
	readerYieldLimit = 100 * time.Millisecond
	defer func() { readerYieldLimit = saved }()
	root, b := pipeTestBinding(t)
	c, err := OpenCapture(root, b, StreamStdout)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := OpenOutput(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	s, err := reader.stream(b, StreamStdout)
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var readers sync.WaitGroup
	defer func() { close(stop); readers.Wait() }()
	for index := range 3 {
		readers.Go(func() { holdSharedInTurn(stop, s, time.Duration(index)*50*time.Millisecond) })
	}
	payload := bytes.Repeat([]byte("overlap"), 64<<10)
	if !c.OfferExact(t.Context(), payload) {
		t.Fatal("capture refused the payload")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	summary, err := c.Finish(ctx, "eof")
	if err != nil || !summary.EOF || summary.Loss != "" || summary.End != strconv.Itoa(len(payload)) {
		t.Fatalf("capture behind overlapping readers = %+v %v", summary, err)
	}
}

// holdSharedInTurn takes the stream's shared lock as a reader would and holds
// it for 150 ms at a time, starting after offset, until stopped.
func holdSharedInTurn(stop <-chan struct{}, s *streamStore, offset time.Duration) {
	time.Sleep(offset)
	for {
		select {
		case <-stop:
			return
		default:
		}
		fence, err := s.readerFence(context.Background(), unix.LOCK_SH)
		if err != nil {
			continue
		}
		time.Sleep(150 * time.Millisecond)
		_ = fence.Close()
	}
}

// pageUntilStopped tails the stream on its own handle. A positive stall holds
// each new manifest's confirmation, and so the shared stream lock, that long.
// Its reads may fail on their own lock limit; only the writer's outcome counts.
func pageUntilStopped(t *testing.T, stop <-chan struct{}, root string, b Binding, stall time.Duration) {
	reader, err := OpenOutput(root)
	if err != nil {
		t.Error(err)
		return
	}
	defer func() { _ = reader.Close() }()
	if stall > 0 {
		reader.files.fault = func(step string) error {
			if step == "data_sync" {
				time.Sleep(stall)
			}
			return nil
		}
	}
	cursor := ""
	for {
		select {
		case <-stop:
			return
		default:
		}
		page, err := reader.Page(context.Background(), b, StreamStdout, cursor, MaxPage)
		switch {
		case errors.Is(err, ErrCursorExpired):
			cursor = page.EarliestCursor
		case err == nil:
			cursor = page.NextCursor
		}
	}
}

func readStream(t *testing.T, root string, b Binding) []byte {
	t.Helper()
	reader, err := OpenOutput(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	var all []byte
	cursor := ""
	for {
		page, err := reader.Page(t.Context(), b, StreamStdout, cursor, MaxPage)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, page.Bytes...)
		if page.EOF || len(page.Bytes) == 0 {
			return all
		}
		cursor = page.NextCursor
	}
}

// reader.lock is coordination state. A stream without one, like a stream
// retained from before the reader hand-off, still saves, pages and confirms
// its final result.
func TestCaptureStreamWithoutReaderLockStaysUsable(t *testing.T) {
	root, b := pipeTestBinding(t)
	c, err := OpenCapture(root, b, StreamStdout)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, streamPath(b, StreamStdout, readerLockName))); err != nil {
		t.Fatal(err)
	}
	c.Offer([]byte("retained output"))
	summary, err := c.Finish(t.Context(), "eof")
	if err != nil || !summary.EOF || summary.Loss != "" {
		t.Fatalf("finish without reader.lock: %+v %v", summary, err)
	}
	if got := readStream(t, root, b); string(got) != "retained output" {
		t.Fatalf("page without reader.lock = %q", got)
	}
	reader, err := OpenOutput(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	if err := reader.confirmFinal(t.Context(), b, summary); err != nil {
		t.Fatalf("final confirmation without reader.lock: %v", err)
	}
}
