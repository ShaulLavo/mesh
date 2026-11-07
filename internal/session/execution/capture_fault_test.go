package execution

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestCaptureQueueLossAdvancesOffsetsWithoutStitching(t *testing.T) {
	root, b := captureTestBinding(t)
	c, err := OpenCapture(root, b)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	held := false
	c.store.files.fault = func(step string) error {
		if step != "data_sync" || held {
			return nil
		}
		held = true
		close(entered)
		<-release
		return nil
	}
	c.Offer(bytes.Repeat([]byte{'a'}, captureSegment))
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("writer did not block")
	}
	c.Offer(make([]byte, captureQueue+1))
	close(release)
	final, err := c.Finish(t.Context(), "eof")
	if err != nil {
		t.Fatal(err)
	}
	if final.Loss != "queue_overflow" || final.End != offsetText(captureSegment+captureQueue+1) || final.Earliest != final.End {
		t.Fatalf("gap not published: %+v", final)
	}
	reader, err := OpenOutput(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	page, err := reader.Page(t.Context(), b, "pty", "", DefaultPage)
	if err != nil || len(page.Bytes) != 0 || page.Loss != "queue_overflow" {
		t.Fatalf("stitched gap: %+v %v", page, err)
	}
}
func TestCaptureCursorCasesAndDamagedStorage(t *testing.T) {
	root, b := captureTestBinding(t)
	c, err := OpenCapture(root, b)
	if err != nil {
		t.Fatal(err)
	}
	c.Offer([]byte("hello"))
	summary, err := c.Finish(t.Context(), "eof")
	if err != nil {
		t.Fatal(err)
	}
	reader, err := OpenOutput(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	for _, test := range []struct {
		value string
		want  error
	}{{"bad", ErrCursorFormat}, {makeCursor(b, summary.Generation, 6), ErrCursorInvalid}, {makeCursor(Binding{Host: "wrong"}, summary.Generation, 0), ErrCursorExpired}} {
		if _, err := reader.Page(t.Context(), b, "pty", test.value, 1); !errors.Is(err, test.want) {
			t.Fatalf("cursor: %v want %v", err, test.want)
		}
	}
	page, err := reader.Page(t.Context(), b, "pty", makeCursor(b, summary.Generation, 5), 1)
	if err != nil || !page.EOF || len(page.Bytes) != 0 {
		t.Fatalf("empty end: %+v %v", page, err)
	}
	store, _ := reader.stream(b)
	m, err := store.load()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(filepath.Join(root, streamPath(b, m.Segments[0].Name)), 1); err != nil {
		t.Fatal(err)
	}
	if page, err := reader.Page(t.Context(), b, "pty", "", DefaultPage); err == nil || len(page.Bytes) != 0 {
		t.Fatal("shortened damaged storage succeeded")
	}
}
func TestCaptureOpenEmptyPageIsNotEOF(t *testing.T) {
	root, b := captureTestBinding(t)
	c, err := OpenCapture(root, b)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = c.Finish(context.Background(), "interrupted") }()
	reader, err := OpenOutput(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	page, err := reader.Page(t.Context(), b, "pty", "", 1)
	if err != nil || page.EOF || page.StreamClosed || len(page.Bytes) != 0 {
		t.Fatalf("empty active: %+v %v", page, err)
	}
}
func TestCaptureNewReaderProcessConfirmsDependencies(t *testing.T) {
	root, b := captureTestBinding(t)
	c, err := OpenCapture(root, b)
	if err != nil {
		t.Fatal(err)
	}
	c.Offer([]byte("confirmed"))
	if _, err := c.Finish(t.Context(), "eof"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.store.files.root.Stat("."); err == nil {
		t.Fatal("writer descriptor leaked")
	}
	f, err := OpenFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Publish("binding.json", b, true); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := &exec.Cmd{Path: executable, Args: []string{executable, "-test.run=^TestCaptureReaderProcess$"}, Env: append(os.Environ(), "MESH_CAPTURE_TEST_ROOT="+root)}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("new process: %v %s", err, output)
	}
}
func TestCaptureReaderProcess(t *testing.T) {
	root := os.Getenv("MESH_CAPTURE_TEST_ROOT")
	if root == "" {
		return
	}
	f, err := OpenFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var b Binding
	if err := f.Read("binding.json", &b); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenOutput(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	reader.files.fault = func(step string) error {
		if step == "device_sync" {
			return errors.New("unconfirmed namespace")
		}
		return nil
	}
	if page, err := reader.Page(t.Context(), b, "pty", "", 1); err == nil || len(page.Bytes) != 0 {
		t.Fatal("new reader inherited confirmation")
	}
	reader.files.fault = nil
	page, err := reader.Page(t.Context(), b, "pty", "", 32)
	if err != nil || string(page.Bytes) != "confirmed" {
		t.Fatalf("settlement: %+v %v", page, err)
	}
}

func TestCaptureUnconfirmedManifestCannotAuthorizePruning(t *testing.T) {
	root, b := captureTestBinding(t)
	c, err := OpenCapture(root, b)
	if err != nil {
		t.Fatal(err)
	}
	c.Offer([]byte("predecessor"))
	if _, err := c.Finish(t.Context(), "eof"); err != nil {
		t.Fatal(err)
	}
	f, err := OpenFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	s := newStreamStore(f, b)
	old, err := s.load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Publish("binding.json", b, true); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := &exec.Cmd{Path: executable, Args: []string{executable, "-test.run=^TestCapturePublisherProcess$"}, Env: append(os.Environ(), "MESH_CAPTURE_TEST_ROOT="+root)}
	output, err := command.CombinedOutput()
	var exited *exec.ExitError
	if !errors.As(err, &exited) || exited.ExitCode() != 77 {
		t.Fatalf("publisher did not die at boundary: %v %s", err, output)
	}
	current, err := s.load()
	if err != nil {
		t.Fatal(err)
	}
	f.fault = func(step string) error {
		if step == "device_sync" {
			return errors.New("settlement failed")
		}
		return nil
	}
	gate, err := f.LockExisting(t.Context(), streamPath(b, "stream.lock"))
	if err != nil {
		t.Fatal(err)
	}
	err = s.prune(t.Context(), current)
	_ = gate.Close()
	if err == nil {
		t.Fatal("pruned an unconfirmed manifest")
	}
	if _, err := f.root.Stat(streamPath(b, old.Segments[0].Name)); err != nil {
		t.Fatal("predecessor was deleted before acknowledgement")
	}
	f.fault = nil
	gate, err = f.LockExisting(t.Context(), streamPath(b, "stream.lock"))
	if err != nil {
		t.Fatal(err)
	}
	err = s.prune(t.Context(), current)
	_ = gate.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.root.Stat(streamPath(b, old.Segments[0].Name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("confirmed orphan was not pruned")
	}
}
func TestCapturePublisherProcess(t *testing.T) {
	root := os.Getenv("MESH_CAPTURE_TEST_ROOT")
	if root == "" {
		return
	}
	f, err := OpenFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	var b Binding
	if err := f.Read("binding.json", &b); err != nil {
		t.Fatal(err)
	}
	s := newStreamStore(f, b)
	gate, err := f.LockExisting(t.Context(), streamPath(b, "stream.lock"))
	if err != nil {
		t.Fatal(err)
	}

	m, err := s.load()
	if err != nil {
		t.Fatal(err)
	}
	c := &Capture{store: s}
	seg, err := c.writeSegment(0, []byte("replacement"))
	if err != nil {
		t.Fatal(err)
	}
	m.Segments = []segment{seg}
	m.Earliest = 0
	m.End = uint64(seg.Size)
	m.Observed = m.End
	f.fault = func(step string) error {
		if step == "directory_sync" {
			return errors.New("manifest namespace unconfirmed")
		}
		return nil
	}
	stage, err := f.Publish(streamPath(b, "manifest.json"), m, false)
	if err == nil || stage != Unconfirmed {
		t.Fatal("missing visible-unconfirmed boundary")
	}
	runtime.KeepAlive(gate)
	os.Exit(77)
}
func TestCaptureConfirmedPollingDoesNotResyncSuffix(t *testing.T) {
	root, b := captureTestBinding(t)
	c, err := OpenCapture(root, b)
	if err != nil {
		t.Fatal(err)
	}
	c.Offer([]byte("bounded"))
	if _, err := c.Finish(t.Context(), "eof"); err != nil {
		t.Fatal(err)
	}
	o, err := OpenOutput(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = o.Close() }()
	if _, err := o.Page(t.Context(), b, "pty", "", 1); err != nil {
		t.Fatal(err)
	}
	o.files.fault = func(step string) error {
		if step == "data_sync" || step == "device_sync" {
			return errors.New("poll resynced an established identity")
		}
		return nil
	}
	for range 3 {
		if _, err := o.Page(t.Context(), b, "pty", "", 1); err != nil {
			t.Fatal(err)
		}
	}
}
func TestCaptureDecimalOffsetsBeyondJavaScriptInteger(t *testing.T) {
	_, b := captureTestBinding(t)
	generation, _ := freshToken()
	offset := uint64(1<<53) + 17
	token := makeCursor(b, generation, offset)
	decoded, n, err := readCursor(token)
	if err != nil || n != offset || decoded.Offset != "9007199254741009" {
		t.Fatalf("offset rounded: %+v %d %v", decoded, n, err)
	}
}
