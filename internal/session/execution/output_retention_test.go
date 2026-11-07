package execution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func terminalOutputFixture(t *testing.T) (*Journal, Key, Intent, []string, Receipt) {
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
	capture, err := OpenCapture(j.files.root.Name(), r.Binding)
	if err != nil {
		t.Fatal(err)
	}
	capture.Offer([]byte("retained output"))
	stream, err := capture.Finish(t.Context(), "eof")
	if err != nil {
		t.Fatal(err)
	}
	code := 0
	if err := a.Finish(Result{Binding: r.Binding, Termination: Termination{Kind: "exit", Code: &code}, Capture: "complete", Streams: []StreamResult{stream}}); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	r, err = j.Lookup(t.Context(), k)
	if err != nil {
		t.Fatal(err)
	}
	return j, k, intent, env, r
}
func TestTerminalOutputQuotaPreservesReceiptAndRecovery(t *testing.T) {
	j, k, intent, env, r := terminalOutputFixture(t)
	j.maxActive = 1
	if _, err := j.files.Publish(filepath.Join("s", r.Binding.Reservation.ID, "recovery.json"), map[string]string{"saved": "recovery"}, true); err != nil {
		t.Fatal(err)
	}
	next := k
	next.Opaque = "next"
	if _, owner, err := reserveTest(t, j, next, intent, env); err != nil || !owner {
		t.Fatalf("terminal slot not released: %v %v", owner, err)
	}
	retained, err := j.Lookup(t.Context(), k)
	if err != nil || !retained.OutputExpired || retained.Result == nil || retained.RetentionReason != "recovery_pinned" {
		t.Fatalf("receipt or recovery lost: %+v %v", retained, err)
	}
	if _, err := j.files.root.Stat(filepath.Join("s", r.Binding.Reservation.ID, "recovery.json")); err != nil {
		t.Fatal(err)
	}
	output, err := OpenOutput(j.files.root.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = output.Close() }()
	page, err := output.Page(t.Context(), r.Binding, "pty", "", 1)
	if !errors.Is(err, ErrCursorExpired) || page.Available {
		t.Fatalf("expired output reopened: %+v %v", page, err)
	}
	if _, err := output.Result(t.Context(), r.Binding); err != nil {
		t.Fatalf("output expiry erased process result: %v", err)
	}
}
func TestOutputExpiryFailurePreservesBytesAndRetries(t *testing.T) {
	j, k, _, _, r := terminalOutputFixture(t)
	s := newStreamStore(j.files, r.Binding)
	m, err := s.load()
	if err != nil {
		t.Fatal(err)
	}
	j.files.fault = func(step string) error {
		if step == "publish" {
			return errors.New("expiry publication failed")
		}
		return nil
	}
	if err := j.expireOutput(t.Context(), r.Binding); err == nil {
		t.Fatal("expiry acknowledged a failed tombstone")
	}
	if _, err := j.files.root.Stat(streamPath(r.Binding, m.Segments[0].Name)); err != nil {
		t.Fatal("deleted bytes before tombstone")
	}
	j.files.fault = nil
	if err := j.expireOutput(t.Context(), r.Binding); err != nil {
		t.Fatal(err)
	}
	if _, err := j.files.root.Stat(streamPath(r.Binding, m.Segments[0].Name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("expired bytes retained without a consumer")
	}
	if retained, err := j.Lookup(t.Context(), k); err != nil || !retained.OutputExpired || retained.Result == nil {
		t.Fatalf("expiry lost receipt: %+v %v", retained, err)
	}
}
func TestOutputExpiryCannotPassOwnedLifetime(t *testing.T) {
	j, _, _, _, r := terminalOutputFixture(t)
	gate, err := j.files.LockExisting(t.Context(), attempt(r.Binding, "lifetime.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gate.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := j.expireOutput(ctx, r.Binding); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expiry crossed lifetime: %v", err)
	}
	current, err := j.read(r.Binding.RequestHash)
	if err != nil || current.OutputExpired {
		t.Fatalf("fenced output changed: %+v %v", current, err)
	}
}
