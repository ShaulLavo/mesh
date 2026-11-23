package execution

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The result and every stream's final capture are confirmed in one round.
func TestResultConfirmsTheResultAndEveryStreamTogether(t *testing.T) {
	j, k, i, env := pipeJournalFixture(t)
	b, a := claimedAttempt(t, j, k, i, env)
	want := finishedPipe(t, j, b, a, 0)
	output, err := OpenOutput(j.files.root.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = output.Close() }()
	var got Result
	counts := syncCounts(output.files, func() {
		if got, err = output.Result(t.Context(), b); err != nil {
			t.Fatal(err)
		}
	})
	if len(got.Streams) != len(want.Streams) || got.Streams[0] != want.Streams[0] || got.Streams[1] != want.Streams[1] {
		t.Fatalf("result streams: %+v", got.Streams)
	}
	// result.json and both manifests; the attempt, stdout and stderr
	// directories, attempts, execution and the root, each once.
	if counts["data_sync"] != 3 || counts["directory_sync"] != 6 || counts["device_sync"] != 1 {
		t.Fatalf("result confirmation steps: %v", counts)
	}
}

// A dispatching reservation saves the reserved receipt, the dispatch record
// and the dispatched receipt under one lock hold, ready for its worker.
func TestReserveAndDispatchIsReadyForItsWorker(t *testing.T) {
	j, k, i, env := pipeJournalFixture(t)
	r, owner, err := j.ReserveAndDispatch(t.Context(), k, i, func() (Binding, error) { return newTestBinding(t, j, k, i, env) })
	if err != nil || !owner || r.Phase != phaseDispatched || r.LaunchOutcome != outcomeUnknown {
		t.Fatalf("reserve and dispatch: owner=%v phase=%s outcome=%s err=%v", owner, r.Phase, r.LaunchOutcome, err)
	}
	a, err := StartAttempt(t.Context(), j.files.root.Name(), r.Binding, i, env)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	_ = a.Close()
}

// The reserved receipt is durable before the dispatch record is written, so
// a coordinator that fails between them still settles as not_started.
func TestReserveAndDispatchKeepsTheReservedReceiptFirst(t *testing.T) {
	j, k, i, env := pipeJournalFixture(t)
	var blocked string
	r, owner, err := j.ReserveAndDispatch(t.Context(), k, i, func() (Binding, error) {
		b, err := newTestBinding(t, j, k, i, env)
		if err != nil {
			return b, err
		}
		// A directory where the dispatch record goes makes its exclusive
		// publication fail after the reserved receipt was saved.
		blocked = filepath.Join(j.files.root.Name(), attempt(b, "dispatch.json"))
		return b, os.MkdirAll(blocked, 0700)
	})
	if err == nil || !owner || r.Phase != phaseReserved {
		t.Fatalf("failed dispatch: owner=%v phase=%s err=%v", owner, r.Phase, err)
	}
	var saved Receipt
	if err := j.files.Read(requestName(k.Hash()), &saved); err != nil || saved.Phase != phaseReserved {
		t.Fatalf("receipt on disk: %s %v", saved.Phase, err)
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	replacement, err := Open(j.files.root.Name(), "boot", "replacement")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = replacement.Close() }()
	settled, err := replacement.Lookup(t.Context(), k)
	if err != nil || settled.Phase != phaseTerminal || settled.LaunchOutcome != notStarted {
		t.Fatalf("replacement lookup: phase=%s outcome=%s err=%v", settled.Phase, settled.LaunchOutcome, err)
	}
}

// An existing key is returned as Reserve returns it and never dispatched again.
func TestReserveAndDispatchReplaysAnExistingKey(t *testing.T) {
	j, k, i, env := pipeJournalFixture(t)
	first, _ := claimedAttempt(t, j, k, i, env)
	r, owner, err := j.ReserveAndDispatch(t.Context(), k, i, func() (Binding, error) {
		t.Fatal("allocated a second attempt for an existing key")
		return Binding{}, nil
	})
	if owner || r.Binding != first || !errors.Is(err, ErrUnknown) {
		t.Fatalf("replay: owner=%v binding=%v err=%v", owner, r.Binding == first, err)
	}
}

// A context job's launch snapshot and membership marker share the attempt's
// round, and a dispatching reservation is ready for its worker to claim.
func TestContextJobDispatchesWithItsSnapshotAndMembership(t *testing.T) {
	j := contextJournal(t)
	c := openTestContext(t, j, ownerA, "ctx-dispatch", j.files.root.Name())
	k := jobKey(ownerA, c.ID, "job-dispatch")
	resolve := func(c Context) (Intent, map[string]string, error) {
		return Intent{Kind: KindPipe, Command: []string{"/bin/true"}, Cwd: c.Cwd, Stdin: StdinClosed}, nil, nil
	}
	env := []string{"PATH=/bin"}
	r, owner, err := j.ReserveInContextAndDispatch(t.Context(), k, Digest("job"), resolve, func(s LaunchSnapshot) (Binding, error) {
		return newTestBinding(t, j, k, s.Intent, env)
	})
	if err != nil || !owner || r.Phase != phaseDispatched {
		t.Fatalf("reserve context job: owner=%v phase=%s err=%v", owner, r.Phase, err)
	}
	snapshot, err := j.ReadLaunchSnapshot(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.files.Confirm(contextMember(c.ID, r.Binding.RequestHash)); err != nil {
		t.Fatalf("membership marker: %v", err)
	}
	a, err := StartAttempt(t.Context(), j.files.root.Name(), r.Binding, snapshot.Intent, env)
	if err != nil {
		t.Fatalf("claim dispatched context job: %v", err)
	}
	_ = a.Close()
}

// Every stream's directory, locks and first manifest take two rounds in all.
func TestOpenCapturesSetsUpEveryStreamInTwoRounds(t *testing.T) {
	j, k, i, env := pipeJournalFixture(t)
	b, _ := claimedAttempt(t, j, k, i, env)
	stores := []*streamStore{newStreamStore(j.files, b, StreamStdout), newStreamStore(j.files, b, StreamStderr)}
	counts := syncCounts(j.files, func() {
		if err := initializeStreams(stores); err != nil {
			t.Fatal(err)
		}
	})
	if counts["device_sync"] != 2 || counts["publish"] != 2 {
		t.Fatalf("capture setup steps: %v", counts)
	}
	for _, s := range stores {
		for _, name := range []string{"stream.lock", readerLockName, writerLockName, "manifest.json"} {
			if err := j.files.check(s.path(name), false); err != nil {
				t.Fatalf("%s: %v", s.path(name), err)
			}
		}
	}
}

func TestOpenCapturesRejectsARepeatedStream(t *testing.T) {
	j, k, i, env := pipeJournalFixture(t)
	b, _ := claimedAttempt(t, j, k, i, env)
	if _, err := OpenCaptures(j.files.root.Name(), b, []string{StreamStdout, StreamStdout}); err == nil {
		t.Fatal("opened one stream twice")
	}
}
