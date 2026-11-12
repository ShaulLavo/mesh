package execution

import (
	"errors"
	"io/fs"
	"os"
	"testing"
)

func TestLookupDuringResultPublication(t *testing.T) {
	j, k, intent, env := journalFixture(t)
	r, _, err := reserveTest(t, j, k, intent, env)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Dispatch(t.Context(), r.Binding, intent); err != nil {
		t.Fatal(err)
	}
	a, err := StartAttempt(t.Context(), j.files.root.Name(), r.Binding, intent, env)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	if _, err := j.Published(t.Context(), r.Binding); err != nil {
		t.Fatal(err)
	}
	code := 7
	result := Result{Binding: r.Binding, Termination: Termination{Kind: "exit", Code: &code}, Capture: captureNotRecorded}
	observed := false
	a.files.fault = func(operation string) error {
		if operation != "publication_visible" {
			return nil
		}
		observed = true
		got, err := j.Lookup(t.Context(), k)
		if err != nil || got.Phase != phaseTerminal || got.Result == nil {
			t.Errorf("lookup during result publication: phase=%s result=%+v err=%v", got.Phase, got.Result, err)
			return nil
		}
		if got.Result.Termination.Code == nil || *got.Result.Termination.Code != code {
			t.Errorf("lookup lost exit code: %+v", got.Result.Termination)
		}
		return nil
	}
	if err := a.Finish(result); err != nil {
		t.Fatal(err)
	}
	if !observed {
		t.Fatal("result publication was not observed")
	}
	if err := a.Finish(result); !errors.Is(err, os.ErrExist) {
		t.Fatalf("second result publication: %v", err)
	}
}

func TestExclusivePublicationPreservesExistingRecord(t *testing.T) {
	f, err := OpenFiles(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if stage, err := f.Publish("record", 7, true); err != nil || stage != Durable {
		t.Fatalf("first publication: %s %v", stage, err)
	}
	stage, err := f.Publish("record", 8, true)
	if stage != BeforePublish || !errors.Is(err, os.ErrExist) {
		t.Fatalf("exclusive conflict: %s %v", stage, err)
	}
	var got int
	if err := f.Read("record", &got); err != nil || got != 7 {
		t.Fatalf("existing record changed: %d %v", got, err)
	}
	entries, err := fs.ReadDir(f.root.FS(), ".")
	if err != nil || len(entries) != 1 || entries[0].Name() != "record" {
		t.Fatalf("publication left temporary names: %v %v", entries, err)
	}
}

func TestExclusivePublicationFailureAfterVisibility(t *testing.T) {
	f, err := OpenFiles(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	failure := errors.New("injected after visibility")
	f.fault = func(operation string) error {
		if operation == "publication_visible" {
			return failure
		}
		return nil
	}
	stage, err := f.Publish("record", 7, true)
	if stage != Unconfirmed || !errors.Is(err, failure) {
		t.Fatalf("visible failure: %s %v", stage, err)
	}
	f.fault = nil
	var got int
	if err := f.Read("record", &got); err != nil || got != 7 {
		t.Fatalf("lost visible bytes: %d %v", got, err)
	}
	if err := f.Confirm("record"); err != nil {
		t.Fatal(err)
	}
}
