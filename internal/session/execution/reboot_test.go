package execution

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// These fixtures simulate what a power cut could leave behind: a job written
// on one boot, files then damaged directly, and the journal reopened on a new
// boot. They prove the recovery logic, not what a real disk keeps.

// finishedOnEarlierBoot runs a pipe job to a terminal receipt with output on
// both streams, and returns the cursor a reader held at stdout's end.
func finishedOnEarlierBoot(t *testing.T) (*Journal, Key, Binding, string) {
	t.Helper()
	j, k, i, env := pipeJournalFixture(t)
	b, a := claimedAttempt(t, j, k, i, env)
	code := 3
	result := Result{Binding: b, Termination: Termination{Kind: "exit", Code: &code}, Capture: "complete"}
	for _, name := range b.Streams() {
		c, err := OpenCapture(j.files.root.Name(), b, name)
		if err != nil {
			t.Fatal(err)
		}
		c.Offer([]byte("bytes of " + name))
		stream, err := c.Finish(t.Context(), captureEOF)
		if err != nil {
			t.Fatal(err)
		}
		result.Streams = append(result.Streams, stream)
	}
	if err := a.Finish(result); err != nil {
		t.Fatal(err)
	}
	// The worker is gone: release the lifetime fence reclamation waits for.
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if r, err := j.Published(t.Context(), b); err != nil || r.Phase != phaseTerminal {
		t.Fatalf("published: %+v %v", r, err)
	}
	page, err := j.output().Page(t.Context(), b, StreamStdout, "", 1024)
	if err != nil || string(page.Bytes) != "bytes of stdout" {
		t.Fatalf("stdout before the reboot: %+v %v", page, err)
	}
	return j, k, b, page.NextCursor
}

func streamFile(t *testing.T, j *Journal, b Binding, stream, name string) string {
	t.Helper()
	return filepath.Join(j.files.root.Name(), streamPath(b, stream, name))
}

func onlySegment(t *testing.T, j *Journal, b Binding, stream string) string {
	t.Helper()
	m, err := newStreamStore(j.files, b, stream).load()
	if err != nil || len(m.Segments) != 1 {
		t.Fatalf("%s manifest: %+v %v", stream, m, err)
	}
	return streamFile(t, j, b, stream, m.Segments[0].Name)
}

func truncate(t *testing.T, path string) {
	t.Helper()
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
}

