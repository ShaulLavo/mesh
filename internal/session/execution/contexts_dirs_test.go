package execution

import (
	"errors"
	"path/filepath"
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
	assertContextDirsSkipped(t, j)
}

// A failed write anywhere on the journal's files can leave a visible rename
// whose directory was never synced, such as a context receipt whose round
// failed after its rename. The next ensure syncs the directories again so a
// retry cannot acknowledge that receipt while it can still vanish.
func TestAFailedWriteMakesTheNextEnsureSyncAgain(t *testing.T) {
	j := contextJournal(t)
	if err := j.ensureContextDirs(); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("injected directory sync failure after a rename")
	recordSteps(j.files, func(step string, _ int) error {
		if step == "directory_sync" {
			return boom
		}
		return nil
	})
	if _, err := j.files.Publish(filepath.Join(contextRequestDir, "r.json"), 1, false); !errors.Is(err, boom) {
		t.Fatalf("publish with failing sync: %v", err)
	}
	if err := j.ensureContextDirs(); !errors.Is(err, boom) {
		t.Fatalf("ensure after a failed write skipped its syncs: %v", err)
	}
	j.files.fault = nil
	counts := syncCounts(j.files, func() {
		if err := j.ensureContextDirs(); err != nil {
			t.Fatal(err)
		}
	})
	if counts["directory_sync"] == 0 {
		t.Fatalf("ensure after a failed write synced nothing: %v", counts)
	}
	assertContextDirsSkipped(t, j)
}

// A shared directory removed while the daemon runs is created again instead
// of failing every later context request.
func TestAMissingContextDirIsCreatedAgain(t *testing.T) {
	j := contextJournal(t)
	if err := j.ensureContextDirs(); err != nil {
		t.Fatal(err)
	}
	if err := j.files.root.RemoveAll(contextRequestDir); err != nil {
		t.Fatal(err)
	}
	if err := j.ensureContextDirs(); err != nil {
		t.Fatal(err)
	}
	if info, err := j.files.root.Lstat(contextRequestDir); err != nil || !info.IsDir() {
		t.Fatalf("context request directory after ensure: %v %v", info, err)
	}
	assertContextDirsSkipped(t, j)
}

func assertContextDirsSkipped(t *testing.T, j *Journal) {
	t.Helper()
	counts := syncCounts(j.files, func() {
		if err := j.ensureContextDirs(); err != nil {
			t.Fatal(err)
		}
	})
	if len(counts) != 0 {
		t.Fatalf("ensure after a success synced: %v", counts)
	}
}
