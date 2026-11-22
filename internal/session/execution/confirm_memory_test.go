package execution

import (
	"fmt"
	"os"
	"testing"
)

// The tests here use the package's ordinary Files, so the confirmation memory
// is live; fault injection (which bypasses it) is set only where a test counts
// raw sync steps.

func TestPublishedRecordNeedsNoSecondConfirmation(t *testing.T) {
	j, _, _, _ := journalFixture(t)
	const name = "execution/memory.json"
	if _, err := j.files.Publish(name, map[string]string{"a": "b"}, false); err != nil {
		t.Fatal(err)
	}
	file, err := j.files.open(name, os.O_RDONLY)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	stamp, ok := j.files.stampOf(file)
	if !ok || !j.files.wasConfirmed(name, stamp) {
		t.Fatal("a durable publication was not remembered as confirmed")
	}
}

func TestChangedRecordIsConfirmedAgain(t *testing.T) {
	j, _, _, _ := journalFixture(t)
	const name = "execution/memory.json"
	if _, err := j.files.Publish(name, map[string]string{"a": "b"}, false); err != nil {
		t.Fatal(err)
	}
	// A replacement is a different file, so the memory of the old one must not
	// vouch for it until it has been confirmed itself.
	other := name + ".other"
	if _, err := j.files.Publish(other, map[string]string{"a": "c"}, false); err != nil {
		t.Fatal(err)
	}
	if err := j.files.root.Rename(other, name); err != nil {
		t.Fatal(err)
	}
	file, err := j.files.open(name, os.O_RDONLY)
	if err != nil {
		t.Fatal(err)
	}
	stamp, ok := j.files.stampOf(file)
	_ = file.Close()
	if !ok || j.files.wasConfirmed(name, stamp) {
		t.Fatal("a record replaced by rename was treated as already confirmed")
	}
	if err := j.files.Confirm(name); err != nil {
		t.Fatal(err)
	}
	if !j.files.wasConfirmed(name, stamp) {
		t.Fatal("a confirmed replacement was not remembered")
	}
}

func TestRewrittenRecordIsConfirmedAgain(t *testing.T) {
	j, _, _, _ := journalFixture(t)
	const name = "execution/memory.json"
	if _, err := j.files.Publish(name, map[string]string{"a": "b"}, false); err != nil {
		t.Fatal(err)
	}
	file, err := j.files.root.OpenFile(name, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(" "); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	read, err := j.files.open(name, os.O_RDONLY)
	if err != nil {
		t.Fatal(err)
	}
	stamp, ok := j.files.stampOf(read)
	_ = read.Close()
	if !ok || j.files.wasConfirmed(name, stamp) {
		t.Fatal("an appended record was treated as already confirmed")
	}
}

// Admission only counts active records. It once confirmed every retained
// receipt, so each create paid for every earlier one.
func TestReserveSyncCostDoesNotGrowWithRetainedReceipts(t *testing.T) {
	j, _, i, env := journalFixture(t)
	reserveOne := func(opaque string) int {
		k := Key{Host: "host", Class: "local-account", Principal: "account", Opaque: opaque}
		syncs := 0
		j.files.fault = func(operation string) error {
			if operation == "data_sync" {
				syncs++
			}
			return nil
		}
		defer func() { j.files.fault = nil }()
		if _, owner, err := reserveTest(t, j, k, i, env); err != nil || !owner {
			t.Fatalf("reserve %s: owner=%v err=%v", opaque, owner, err)
		}
		return syncs
	}
	first := reserveOne("first")
	for n := 0; n < 5; n++ {
		reserveOne(fmt.Sprintf("filler-%d", n))
	}
	if later := reserveOne("later"); later != first {
		t.Fatalf("reserve used %d data syncs with six retained receipts, %d with none", later, first)
	}
}
