package execution

import (
	"bytes"
	"errors"
	"testing"
)

func TestPipeBootChangeKeepsOnlyProvableState(t *testing.T) {
	j, key, _, env := journalFixture(t)
	intent, err := Normalize(Intent{Kind: KindPipe, Command: []string{"/bin/true"}, Cwd: j.files.root.Name()})
	if err != nil {
		t.Fatal(err)
	}
	started := interruptedAttempt(t, j, key, intent, env)
	uncertainKey := key
	uncertainKey.Opaque = "dispatch-without-start"
	uncertain := interruptedAttempt(t, j, uncertainKey, intent, env)
	j = reopenTestJournal(t, j)

	done, err := j.Lookup(t.Context(), key)
	if err != nil || done.Phase != phaseTerminal || done.Result.Termination.Kind != "interrupted" || done.Result.Termination.Reason != "host_reboot" {
		t.Fatalf("started pipe was not interrupted: %+v %v", done, err)
	}
	if done.Result.Capture != "incomplete" || done.OutputRetainUntil == nil {
		t.Fatalf("interrupted output lost retention: %+v", done)
	}
	o := &Output{files: j.files, streams: make(map[streamKey]*streamStore)}
	page, err := o.Page(t.Context(), started, StreamStdout, "", 20)
	if err != nil || !bytes.Equal(page.Bytes, []byte{0, 255}) || page.EOF || !page.StreamClosed || page.CloseReason != "interrupted" {
		t.Fatalf("recovered interrupted output: %+v %v", page, err)
	}
	maybe, err := j.Lookup(t.Context(), uncertainKey)
	if !errors.Is(err, ErrUnknown) || maybe.Phase != phaseUncertain || maybe.Binding != uncertain {
		t.Fatalf("unproven dispatch was treated as settled: %+v %v", maybe, err)
	}
	_, again, err := j.Reserve(t.Context(), uncertainKey, intent, func() (Binding, error) {
		t.Fatal("uncertain dispatch was allocated again")
		return Binding{}, ErrUnknown
	})
	if again || !errors.Is(err, ErrUnknown) {
		t.Fatalf("uncertain key admitted again: %v %v", again, err)
	}
}

func interruptedAttempt(t *testing.T, j *Journal, key Key, intent Intent, env []string) Binding {
	t.Helper()
	r, first, err := reserveTest(t, j, key, intent, env)
	if err != nil || !first {
		t.Fatalf("pipe reservation: %+v %v", r, err)
	}
	b := r.Binding
	if err := j.Dispatch(t.Context(), b, intent); err != nil {
		t.Fatal(err)
	}
	if key.Opaque != "dispatch-without-start" {
		writeUnfinishedPipe(t, j, b)
	}
	return b
}

func writeUnfinishedPipe(t *testing.T, j *Journal, b Binding) {
	t.Helper()
	a := Attempt{files: j.files, binding: b}
	for _, stream := range b.Streams() {
		c := Capture{store: newStreamStore(j.files, b, stream)}
		if err := c.initialize(); err != nil {
			t.Fatal(err)
		}
		if _, err := c.commit(captureBatch{data: []byte{0, 255}, observed: 2}); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Started(); err != nil {
		t.Fatal(err)
	}
}

func reopenTestJournal(t *testing.T, j *Journal) *Journal {
	t.Helper()
	current, err := Open(j.files.root.Name(), "new-boot", "new-daemon")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = current.Close() })
	return current
}
