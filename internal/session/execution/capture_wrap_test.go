package execution

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestCaptureSixteenMiBSuffixAndConcurrentReaders(t *testing.T) {
	root, b := captureTestBinding(t)
	files, err := OpenFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = files.Close() }()
	capture := &Capture{store: newStreamStore(files, b)}
	if err := capture.initialize(); err != nil {
		t.Fatal(err)
	}
	readers := make([]*Output, 2)
	for index := range readers {
		readers[index], err = OpenOutput(root)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = readers[index].Close() })
	}
	initial, err := readers[0].Page(t.Context(), b, "pty", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte{'q'}, captureSegment)
	for index := uint64(0); index < 255; index++ {
		_, err := capture.commit(captureBatch{data: data, start: index * captureSegment, observed: (index + 1) * captureSegment})
		if err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	var group sync.WaitGroup
	failures := make(chan error, 2)
	for _, reader := range readers {
		group.Go(func() { failures <- readWhileWrapping(ctx, reader, b) })
	}
	defer func() { cancel(); group.Wait(); close(failures) }()
	for index := uint64(255); index < 258; index++ {
		_, err := capture.commit(captureBatch{data: data, start: index * captureSegment, observed: (index + 1) * captureSegment})
		if err != nil {
			t.Fatal(err)
		}
	}
	end := uint64(258 * captureSegment)
	final, err := capture.commit(captureBatch{start: end, observed: end, final: true, reason: "eof"})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	group.Wait()
	for range 2 {
		if err := <-failures; err != nil {
			t.Fatal(err)
		}
	}
	if final.Earliest != offsetText(2*captureSegment) || final.End != offsetText(end) || final.Loss != "retention" {
		t.Fatalf("wrong retained suffix: %+v", final)
	}
	expired, err := readers[0].Page(t.Context(), b, "pty", initial.EarliestCursor, 1)
	if !errors.Is(err, ErrCursorExpired) || !expired.Gap || len(expired.Bytes) != 0 {
		t.Fatalf("stale reader silently jumped: %+v %v", expired, err)
	}
	page, err := readers[1].Page(t.Context(), b, "pty", "", MaxPage)
	if err != nil || len(page.Bytes) != MaxPage || !bytes.Equal(page.Bytes, bytes.Repeat([]byte{'q'}, MaxPage)) {
		t.Fatalf("max page: bytes=%d error=%v", len(page.Bytes), err)
	}
	entries, err := files.Names(streamPath(b, "."))
	if err != nil {
		t.Fatal(err)
	}
	diskBytes := int64(0)
	for _, entry := range entries {
		if !validToken(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		diskBytes += info.Size()
	}
	if diskBytes != captureSuffix {
		t.Fatalf("unbounded or missing segments: %d", diskBytes)
	}
}
func readWhileWrapping(ctx context.Context, reader *Output, b Binding) error {
	cursor := ""
	for ctx.Err() == nil {
		page, err := reader.Page(ctx, b, "pty", cursor, DefaultPage)
		if errors.Is(err, context.Canceled) {
			return nil
		}
		if errors.Is(err, ErrCursorExpired) {
			cursor = page.EarliestCursor
			continue
		}
		if err != nil {
			return err
		}
		if len(page.Bytes) > DefaultPage {
			return fmt.Errorf("reader exceeded its independent page bound")
		}
		cursor = page.NextCursor
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(5 * time.Millisecond):
		}
	}
	return nil
}
