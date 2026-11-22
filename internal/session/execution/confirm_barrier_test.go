package execution

import (
	"fmt"
	"testing"
)

// syncCounts records how many of each sync step run inside fn, with the
// confirmation memory active.
func syncCounts(f *Files, fn func()) map[string]int {
	counts := map[string]int{}
	f.observe = func(operation string) { counts[operation]++ }
	defer func() { f.observe = nil }()
	fn()
	return counts
}

func publishRecord(t *testing.T, f *Files, name string) {
	t.Helper()
	if _, err := f.Publish(name, map[string]string{"k": name}, false); err != nil {
		t.Fatal(err)
	}
}

// A rename can be undone or repeated without touching the file, so a
// remembered file must still have its directories synced on every confirm.
func TestConfirmingARememberedRecordStillSyncsItsDirectories(t *testing.T) {
	j, _, _, _ := journalFixture(t)
	const name = "execution/barrier.json"
	publishRecord(t, j.files, name)
	counts := syncCounts(j.files, func() {
		if err := j.files.Confirm(name); err != nil {
			t.Fatal(err)
		}
	})
	if counts["directory_sync"] == 0 {
		t.Fatalf("a confirmed record skipped its directory syncs: %v", counts)
	}
	if counts["data_sync"] != 0 {
		t.Fatalf("an unchanged remembered record was fsynced again: %v", counts)
	}
}

// Only records are remembered. Anchors and locks carry no bytes to vouch for
// and are the barrier other namespaces rely on.
func TestAnchorsAreNeverRemembered(t *testing.T) {
	j, _, _, _ := journalFixture(t)
	const anchor = "execution/.barrier"
	for range 2 {
		counts := syncCounts(j.files, func() {
			if err := j.files.Confirm(anchor); err != nil {
				t.Fatal(err)
			}
		})
		if counts["data_sync"] == 0 || counts["directory_sync"] == 0 {
			t.Fatalf("a barrier anchor was skipped: %v", counts)
		}
	}
}

func smallConfirmationBound(t *testing.T) {
	t.Helper()
	saved := maxConfirmed
	maxConfirmed = 8
	t.Cleanup(func() { maxConfirmed = saved })
}

func TestConfirmationMemoryStaysBoundedUnderChurn(t *testing.T) {
	smallConfirmationBound(t)
	j, _, _, _ := journalFixture(t)
	for i := range 3 * maxConfirmed {
		name := fmt.Sprintf("execution/churn-%d.json", i)
		publishRecord(t, j.files, name)
		if err := j.files.Remove(name, "execution/.barrier"); err != nil {
			t.Fatal(err)
		}
	}
	j.files.confirmMu.Lock()
	size := len(j.files.confirmed)
	j.files.confirmMu.Unlock()
	if size != 0 {
		t.Fatalf("%d removed paths are still remembered", size)
	}
}

func TestConfirmationMemoryIsCappedWithoutRemovals(t *testing.T) {
	smallConfirmationBound(t)
	j, _, _, _ := journalFixture(t)
	for i := range 3 * maxConfirmed {
		publishRecord(t, j.files, fmt.Sprintf("execution/kept-%d.json", i))
	}
	j.files.confirmMu.Lock()
	size := len(j.files.confirmed)
	j.files.confirmMu.Unlock()
	if size > maxConfirmed {
		t.Fatalf("%d paths remembered, limit %d", size, maxConfirmed)
	}
}

func TestRemovingATreeForgetsItsRecords(t *testing.T) {
	j, _, _, _ := journalFixture(t)
	if err := j.files.Ensure("execution/tree"); err != nil {
		t.Fatal(err)
	}
	publishRecord(t, j.files, "execution/tree/a.json")
	publishRecord(t, j.files, "execution/tree/b.json")
	if err := j.files.RemoveTree("execution/tree", "execution/.barrier"); err != nil {
		t.Fatal(err)
	}
	j.files.confirmMu.Lock()
	size := len(j.files.confirmed)
	j.files.confirmMu.Unlock()
	if size != 0 {
		t.Fatalf("%d paths of a removed tree are still remembered", size)
	}
	if _, err := j.files.root.Lstat("execution/tree"); err == nil {
		t.Fatal("tree still exists")
	}
}