func remove(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

// A lost segment shows the same loss to the receipt, pages, tail and
// reclamation, and the job keeps the termination its receipt recorded.
func TestEarlierBootLostSegmentReadsAsHostRebootEverywhere(t *testing.T) {
	j, k, b, heldCursor := finishedOnEarlierBoot(t)
	remove(t, onlySegment(t, j, b, StreamStdout))
	truncate(t, filepath.Join(j.files.root.Name(), attempt(b, "result.json")))
	j = reopenTestJournal(t, j)
	output, err := OpenOutput(j.files.root.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = output.Close() }()
	output.WithBoot("new-boot")

	r, err := j.Lookup(t.Context(), k)
	if err != nil || r.Phase != phaseTerminal || *r.Result.Termination.Code != 3 {
		t.Fatalf("receipt after the reboot: %+v %v", r, err)
	}
	stdout, stderr := r.Result.Streams[0], r.Result.Streams[1]
	if r.Result.Capture != "incomplete" || stdout.Loss != captureHostReboot || stdout.End != stdout.Earliest || stdout.EOF {
		t.Fatalf("stdout summary: capture=%s %+v", r.Result.Capture, stdout)
	}
	if stderr.Loss != "" || !stderr.EOF {
		t.Fatalf("intact stderr changed: %+v", stderr)
	}
	if err := r.Result.validate(b); err != nil {
		t.Fatalf("salvaged result is invalid: %v", err)
	}

	page, err := output.Page(t.Context(), b, StreamStdout, "", 1024)
	if err != nil || len(page.Bytes) != 0 || page.Loss != captureHostReboot || !page.StreamClosed || page.EOF {
		t.Fatalf("fresh page: %+v %v", page, err)
	}
	rolled, err := output.Page(t.Context(), b, StreamStdout, heldCursor, 1024)
	if err != nil || !rolled.Gap || rolled.Reason != captureHostReboot {
		t.Fatalf("cursor past the rolled-back end: %+v %v", rolled, err)
	}
	stderrPage, err := output.Page(t.Context(), b, StreamStderr, "", 1024)
	if err != nil || string(stderrPage.Bytes) != "bytes of stderr" {
		t.Fatalf("stderr page: %+v %v", stderrPage, err)
	}
	if foreign, err := output.Page(t.Context(), b, StreamStdout, stderrPage.NextCursor, 1024); !errors.Is(err, ErrCursorExpired) || foreign.Reason != "binding_mismatch" {
		t.Fatalf("foreign cursor: %+v %v", foreign, err)
	}

	tail, err := output.Tail(t.Context(), b, StreamStdout, 64)
	if err != nil || len(tail.Bytes) != 0 || tail.Loss != captureHostReboot {
		t.Fatalf("tail: %+v %v", tail, err)
	}

	if err := j.expireOutput(t.Context(), b); err != nil {
		t.Fatalf("reclaim salvaged output: %v", err)
	}
	expired, err := output.Page(t.Context(), b, StreamStdout, "", 1024)
	if !errors.Is(err, ErrCursorExpired) || expired.Available || expired.Reason != "retained_range" {
		t.Fatalf("page after reclamation: %+v %v", expired, err)
	}
	if after, err := j.Lookup(t.Context(), k); err != nil || !after.OutputExpired || *after.Result.Termination.Code != 3 {
		t.Fatalf("receipt after reclamation: %+v %v", after, err)
	}
}

// A torn manifest is rebuilt as an empty stream with a stable generation.
func TestEarlierBootTornManifestRebuildsTheStream(t *testing.T) {
	j, _, b, heldCursor := finishedOnEarlierBoot(t)
	truncate(t, streamFile(t, j, b, StreamStdout, "manifest.json"))
	j = reopenTestJournal(t, j)
	output := j.output()
	first, err := output.Page(t.Context(), b, StreamStdout, "", 1024)
	if err != nil || first.Generation != rebootGeneration(b, StreamStdout) || first.Loss != captureHostReboot || len(first.Bytes) != 0 {
		t.Fatalf("rebuilt stream: %+v %v", first, err)
	}
	again, err := j.output().Page(t.Context(), b, StreamStdout, first.NextCursor, 1024)
	if err != nil || again.Gap || again.Generation != first.Generation {
		t.Fatalf("rebuilt generation is not stable: %+v %v", again, err)
	}
	old, err := output.Page(t.Context(), b, StreamStdout, heldCursor, 1024)
	if err != nil || !old.Gap || old.Reason != captureHostReboot {
		t.Fatalf("cursor of the replaced generation: %+v %v", old, err)
	}
}

// A started pipe job whose result tore and whose stream lost its lock reads
// as interrupted at the reboot instead of failing.
func TestEarlierBootTornResultAndLostLockInterruptTheJob(t *testing.T) {
	j, key, _, env := journalFixture(t)
	intent, err := Normalize(Intent{Kind: KindPipe, Command: []string{"/bin/true"}, Cwd: j.files.root.Name()})
	if err != nil {
		t.Fatal(err)
	}
	b := interruptedAttempt(t, j, key, intent, env)
	if err := os.WriteFile(filepath.Join(j.files.root.Name(), attempt(b, "result.json")), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	remove(t, streamFile(t, j, b, StreamStderr, "stream.lock"))
	j = reopenTestJournal(t, j)
	r, err := j.Lookup(t.Context(), key)
	if err != nil || r.Phase != phaseTerminal || r.Result.Termination.Kind != interrupted || r.Result.Termination.Reason != "host_reboot" {
		t.Fatalf("torn result after the reboot: %+v %v", r, err)
	}
	if err := r.Result.validate(b); err != nil {
		t.Fatalf("interrupted result is invalid: %v", err)
	}
}

// A PTY job never writes started.json; its recorded publication is the start
// evidence, so a reboot interrupts it instead of leaving it uncertain.
func TestEarlierBootPublishedPTYIsInterrupted(t *testing.T) {
	j, k, i, env := journalFixture(t)
	b, _ := claimedAttempt(t, j, k, i, env)
	s := newStreamStore(j.files, b, KindPTY)
	if err := initializeStreams([]*streamStore{s}); err != nil {
		t.Fatal(err)
	}
	c := Capture{store: s}
	if _, err := c.commit(captureBatch{data: []byte("screen"), observed: 6}); err != nil {
		t.Fatal(err)
	}
	if r, err := j.Published(t.Context(), b); err != nil || r.LaunchOutcome != outcomeStarted {
		t.Fatalf("published PTY: %+v %v", r, err)
	}
	j = reopenTestJournal(t, j)
	r, err := j.Lookup(t.Context(), k)
	if err != nil || r.Phase != phaseTerminal || r.Result.Termination.Kind != interrupted {
		t.Fatalf("PTY after the reboot: %+v %v", r, err)
	}
}

// The same damage in an attempt of the current boot is still an error: only
// a reboot can explain a lost record.
func TestCurrentBootDamageStillFails(t *testing.T) {
	j, _, b, _ := finishedOnEarlierBoot(t)
	remove(t, onlySegment(t, j, b, StreamStdout))
	output, err := OpenOutput(j.files.root.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = output.Close() }()
	output.WithBoot("boot")
	if _, err := output.Page(t.Context(), b, StreamStdout, "", 1024); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("current-boot page of a lost segment: %v", err)
	}
	if _, err := output.Tail(t.Context(), b, StreamStdout, 64); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("current-boot tail of a lost segment: %v", err)
	}
	truncate(t, filepath.Join(j.files.root.Name(), attempt(b, "result.json")))
	if _, err := output.Result(t.Context(), b); err == nil || IsResultPending(err) {
		t.Fatalf("current-boot torn result: %v", err)
	}
}

// Ordinary reclamation of an intact earlier-boot job still expires it.
func TestEarlierBootIntactOutputExpiresNormally(t *testing.T) {
	j, k, b, heldCursor := finishedOnEarlierBoot(t)
	j = reopenTestJournal(t, j)
	r, err := j.Lookup(t.Context(), k)
	if err != nil || r.Result.Capture != "complete" || r.Result.Streams[0].Loss != "" {
		t.Fatalf("intact receipt after the reboot: %+v %v", r, err)
	}
	if err := j.expireOutput(t.Context(), b); err != nil {
		t.Fatal(err)
	}
	page, err := j.output().Page(t.Context(), b, StreamStdout, heldCursor, 1024)
	if !errors.Is(err, ErrCursorExpired) || page.Reason != "retained_range" {
		t.Fatalf("expired page: %+v %v", page, err)
	}
}
