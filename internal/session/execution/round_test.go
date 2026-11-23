package execution

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func roundFiles(t *testing.T) *Files {
	t.Helper()
	f, err := OpenFiles(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// recordSteps records every sync step in the order the hooks see them.
func recordSteps(f *Files, fail func(step string, seen int) error) *[]string {
	steps := []string{}
	f.fault = func(step string) error {
		steps = append(steps, step)
		if fail == nil {
			return nil
		}
		return fail(step, count(steps, step))
	}
	return &steps
}

func count(steps []string, step string) int {
	n := 0
	for _, s := range steps {
		if s == step {
			n++
		}
	}
	return n
}

// A new directory chain is synced once from the deepest directory up, with
// one device barrier, rather than once per component.
func TestEnsureSyncsANewChainInOneRound(t *testing.T) {
	f := roundFiles(t)
	steps := recordSteps(f, nil)
	if err := f.Ensure("a/b/c"); err != nil {
		t.Fatal(err)
	}
	if got := count(*steps, "directory_sync"); got != 4 {
		t.Fatalf("directory syncs = %d, want 4 (c, b, a and the root): %v", got, *steps)
	}
	if got := count(*steps, "device_sync"); got != 1 {
		t.Fatalf("device barriers = %d, want 1: %v", got, *steps)
	}
	if got := count(*steps, "data_sync"); got != 3 {
		t.Fatalf("anchor syncs = %d, want one per directory: %v", got, *steps)
	}
	for _, dir := range []string{"a", "a/b", "a/b/c"} {
		if err := f.check(dir+"/.barrier", false); err != nil {
			t.Fatalf("anchor in %s: %v", dir, err)
		}
	}
}

// Every file is synced before any rename, and every rename precedes the
// first directory sync, so sharing a round proves what separate rounds would.
func TestRoundSyncsEveryFileBeforeRenamingAndRenamesBeforeDirectories(t *testing.T) {
	f := roundFiles(t)
	if err := f.Ensure("x/y"); err != nil {
		t.Fatal(err)
	}
	steps := recordSteps(f, nil)
	_, err := f.PublishAll(
		Record{Name: "x/one.json", Value: 1, Exclusive: true},
		Record{Name: "x/y/two.json", Value: 2},
		Record{Name: "three.json", Value: 3, Exclusive: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	lastData := lastIndex(*steps, "data_sync")
	firstPublish := slices.Index(*steps, "publish")
	lastPublish := lastIndex(*steps, "publish")
	firstDirectory := slices.Index(*steps, "directory_sync")
	if count(*steps, "data_sync") != 3 || lastData > firstPublish || lastPublish > firstDirectory {
		t.Fatalf("round order: %v", *steps)
	}
	// x/y, x and the root, each once.
	if got := count(*steps, "directory_sync"); got != 3 {
		t.Fatalf("directory syncs = %d, want 3: %v", got, *steps)
	}
	if got := count(*steps, "device_sync"); got != 1 {
		t.Fatalf("device barriers = %d, want 1: %v", got, *steps)
	}
	for name, want := range map[string]int{"x/one.json": 1, "x/y/two.json": 2, "three.json": 3} {
		var got int
		if err := f.Read(name, &got); err != nil || got != want {
			t.Fatalf("%s = %d, %v", name, got, err)
		}
	}
}

func lastIndex(steps []string, step string) int {
	last := -1
	for i, s := range steps {
		if s == step {
			last = i
		}
	}
	return last
}

// A failed rename reports exactly the records that became visible before it.
func TestPublishAllReportsTheNamesThatBecameVisible(t *testing.T) {
	f := roundFiles(t)
	boom := errors.New("injected publish failure")
	recordSteps(f, func(step string, seen int) error {
		if step == "publish" && seen == 2 {
			return boom
		}
		return nil
	})
	visible, err := f.PublishAll(
		Record{Name: "first.json", Value: 1, Exclusive: true},
		Record{Name: "second.json", Value: 2, Exclusive: true},
	)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if !slices.Equal(visible, []string{"first.json"}) {
		t.Fatalf("visible = %v", visible)
	}
	f.fault = nil
	if err := f.check("second.json", false); err == nil {
		t.Fatal("a record that failed to publish is visible")
	}
	names, err := os.ReadDir(f.root.Name())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range names {
		if strings.HasPrefix(entry.Name(), ".tmp-") {
			t.Fatalf("temporary file %s left behind", entry.Name())
		}
	}
}

// A failed directory barrier leaves every renamed record visible but
// unconfirmed, and nothing is remembered as durable.
func TestRoundBarrierFailureRemembersNothing(t *testing.T) {
	f := roundFiles(t)
	// The memory stays active, so a record would be remembered if the
	// failed barrier were treated as proof.
	f.faultKeepsMemory = true
	failing := true
	boom := errors.New("injected directory failure")
	recordSteps(f, func(step string, _ int) error {
		if failing && step == "directory_sync" {
			return boom
		}
		return nil
	})
	visible, err := f.PublishAll(Record{Name: "a.json", Value: 1}, Record{Name: "b.json", Value: 2})
	var persistence *PersistenceError
	if !errors.As(err, &persistence) || persistence.Stage != Unconfirmed || len(visible) != 2 {
		t.Fatalf("visible %v, err %v", visible, err)
	}
	if remembered := rememberedCount(f); remembered != 0 {
		t.Fatalf("%d unconfirmed records were remembered", remembered)
	}
	// The same memory does remember records once their barriers succeed, so
	// the assertion above is not vacuous.
	failing = false
	if _, err := f.PublishAll(Record{Name: "c.json", Value: 3}); err != nil {
		t.Fatal(err)
	}
	if remembered := rememberedCount(f); remembered != 1 {
		t.Fatalf("%d records remembered after a successful round, want 1", remembered)
	}
}

func rememberedCount(f *Files) int {
	f.confirmMu.Lock()
	defer f.confirmMu.Unlock()
	return len(f.confirmed)
}

// Each device gets one barrier, and a file whose device cannot be read gets
// a barrier of its own.
func TestRoundFlushesEachDeviceOnce(t *testing.T) {
	f := roundFiles(t)
	devices := map[string]uint64{"a.json": 1, "b.json": 2, "c.json": 1}
	saved := fileDevice
	fileDevice = func(file *os.File) (uint64, bool) {
		dev, ok := devices[filepath.Base(file.Name())]
		return dev, ok
	}
	t.Cleanup(func() { fileDevice = saved })
	confirm := func(names ...string) map[string]int {
		return syncCounts(f, func() {
			if err := f.ConfirmAll(names...); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, name := range []string{"a.json", "b.json", "c.json", "u.json", "v.json"} {
		publishRecord(t, f, name)
	}
	if got := confirm("a.json", "b.json", "c.json")["device_sync"]; got != 2 {
		t.Fatalf("two devices took %d barriers", got)
	}
	// u.json and v.json have no known device, so neither shares a barrier.
	if got := confirm("a.json", "u.json", "v.json")["device_sync"]; got != 3 {
		t.Fatalf("one known and two unknown devices took %d barriers", got)
	}
}

// A dependent group is not confirmed when its prerequisite's barrier fails.
func TestConfirmInOrderStopsAtAFailedPrerequisite(t *testing.T) {
	f := roundFiles(t)
	for _, name := range []string{"first.json", "second.json", "third.json"} {
		publishRecord(t, f, name)
	}
	boom := errors.New("injected device failure")
	steps := recordSteps(f, func(step string, _ int) error {
		if step == "device_sync" {
			return boom
		}
		return nil
	})
	err := confirmInOrder(f, []string{"first.json", "second.json"}, []string{"third.json"})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	// Only the prerequisites' own files were synced.
	if got := count(*steps, "data_sync"); got != 2 {
		t.Fatalf("data syncs = %d, want the two prerequisites only: %v", got, *steps)
	}
}

// Confirming several records syncs each file, each shared directory once and
// the device once.
func TestConfirmAllSharesDirectoriesAndTheDeviceBarrier(t *testing.T) {
	f := roundFiles(t)
	if err := f.Ensure("p/q"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"p/a.json", "p/q/b.json", "p/q/c.json"} {
		publishRecord(t, f, name)
	}
	fresh, err := OpenFiles(f.root.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fresh.Close() }()
	counts := syncCounts(fresh, func() {
		if err := fresh.ConfirmAll("p/a.json", "p/q/b.json", "p/q/c.json"); err != nil {
			t.Fatal(err)
		}
	})
	if counts["data_sync"] != 3 || counts["directory_sync"] != 3 || counts["device_sync"] != 1 {
		t.Fatalf("confirmation steps: %v", counts)
	}
	// The same records are now remembered, so only their barriers run again.
	counts = syncCounts(fresh, func() {
		if err := fresh.ConfirmAll("p/a.json", "p/q/b.json", "p/q/c.json"); err != nil {
			t.Fatal(err)
		}
	})
	if counts["data_sync"] != 0 || counts["directory_sync"] != 3 || counts["device_sync"] != 1 {
		t.Fatalf("remembered confirmation steps: %v", counts)
	}
}

func TestConfirmAllFailsForAMissingRecord(t *testing.T) {
	f := roundFiles(t)
	publishRecord(t, f, "present.json")
	if err := f.ConfirmAll("present.json", "absent.json"); err == nil {
		t.Fatal("confirmed a record that does not exist")
	}
}
