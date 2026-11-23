package execution

import (
	"testing"
)

// pipeJournalFixture is journalFixture for a pipe job with closed stdin.
func pipeJournalFixture(t *testing.T) (*Journal, Key, Intent, []string) {
	t.Helper()
	j, k, _, env := journalFixture(t)
	i, err := Normalize(Intent{Kind: KindPipe, Command: []string{"/bin/true"}, Cwd: j.files.root.Name(), Stdin: StdinClosed})
	if err != nil {
		t.Fatal(err)
	}
	return j, k, i, env
}

// claimedAttempt reserves, dispatches and claims one job as its worker would.
func claimedAttempt(t *testing.T, j *Journal, k Key, i Intent, env []string) (Binding, *Attempt) {
	t.Helper()
	r, owner, err := reserveTest(t, j, k, i, env)
	if err != nil || !owner {
		t.Fatalf("reserve: owner=%v err=%v", owner, err)
	}
	if err := j.Dispatch(t.Context(), r.Binding, i); err != nil {
		t.Fatal(err)
	}
	a, err := StartAttempt(t.Context(), j.files.root.Name(), r.Binding, i, env)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return r.Binding, a
}

// finishedPipe runs a pipe job's captures and result to completion.
func finishedPipe(t *testing.T, j *Journal, b Binding, a *Attempt, code int) Result {
	t.Helper()
	result := Result{Binding: b, Termination: Termination{Kind: "exit", Code: &code}, Capture: "complete"}
	for _, name := range b.Streams() {
		c, err := OpenCapture(j.files.root.Name(), b, name)
		if err != nil {
			t.Fatal(err)
		}
		stream, err := c.Finish(t.Context(), captureEOF)
		if err != nil {
			t.Fatal(err)
		}
		result.Streams = append(result.Streams, stream)
	}
	if err := a.Finish(result); err != nil {
		t.Fatal(err)
	}
	return result
}

// A job that finished before the daemon recorded its publication is saved
// once, as terminal, instead of as live and then as terminal.
func TestPublishedSavesAFinishedJobOnceAsTerminal(t *testing.T) {
	j, k, i, env := pipeJournalFixture(t)
	b, a := claimedAttempt(t, j, k, i, env)
	finishedPipe(t, j, b, a, 7)
	var got Receipt
	counts := syncCounts(j.files, func() {
		var err error
		if got, err = j.Published(t.Context(), b); err != nil {
			t.Fatal(err)
		}
	})
	if got.Phase != phaseTerminal || got.Publication != publicationPublished || got.Result == nil || *got.Result.Termination.Code != 7 {
		t.Fatalf("published receipt: %+v", got)
	}
	if counts["publish"] != 1 {
		t.Fatalf("receipt saved %d times: %v", counts["publish"], counts)
	}
	again, err := j.Lookup(t.Context(), k)
	if err != nil || again.Phase != phaseTerminal || again.Publication != publicationPublished {
		t.Fatalf("lookup after publication: %+v %v", again, err)
	}
}

// Publishing a running job replaces its receipt in one round: the receipt
// it replaces is not confirmed first.
func TestPublishedReplacesARunningJobsReceiptInOneRound(t *testing.T) {
	j, k, i, env := pipeJournalFixture(t)
	b, _ := claimedAttempt(t, j, k, i, env)
	var got Receipt
	counts := syncCounts(j.files, func() {
		var err error
		if got, err = j.Published(t.Context(), b); err != nil {
			t.Fatal(err)
		}
	})
	if got.Phase != phaseLive || got.Publication != publicationPublished || got.LaunchOutcome != outcomeStarted {
		t.Fatalf("published receipt: %+v", got)
	}
	if counts["publish"] != 1 || counts["device_sync"] != 1 || counts["data_sync"] != 1 {
		t.Fatalf("publication steps: %v", counts)
	}
}

// A terminal receipt that is already published is returned unchanged, and so
// is confirmed rather than replaced.
func TestPublishedConfirmsAnUnchangedTerminalReceipt(t *testing.T) {
	j, k, i, env := pipeJournalFixture(t)
	b, a := claimedAttempt(t, j, k, i, env)
	finishedPipe(t, j, b, a, 0)
	if _, err := j.Published(t.Context(), b); err != nil {
		t.Fatal(err)
	}
	counts := syncCounts(j.files, func() {
		got, err := j.Published(t.Context(), b)
		if err != nil || got.Phase != phaseTerminal {
			t.Fatalf("repeated publication: %+v %v", got, err)
		}
	})
	if counts["publish"] != 0 {
		t.Fatalf("an unchanged terminal receipt was saved again: %v", counts)
	}
	if _, err := j.Lookup(t.Context(), k); err != nil {
		t.Fatal(err)
	}
}

// Complete settles the receipt with the result Output already confirmed,
// without confirming it again, and returns the same receipt to a repeat.
func TestCompleteSettlesWithTheConfirmedResult(t *testing.T) {
	j, k, i, env := pipeJournalFixture(t)
	b, a := claimedAttempt(t, j, k, i, env)
	want := finishedPipe(t, j, b, a, 3)
	result, err := j.FinalResult(t.Context(), b)
	if err != nil || len(result.Streams) != 2 || result.Streams[0] != want.Streams[0] || result.Streams[1] != want.Streams[1] {
		t.Fatalf("confirmed result: %+v %v", result, err)
	}
	var got Receipt
	counts := syncCounts(j.files, func() {
		if got, err = j.Complete(t.Context(), b, result); err != nil {
			t.Fatal(err)
		}
	})
	if got.Phase != phaseTerminal || *got.Result.Termination.Code != 3 {
		t.Fatalf("completed receipt: %+v", got)
	}
	// Only the terminal receipt's own round: no capture or result confirmation.
	if counts["publish"] != 1 || counts["device_sync"] != 1 {
		t.Fatalf("completion steps: %v", counts)
	}
	var repeat Receipt
	counts = syncCounts(j.files, func() {
		if repeat, err = j.Complete(t.Context(), b, result); err != nil {
			t.Fatal(err)
		}
	})
	if repeat.Phase != phaseTerminal || *repeat.Result.Termination.Code != 3 {
		t.Fatalf("repeated completion: %+v", repeat)
	}
	// The unchanged receipt is returned only after its own barriers: the
	// requests directory, execution and the root, then the device.
	if counts["publish"] != 0 || counts["directory_sync"] != 3 || counts["device_sync"] != 1 {
		t.Fatalf("an unchanged terminal receipt was not confirmed: %v", counts)
	}
	other := result
	other.Binding.Reservation.Instance = "00000000000000000000000000000000"
	if _, err := j.Complete(t.Context(), b, other); err == nil {
		t.Fatal("completed with another attempt's result")
	}
	if _, err := j.Lookup(t.Context(), k); err != nil {
		t.Fatal(err)
	}
}
