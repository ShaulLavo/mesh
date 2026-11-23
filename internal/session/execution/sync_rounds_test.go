package execution

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Both streams' final manifests share one round, and the result, which
// depends on them, takes the next.
func TestResultConfirmsManifestsTogetherThenTheResult(t *testing.T) {
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
	// Both manifests with the stdout, stderr, attempt, attempts, execution and
	// root directories; then result.json with the last four again.
	if counts["data_sync"] != 3 || counts["directory_sync"] != 10 || counts["device_sync"] != 2 {
		t.Fatalf("result confirmation steps: %v", counts)
	}
}

// Captured segments are prerequisites of their manifests, which are
// prerequisites of the result: a failed segment barrier confirms nothing
// that depends on it.
func TestResultConfirmsNothingPastAFailedSegmentBarrier(t *testing.T) {
	j, k, i, env := pipeJournalFixture(t)
	b, a := claimedAttempt(t, j, k, i, env)
	result := Result{Binding: b, Termination: Termination{Kind: "exit", Code: new(int)}, Capture: "complete"}
	captures, err := OpenCaptures(j.files.root.Name(), b, b.Streams())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range captures {
		c.Offer([]byte("output"))
		stream, err := c.Finish(t.Context(), captureEOF)
		if err != nil {
			t.Fatal(err)
		}
		result.Streams = append(result.Streams, stream)
	}
	if err := a.Finish(result); err != nil {
		t.Fatal(err)
	}
	output, err := OpenOutput(j.files.root.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = output.Close() }()
	boom := errors.New("injected segment barrier failure")
	steps := recordSteps(output.files, func(step string, _ int) error {
		if step == "device_sync" {
			return boom
		}
		return nil
	})
	if _, err := output.Result(t.Context(), b); !errors.Is(err, boom) {
		t.Fatalf("result after a failed barrier: %v", err)
	}
	// Only the two streams' segments were synced.
	if got := count(*steps, "data_sync"); got != 2 {
		t.Fatalf("data syncs = %d, want the two segments only: %v", got, *steps)
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

// coordinatorExit stands in for a coordinator process that dies at a sync
// step: the step panics, the test recovers, and nothing after it runs.
type coordinatorExit struct{}

// exitAtPublish makes the coordinator die at the nth publication after it is
// installed, before that record is renamed.
func exitAtPublish(f *Files, n int) {
	seen := 0
	f.fault = func(step string) error {
		if step != "publish" {
			return nil
		}
		seen++
		if seen == n {
			panic(coordinatorExit{})
		}
		return nil
	}
}

// runUntilExit runs fn and reports whether the coordinator died in it.
func runUntilExit(fn func()) (exited bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if _, ok := recovered.(coordinatorExit); !ok {
				panic(recovered)
			}
			exited = true
		}
	}()
	fn()
	return false
}

// settledByReplacement looks k up from a new coordinator, as after a restart.
func settledByReplacement(t *testing.T, j *Journal, k Key) Receipt {
	t.Helper()
	replacement, err := Open(j.files.root.Name(), "boot", "replacement")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = replacement.Close() }()
	settled, err := replacement.Lookup(t.Context(), k)
	if err != nil {
		t.Fatalf("replacement lookup: %v", err)
	}
	return settled
}

// The reserved receipt is durable before the dispatch record is written, so a
// coordinator that dies after acceptance and before dispatch still settles as
// not_started.
func TestCoordinatorExitBeforeDispatchSettlesNotStarted(t *testing.T) {
	j, k, i, env := pipeJournalFixture(t)
	exited := runUntilExit(func() {
		_, _, _ = j.ReserveAndDispatch(t.Context(), k, i, func() (Binding, error) {
			b, err := newTestBinding(t, j, k, i, env)
			// After the allocation: the attempt round, the reserved receipt,
			// then the dispatch record, where the coordinator dies.
			exitAtPublish(j.files, 3)
			return b, err
		})
	})
	j.files.fault = nil
	if !exited {
		t.Fatal("the coordinator did not reach its dispatch")
	}
	var saved Receipt
	if err := j.files.Read(requestName(k.Hash()), &saved); err != nil || saved.Phase != phaseReserved {
		t.Fatalf("receipt on disk: %s %v", saved.Phase, err)
	}
	if settled := settledByReplacement(t, j, k); settled.Phase != phaseTerminal || settled.LaunchOutcome != notStarted {
		t.Fatalf("replacement settled phase=%s outcome=%s", settled.Phase, settled.LaunchOutcome)
	}
}

