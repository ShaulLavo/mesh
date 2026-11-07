package execution

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestExecutionReviewRetirementRetryMissingReference(t *testing.T) {
	j, k, i, env := journalFixture(t)
	r, _, err := reserveTest(t, j, k, i, env)
	if err != nil {
		t.Fatal(err)
	}
	r, err = j.NotStarted(t.Context(), r.Binding, "no_child")
	if err != nil {
		t.Fatal(err)
	}
	if err := j.prepareRetirement(t.Context(), r.Binding); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(j.files.root.Name(), "s", r.Binding.Reservation.ID)
	if err := os.RemoveAll(name); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(name, 0700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(name, "new-allocation")
	if err := os.WriteFile(sentinel, []byte("not owned by retired attempt"), 0600); err != nil {
		t.Fatal(err)
	}
	err = j.Maintain(t.Context())
	_, readErr := os.Stat(sentinel)
	t.Logf("maintenance error=%v; replacement sentinel=%v", err, readErr)
	if errors.Is(readErr, os.ErrNotExist) {
		t.Fatal("pending retirement deleted an existing directory without its exact reference")
	}
}

func TestExecutionReviewUnknownExecutionOrigin(t *testing.T) {
	j, k, i, env := journalFixture(t)
	r, _, err := reserveTest(t, j, k, i, env)
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(j.files.root.Name(), "s", r.Binding.Reservation.ID)
	if err := os.Remove(filepath.Join(name, ".reservation")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(name, "meta.json"), []byte("torn"), 0600); err != nil {
		t.Fatal(err)
	}
	protected := ProtectedSession(filepath.Dir(name), r.Binding.Reservation.ID)
	t.Logf("valid execution .launching and journal binding remain; protected=%v", protected)
	if !protected {
		t.Fatal("missing reference plus unreadable metadata became permission despite execution launch marker")
	}
}

func TestExecutionOriginRequiresPositiveEvidence(t *testing.T) {
	for _, test := range []struct {
		name, marker, meta string
		protected          bool
	}{
		{name: "missing origin", protected: true},
		{name: "torn metadata", meta: "torn", protected: true},
		{name: "unclassified metadata", meta: `{}`, protected: true},
		{name: "null origin", meta: `{"reservation":null}`, protected: true},
		{name: "unknown purpose", meta: `{"reservation":{"id":"7K3D","instance":"0123456789abcdef0123456789abcdef","purpose":"unknown"}}`, protected: true},
		{name: "wrong ID", meta: `{"reservation":{"id":"91AZ","instance":"0123456789abcdef0123456789abcdef","purpose":"generic"}}`, protected: true},
		{name: "damaged marker", marker: "torn", meta: genericOriginMetadata(), protected: true},
		{name: "positive generic metadata", meta: genericOriginMetadata()},
	} {
		t.Run(test.name, func(t *testing.T) { checkExecutionOrigin(t, test.marker, test.meta, test.protected) })
	}
}

func genericOriginMetadata() string {
	return `{"reservation":{"id":"7K3D","instance":"0123456789abcdef0123456789abcdef","purpose":"generic"}}`
}

func checkExecutionOrigin(t *testing.T, marker, meta string, protected bool) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "7K3D")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{".reservation": marker, "meta.json": meta} {
		if data == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if got := ProtectedSession(root, "7K3D"); got != protected {
		t.Fatalf("protected = %t, want %t", got, protected)
	}
}
