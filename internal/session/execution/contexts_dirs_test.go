package execution

import (
	"errors"
	"testing"
)

// The context directories are made durable once per journal; later context
// requests pay no sync round for them.
func TestContextDirsAreEnsuredOncePerJournal(t *testing.T) {
	j := contextJournal(t)
	if err := j.ensureContextDirs(); err != nil {
		t.Fatal(err)
	}
	counts := syncCounts(j.files, func() {
		if err := j.ensureContextDirs(); err != nil {
			t.Fatal(err)
		}
	})
	if len(counts) != 0 {
		t.Fatalf("second ensure synced: %v", counts)
	}
}

// A failed ensure is not remembered, so the next request tries again.
func TestFailedContextDirsAreEnsuredAgain(t *testing.T) {
	j := contextJournal(t)
	boom := errors.New("injected directory sync failure")
	recordSteps(j.files, func(step string, _ int) error {
		if step == "directory_sync" {
			return boom
		}
		return nil
	})
	if err := j.ensureContextDirs(); !errors.Is(err, boom) {
		t.Fatalf("ensure with failing sync: %v", err)
	}
	j.files.fault = nil
	counts := syncCounts(j.files, func() {
		if err := j.ensureContextDirs(); err != nil {
			t.Fatal(err)
		}
	})
	if counts["directory_sync"] == 0 {
		t.Fatalf("retry after a failure synced nothing: %v", counts)
	}
	if !j.contextDirsReady.Load() {
		t.Fatal("successful retry not remembered")
	}
}