// A context job is dispatched only after the context's use is recorded and
// its launch snapshot reads back, so a coordinator that dies at the dispatch
// leaves a reserved receipt and a recorded use, and settles as not_started.
func TestContextCoordinatorExitBeforeDispatchSettlesNotStarted(t *testing.T) {
	j := contextJournal(t)
	c := openTestContext(t, j, ownerA, "ctx-exit", j.files.root.Name())
	k := jobKey(ownerA, c.ID, "job-exit")
	resolve := func(c Context) (Intent, map[string]string, error) {
		return Intent{Kind: KindPipe, Command: []string{"/bin/true"}, Cwd: c.Cwd, Stdin: StdinClosed}, nil, nil
	}
	j.now = func() time.Time { return c.LastUsedAt.Add(time.Hour) }
	exited := runUntilExit(func() {
		_, _, _ = j.ReserveInContextAndDispatch(t.Context(), k, Digest("job"), resolve, func(s LaunchSnapshot) (Binding, error) {
			b, err := newTestBinding(t, j, k, s.Intent, []string{"PATH=/bin"})
			// After the allocation: the attempt round's reference, snapshot
			// and membership marker, the reserved receipt, the context's use,
			// then the dispatch record.
			exitAtPublish(j.files, 6)
			return b, err
		})
	})
	j.files.fault = nil
	if !exited {
		t.Fatal("the coordinator did not reach its dispatch")
	}
	var saved Receipt
	if err := j.files.Read(requestName(k.Hash()), &saved); err != nil || saved.Phase != phaseReserved {
		t.Fatalf("receipt on disk: %s %v", saved.Phase, err)
	}
	if _, err := os.Stat(filepath.Join(j.files.root.Name(), attempt(saved.Binding, "dispatch.json"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dispatch record: %v", err)
	}
	used, err := j.ownedContext(ownerA, c.ID)
	if err != nil || !used.LastUsedAt.After(c.LastUsedAt) {
		t.Fatalf("context use was not recorded before dispatch: %v %v", used.LastUsedAt, err)
	}
	if settled := settledByReplacement(t, j, k); settled.Phase != phaseTerminal || settled.LaunchOutcome != notStarted {
		t.Fatalf("replacement settled phase=%s outcome=%s", settled.Phase, settled.LaunchOutcome)
	}
}

// A dispatched receipt that fails to save, before or after its rename, is
// reported with the reserved receipt's proven acceptance and phase, and the
// failure separately.
func TestFailedDispatchSaveKeepsTheProvenAcceptance(t *testing.T) {
	for _, failed := range []string{"publish", "directory_sync"} {
		t.Run(failed, func(t *testing.T) { testFailedDispatchSave(t, failed) })
	}
}

func testFailedDispatchSave(t *testing.T, failed string) {
	j, k, i, env := pipeJournalFixture(t)
	boom := errors.New("injected " + failed)
	r, owner, err := j.ReserveAndDispatch(t.Context(), k, i, func() (Binding, error) {
		b, err := newTestBinding(t, j, k, i, env)
		// The attempt round, the reserved receipt and the dispatch record
		// succeed; the dispatched receipt fails at the given step.
		publishes := 0
		j.files.fault = func(step string) error {
			if step == "publish" {
				publishes++
			}
			if publishes == 4 && step == failed {
				return boom
			}
			return nil
		}
		return b, err
	})
	j.files.fault = nil
	if !errors.Is(err, boom) || !owner {
		t.Fatalf("failed dispatch save: owner=%v err=%v", owner, err)
	}
	if r.Phase != phaseReserved || r.Acceptance != string(Durable) || r.LaunchOutcome != notStarted {
		t.Fatalf("reported phase=%s acceptance=%s outcome=%s", r.Phase, r.Acceptance, r.LaunchOutcome)
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
	if _, err := OpenCaptures(j.files.root.Name(), b, nil); err == nil {
		t.Fatal("opened no streams")
	}
}
