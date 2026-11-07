package execution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func pendingCaptureFixture(t *testing.T) (*Journal, Key, Receipt) {
	t.Helper()
	j, k, intent, env := journalFixture(t)
	r, _, err := reserveTest(t, j, k, intent, env)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Dispatch(t.Context(), r.Binding, intent); err != nil {
		t.Fatal(err)
	}
	a, err := StartAttempt(t.Context(), j.files.root.Name(), r.Binding, intent, env)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	c, err := OpenCapture(j.files.root.Name(), r.Binding)
	if err != nil {
		t.Fatal(err)
	}
	c.Offer([]byte("exact-output"))
	summary, err := c.Finish(t.Context(), "eof")
	if err != nil {
		t.Fatal(err)
	}
	code := 0
	result := Result{Binding: r.Binding, Termination: Termination{Kind: "exit", Code: &code}, Capture: "complete", Streams: []StreamResult{summary}}
	if err := a.Finish(result); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	return j, k, r
}

func TestExecutionRegressionReceiptMustSettleFinalCapture(t *testing.T) {
	j, k, r := pendingCaptureFixture(t)
	output, err := OpenOutput(j.files.root.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = output.Close() })
	store := newStreamStore(j.files, r.Binding)
	m, err := store.load()
	if err != nil {
		t.Fatal(err)
	}
	if err := j.files.root.Remove(streamPath(r.Binding, m.Segments[0].Name)); err != nil {
		t.Fatal(err)
	}
	if _, err := output.Result(t.Context(), r.Binding); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("control did not detect missing final dependency: %v", err)
	}
	next, err := j.Lookup(t.Context(), k)
	if err == nil || next.Phase == phaseTerminal {
		t.Fatalf("request certified capture with missing dependency: phase=%s result=%+v err=%v", next.Phase, next.Result, err)
	}
}

func TestExecutionRegressionDamagedFinalDependencyIsNotPending(t *testing.T) {
	j, _, r := pendingCaptureFixture(t)
	output, err := OpenOutput(j.files.root.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = output.Close() })
	store := newStreamStore(j.files, r.Binding)
	m, err := store.load()
	if err != nil {
		t.Fatal(err)
	}
	if err := j.files.root.Remove(streamPath(r.Binding, m.Segments[0].Name)); err != nil {
		t.Fatal(err)
	}
	_, err = output.Result(context.Background(), r.Binding)
	if IsResultPending(err) {
		t.Fatalf("existing result with missing final dependency misclassified as pending: %v", err)
	}
}

func TestExecutionRegressionExactRemovalRetryConverges(t *testing.T) {
	j, _, _, _, r := terminalOutputFixture(t)
	sessions := filepath.Join(j.files.root.Name(), "s")
	if err := j.RetireExact(t.Context(), sessions, r.Binding, func() error { return nil }); err != nil {
		t.Fatal("first removal failed", err)
	}
	if err := j.RetireExact(t.Context(), sessions, r.Binding, func() error { return nil }); err != nil {
		t.Fatalf("retry of completed exact removal did not converge: %v", err)
	}
}

func TestExecutionRegressionMissingDependencyCannotAuthorizeRetirement(t *testing.T) {
	j, _, r := pendingCaptureFixture(t)
	store := newStreamStore(j.files, r.Binding)
	m, err := store.load()
	if err != nil {
		t.Fatal(err)
	}
	if err := j.files.root.Remove(streamPath(r.Binding, m.Segments[0].Name)); err != nil {
		t.Fatal(err)
	}
	err = j.RetireExact(t.Context(), filepath.Join(j.files.root.Name(), "s"), r.Binding, func() error { return nil })
	current, readErr := j.read(r.Binding.RequestHash)
	_, statErr := j.files.root.Stat(filepath.Join("s", r.Binding.Reservation.ID))
	if err == nil || readErr != nil || current.Removal != removalPresent || statErr != nil {
		t.Fatalf("missing dependency crossed retirement guard: error=%v read=%v removal=%s sessionStat=%v", err, readErr, current.Removal, statErr)
	}
}
func TestExecutionRegressionNewReceiptReaderCannotSkipStreamFence(t *testing.T) {
	j, k, r := pendingCaptureFixture(t)
	held, err := j.files.LockExisting(t.Context(), streamPath(r.Binding, "stream.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	next, err := j.Lookup(ctx, k)
	if err == nil && next.Phase == phaseTerminal {
		t.Fatal("new receipt observation certified final capture while its exclusive settlement fence was unavailable")
	}
}
func TestExecutionRegressionIndependentPageDeadline(t *testing.T) {
	root, b := captureTestBinding(t)
	c, err := OpenCapture(root, b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Finish(t.Context(), "eof"); err != nil {
		t.Fatal(err)
	}
	o, err := OpenOutput(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = o.Close() }()
	s, err := o.stream(b)
	if err != nil {
		t.Fatal(err)
	}
	held, err := o.files.LockExisting(t.Context(), streamPath(b, "stream.lock"))
	if err != nil {
		t.Fatal(err)
	}
	first := make(chan error, 1)
	go func() { _, err := o.Page(t.Context(), b, "pty", "", 1); first <- err }()
	// The first real Page owns admission while waiting on the cross-process lock.
	deadline := time.Now().Add(time.Second)
	for len(s.gate) == 0 {
		if time.Now().After(deadline) {
			_ = held.Close()
			<-first
			t.Fatal("first reader did not arm")
		}
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	second := make(chan error, 1)
	go func() { _, err := o.Page(ctx, b, "pty", "", 1); second <- err }()
	select {
	case err := <-second:
		_ = held.Close()
		<-first
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	case <-time.After(200 * time.Millisecond):
		_ = held.Close()
		<-first
		err := <-second
		t.Fatalf("independent reader remained behind another reader past its own deadline: %v", err)
	}
}

func TestExecutionRegressionPendingRemovalRetriesCatalog(t *testing.T) {
	j, _, _, _, r := terminalOutputFixture(t)
	sessions := filepath.Join(j.files.root.Name(), "s")
	fault := errors.New("catalog retirement interrupted")
	if err := j.RetireExact(t.Context(), sessions, r.Binding, func() error { return fault }); !errors.Is(err, fault) {
		t.Fatal(err)
	}
	calls := 0
	if err := j.RetireExact(t.Context(), sessions, r.Binding, func() error { calls++; return nil }); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("pending removal skipped catalog retirement")
	}
}

func TestExecutionRegressionMissingAttemptIsNotPending(t *testing.T) {
	j, _, r := pendingCaptureFixture(t)
	if err := j.files.root.RemoveAll(filepath.Dir(attempt(r.Binding, "result.json"))); err != nil {
		t.Fatal(err)
	}
	output := &Output{files: j.files, streams: make(map[Binding]*streamStore)}
	_, err := output.Result(t.Context(), r.Binding)
	if err == nil || IsResultPending(err) {
		t.Fatal("missing attempt treated as pending result", err)
	}
}
