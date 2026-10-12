package updateinstall

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestHelperUpdatesRetainCurrentAndOnePriorImage(t *testing.T) {
	f := newHelperUpgradeFixture(t, "systemd", "v0.1.170")
	installed := f.prior
	for i := range 12 {
		f.commit(t, fmt.Sprintf("v0.1.%d", 171+i), "")
		setPruneSource(t, f, installed.Digest)
		var err error
		installed, err = UpgradeHelper(t.Context(), f.cfg)
		if err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(transactionDir(f.cfg.StateDir), "helper"))
	if err != nil {
		t.Fatal(err)
	}
	images := 0
	for _, entry := range entries {
		if entry.IsDir() && helperDigestPattern.MatchString(entry.Name()) {
			images++
		}
	}
	if images != 3 {
		t.Fatalf("12 updates retained %d helper images, want current, previous promotion and one spare", images)
	}
	if err := verifyFile(installed.Executable, installed.Digest); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareExistingHelperDoesNotStageUnusedImages(t *testing.T) {
	f := newHelperUpgradeFixture(t, "systemd", "v0.1.170")
	f.commit(t, "v0.1.171", "")
	digest, err := fileDigest(f.cfg.Executable)
	if err != nil {
		t.Fatal(err)
	}
	installed, err := PrepareHelper(t.Context(), f.cfg)
	if err != nil || installed != f.prior {
		t.Fatalf("existing helper changed: %+v, %v", installed, err)
	}
	assertExists(t, filepath.Join(transactionDir(f.cfg.StateDir), "helper", digest), false)
}

func TestHelperPruneKeepsPendingActivationAndInstallationSource(t *testing.T) {
	f := newHelperUpgradeFixture(t, "systemd", "v0.1.170")
	f.commit(t, "v0.1.171", "")
	pending, err := stageHelperImage(t.Context(), f.cfg, digestBytes(mustReadImage(t, f.cfg.Executable)))
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(helperActivationRecord(f.cfg.StateDir), []byte(pending.Digest), 0600); err != nil {
		t.Fatal(err)
	}
	f.commit(t, "v0.1.172", "")
	setPruneSource(t, f, f.prior.Digest)
	if _, err := stageHelperImage(t.Context(), f.cfg, digestBytes(mustReadImage(t, f.cfg.Executable))); err != nil {
		t.Fatal(err)
	}
	if err := pruneHelperImages(f.cfg.StateDir, f.prior.Digest); err != nil {
		t.Fatal(err)
	}
	assertExists(t, pending.Executable, true)
	assertExists(t, f.prior.Executable, true)
}

func setPruneSource(t *testing.T, f helperUpgradeFixture, digest string) {
	t.Helper()
	status, err := Read(f.cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	status.Request.Current.Digest = digest
	data, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(journalPath(f.cfg.StateDir), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestHelperPruneRejectsIncompleteInstallation(t *testing.T) {
	f := newHelperUpgradeFixture(t, "systemd", "v0.1.170")
	if err := atomicWrite(journalPath(f.cfg.StateDir), []byte(`{"schema":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := pruneHelperImages(f.cfg.StateDir, f.prior.Digest); err == nil {
		t.Fatal("incomplete installation metadata permitted helper cleanup")
	}
	assertExists(t, f.prior.Executable, true)
}

func mustReadImage(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // owned fixture image
	if err != nil {
		t.Fatal(err)
	}
	return data
}
