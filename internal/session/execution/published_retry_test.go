package execution

import (
	"errors"
	"testing"
)

// A publication whose replacement receipt fails, before or after its rename,
// leaves the original binding in place, and a retry publishes that binding.
func TestPublishedRetryAfterAFailedReplacement(t *testing.T) {
	for _, failed := range []string{"publish", "directory_sync"} {
		t.Run(failed, func(t *testing.T) { testPublishedRetry(t, failed) })
	}
}

func testPublishedRetry(t *testing.T, failed string) {
	j, k, i, env := pipeJournalFixture(t)
	b, _ := claimedAttempt(t, j, k, i, env)
	boom := errors.New("injected " + failed)
	j.files.fault = func(step string) error {
		if step == failed {
			return boom
		}
		return nil
	}
	_, err := j.Published(t.Context(), b)
	j.files.fault = nil
	if !errors.Is(err, boom) {
		t.Fatalf("failed publication: %v", err)
	}
	retry, err := j.Published(t.Context(), b)
	if err != nil || retry.Binding != b || retry.Phase != phaseLive || retry.Publication != publicationPublished {
		t.Fatalf("retried publication: %+v %v", retry, err)
	}
	found, err := j.Lookup(t.Context(), k)
	if err != nil || found.Binding != b {
		t.Fatalf("lookup after retry: %+v %v", found, err)
	}
}

// An output reader that saw a final manifest before the result existed must
// not let the wait path trust that earlier read: FinalResult confirms every
// final capture again with its own barriers.
func TestFinalResultConfirmsCapturesAReaderSawEarly(t *testing.T) {
	j, k, i, env := pipeJournalFixture(t)
	b, a := claimedAttempt(t, j, k, i, env)
	result := Result{Binding: b, Termination: Termination{Kind: "exit", Code: new(int)}, Capture: "complete"}
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
	reader, err := OpenOutput(j.files.root.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	for _, name := range b.Streams() {
		if _, err := reader.Page(t.Context(), b, name, "", MaxPage); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Finish(result); err != nil {
		t.Fatal(err)
	}
	// The warm reader trusts its earlier read and syncs only the result,
	// which is why execution.wait does not settle through it.
	warm := syncCounts(reader.files, func() {
		if _, err := reader.Result(t.Context(), b); err != nil {
			t.Fatal(err)
		}
	})
	if warm["data_sync"] != 1 {
		t.Fatalf("warm reader steps: %v", warm)
	}
	counts := syncCounts(j.files, func() {
		if _, err := j.FinalResult(t.Context(), b); err != nil {
			t.Fatal(err)
		}
	})
	// Both final manifests and the result have their own file syncs.
	if counts["data_sync"] != 3 {
		t.Fatalf("final captures were not confirmed again: %v", counts)
	}
}
