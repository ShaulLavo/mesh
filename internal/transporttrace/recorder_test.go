package transporttrace

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testRecorder(t *testing.T) (*Recorder, string) {
	t.Helper()
	dir := t.TempDir()
	r := New(dir, Identity{PID: os.Getpid(), Version: "v0.1.9001", Commit: strings.Repeat("a", 40), Digest: strings.Repeat("b", 64)})
	if r == nil {
		t.Fatal("observer did not initialize")
	}
	t.Cleanup(r.Close)
	return r, dir
}

func readEvents(t *testing.T, r *Recorder, directory string) []event {
	t.Helper()
	r.Close()
	select {
	case <-r.done:
	case <-time.After(time.Second):
		t.Fatal("observer writer did not stop")
	}
	files, err := filepath.Glob(filepath.Join(directory, "*.jsonl"))
	if err != nil || len(files) != 1 {
		t.Fatalf("observer files: %v, %v", files, err)
	}
	data, err := os.ReadFile(files[0])
	if err != nil || len(data) > MaximumBytes {
		t.Fatalf("observer data bytes=%d: %v", len(data), err)
	}
	var rows []event
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var row event
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	return rows
}

func TestRecorderBoundsAndPayloadExclusion(t *testing.T) {
	r, dir := testRecorder(t)
	ctx := WithRecorder(t.Context(), r)
	request := httptest.NewRequest("GET", "http://private-token.invalid/private-command", nil).WithContext(ctx)
	request.RemoteAddr = "127.0.0.1:35428"
	request, finish := Request(request)
	Emit(request.Context(), HandshakeDone, errors.New("private key and terminal contents must never be rendered"))
	From(request.Context()).Session(AttachRead, "AB12")
	From(request.Context()).Session(AttachRead, "secret-contents")
	finish()
	rows := readEvents(t, r, dir)
	encoded, _ := json.Marshal(rows)
	if strings.Contains(string(encoded), "private") || strings.Contains(string(encoded), "secret") {
		t.Fatalf("observer leaked peer-controlled strings: %s", encoded)
	}
	if len(rows) != 6 || rows[2].Error != "other" || rows[3].Session != "AB12" || rows[1].Remote != "127.0.0.1:35428" {
		t.Fatalf("observer coverage = %s", encoded)
	}
	if rows[0].Identity == nil || rows[len(rows)-1].Stage != "process.end" {
		t.Fatal("observer lacks its process ownership boundary")
	}
}

func TestRecorderOverflowInvalidatesCoverageAndStaysBounded(t *testing.T) {
	r, dir := testRecorder(t)
	ctx := WithRecorder(t.Context(), r)
	for range MaximumEvents * 4 {
		Process(ctx, DaemonCancel, io.EOF)
	}
	rows := readEvents(t, r, dir)
	if len(rows) > MaximumEvents || rows[len(rows)-1].Dropped == 0 {
		t.Fatalf("overflow was not bounded and visible: events=%d dropped=%d", len(rows), r.dropped.Load())
	}
}

func TestRecorderFailureDoesNotReachCaller(t *testing.T) {
	r, _ := testRecorder(t)
	Process(WithRecorder(t.Context(), r), DaemonCancel, explodingObserverError{})
	if r.dropped.Load() != 1 {
		t.Fatal("observer panic was not isolated and marked as lost coverage")
	}
	_ = r.file.Close()
	Process(WithRecorder(t.Context(), r), DaemonCancel, io.EOF)
	r.Close()
	if New(filepath.Join(t.TempDir(), "absent"), Identity{}) != nil {
		t.Fatal("invalid observer unexpectedly enabled")
	}
	request := httptest.NewRequest("GET", "/mesh", nil)
	unchanged, finish := Request(request)
	finish()
	if unchanged != request || From(request.Context()) != nil {
		t.Fatal("disabled observation changed request/context")
	}
}

type explodingObserverError struct{}

func (explodingObserverError) Error() string { return "untrusted error" }
func (explodingObserverError) Is(error) bool { panic("observer must isolate this failure") }

func TestErrorClassificationPreservesKnownCausesWithoutText(t *testing.T) {
	if errorClass(context.Canceled) != "canceled" || errorClass(io.EOF) != "eof" || errorClass(io.ErrUnexpectedEOF) != "truncated" {
		t.Fatal("error classification lost its typed cause")
	}
}
