package execution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestStrictPublicationAndLock(t *testing.T) {
	f, err := OpenFiles(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Ensure("execution/requests"); err != nil {
		t.Fatal(err)
	}
	if stage, err := f.Publish("execution/requests/test", map[string]int{"n": 1}, true); err != nil || stage != Durable {
		t.Fatalf("publish %s: %v", stage, err)
	}
	var got map[string]int
	if err := f.Read("execution/requests/test", &got); err != nil || got["n"] != 1 {
		t.Fatalf("read %v: %v", got, err)
	}
	fence, err := f.Lock(context.Background(), "execution/lock")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fence.Close() }()
}
func TestStrictRecordsRejectLinksAndOversize(t *testing.T) {
	root := t.TempDir()
	f, err := OpenFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(root, "file"), filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := f.Read("file", new(any)); err == nil {
		t.Fatal("accepted hard link")
	}
	if err := f.Confirm("file"); err == nil {
		t.Fatal("confirmed hard link")
	}
	if _, err := f.Publish("file", struct{}{}, true); err == nil {
		t.Fatal("exclusively published over hard link")
	}
	if _, err := f.Publish("file", struct{}{}, false); err == nil {
		t.Fatal("replaced hard link")
	}
}

func TestVisiblePublicationRequiresActualBarrierRepair(t *testing.T) {
	for _, failed := range []string{"data_sync", "publish", "directory_sync", "device_sync", "close"} {
		t.Run(failed, func(t *testing.T) { testBarrierFailure(t, failed, false) })
	}
}
func TestExclusivePublicationRequiresActualBarrierRepair(t *testing.T) {
	for _, failed := range []string{"data_sync", "publish", "directory_sync", "device_sync", "close"} {
		t.Run(failed, func(t *testing.T) { testBarrierFailure(t, failed, true) })
	}
}
func testBarrierFailure(t *testing.T, failed string, exclusive bool) {
	f, err := OpenFiles(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Ensure("execution"); err != nil {
		t.Fatal(err)
	}
	var order []string
	f.fault = func(operation string) error {
		order = append(order, operation)
		if operation == failed {
			return errors.New("injected " + failed)
		}
		return nil
	}
	stage, err := f.Publish("execution/record", map[string]int{"n": 7}, exclusive)
	if err == nil || stage == Durable {
		t.Fatalf("barrier %s acknowledged: %s %v", failed, stage, err)
	}
	if failed == "data_sync" || failed == "publish" {
		if stage != BeforePublish {
			t.Fatal(stage)
		}
		return
	}
	if stage != Unconfirmed {
		t.Fatal(stage)
	}
	var got map[string]int
	savedFault := f.fault
	f.fault = nil
	if err := f.Read("execution/record", &got); err != nil || got["n"] != 7 {
		t.Fatalf("wanted visible bytes: %v %v", got, err)
	}
	f.fault = savedFault
	if err := f.Confirm("execution/record"); err == nil {
		t.Fatal("matching bytes skipped failed barrier")
	}
	order = nil
	f.fault = func(operation string) error { order = append(order, operation); return nil }
	if err := f.Confirm("execution/record"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(order, []string{"data_sync", "directory_sync", "directory_sync", "device_sync", "close"}) {
		t.Fatalf("repair order: %v", order)
	}
}
