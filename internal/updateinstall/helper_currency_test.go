package updateinstall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/release"
)

func TestUpgradeHelperPromotesAcrossDatabaseStateVersions(t *testing.T) {
	f := newHelperUpgradeFixture(t, "systemd", "v0.1.170")
	binary, err := os.ReadFile(f.prior.Executable)
	if err != nil {
		t.Fatal(err)
	}
	f.replacePrior(t, []byte(strings.Replace(string(binary), `"stateVersion":7`, `"stateVersion":4`, 1)))
	f.commit(t, "v0.1.171", "")
	got, err := UpgradeHelper(t.Context(), f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest == f.prior.Digest {
		t.Fatal("helper remained on the old database state version")
	}
}

func currentHelperManifest(t *testing.T, f helperUpgradeFixture) release.Manifest {
	t.Helper()
	status, err := Read(f.cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	return status.Request.Manifest
}

func fixtureHelperProbe(ctx context.Context, installed HelperInstallation) (int, error) {
	if err := release.VerifyExecutable(ctx, installed.Executable, installed.Digest); err != nil {
		return 0, fmt.Errorf("verify fixture helper image: %w", err)
	}
	return 123, nil
}

func TestPromoteHelperCommittedSideImageAndNewerPreservation(t *testing.T) {
	f := newHelperUpgradeFixture(t, "systemd", "v0.1.170")
	daemonManifest := currentHelperManifest(t, f)
	daemon := f.cfg.Executable
	f.cfg.Executable = filepath.Join(t.TempDir(), "side-image")
	f.commit(t, "v0.1.180", "")
	source := currentHelperManifest(t, f)
	f.publish(t, release.Build{Version: daemonManifest.Version, Commit: daemonManifest.Commit,
		Digest: f.prior.Digest, StateVersion: 7, WorkerProtocol: 1})
	f.cfg.Probe = fixtureHelperProbe
	got, err := PromoteHelper(t.Context(), f.cfg, source, source.Artifacts[0].BinarySHA256)
	if err != nil || got.Digest == f.prior.Digest {
		t.Fatalf("promote side executable = %+v, %v", got, err)
	}
	f.cfg.Executable = daemon
	retained, err := EnsureCurrentHelper(t.Context(), f.cfg, daemonManifest)
	if err != nil || retained != got {
		t.Fatalf("newer adoption helper was replaced = %+v, %v", retained, err)
	}
	if _, err = PromoteHelper(t.Context(), f.cfg, daemonManifest, f.prior.Digest); err == nil {
		t.Fatal("explicit helper downgrade was accepted")
	}
}

func TestEnsureCurrentHelperStartsStoppedService(t *testing.T) {
	f := newHelperUpgradeFixture(t, "systemd", "v0.1.170")
	checks := 0
	f.cfg.Probe = func(ctx context.Context, installed HelperInstallation) (int, error) {
		checks++
		if _, err := os.Stat(f.commands); err != nil {
			return 0, errors.New("helper is stopped")
		}
		return fixtureHelperProbe(ctx, installed)
	}
	got, err := EnsureCurrentHelper(t.Context(), f.cfg, currentHelperManifest(t, f))
	if err != nil || got != f.prior || checks < 3 {
		t.Fatalf("stopped helper = %+v, %v, probes %d", got, err, checks)
	}
}

func TestEnsureCurrentHelperFinishesPendingSameImage(t *testing.T) {
	f := newHelperUpgradeFixture(t, "systemd", "v0.1.170")
	if err := atomicWrite(helperActivationRecord(f.cfg.StateDir), []byte(f.prior.Digest), 0600); err != nil {
		t.Fatal(err)
	}
	f.cfg.Probe = fixtureHelperProbe
	if _, err := EnsureCurrentHelper(t.Context(), f.cfg, currentHelperManifest(t, f)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(helperActivationRecord(f.cfg.StateDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("settled image still has pending activation = %v", err)
	}
}

func TestPromoteHelperRestoresFailedStart(t *testing.T) {
	f := newHelperUpgradeFixture(t, "systemd", "v0.1.170")
	f.commit(t, "v0.1.171", "")
	before := f.snapshot(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.cfg.Probe = func(ctx context.Context, installed HelperInstallation) (int, error) {
		if installed.Digest != f.prior.Digest {
			cancel()
			return 0, errors.New("candidate process has the wrong mapped image")
		}
		return fixtureHelperProbe(ctx, installed)
	}
	source := currentHelperManifest(t, f)
	_, err := PromoteHelper(ctx, f.cfg, source, source.Artifacts[0].BinarySHA256)
	if err == nil || !strings.Contains(err.Error(), "wrong mapped image") {
		t.Fatalf("unverified process accepted = %v", err)
	}
	for path, data := range f.snapshot(t) {
		if data != before[path] {
			t.Fatalf("failed startup did not restore %s", path)
		}
	}
	if _, err = os.Stat(helperPromotionPath(f.cfg.StateDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restoration left a pending promotion = %v", err)
	}
}

func TestPromoteHelperReportsRestorationFailureAndRetries(t *testing.T) {
	f := newHelperUpgradeFixture(t, "systemd", "v0.1.170")
	f.commit(t, "v0.1.171", "")
	f.cfg.Probe = fixtureHelperProbe
	tool, err := exec.LookPath("systemctl")
	if err != nil {
		t.Fatal(err)
	}
	if err = atomicWrite(tool, []byte("#!/bin/sh\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	source := currentHelperManifest(t, f)
	digest := source.Artifacts[0].BinarySHA256
	_, err = PromoteHelper(t.Context(), f.cfg, source, digest)
	if err == nil || !strings.Contains(err.Error(), "restore prior helper after promotion") {
		t.Fatalf("restoration failure was omitted = %v", err)
	}
	if err = atomicWrite(tool, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	got, err := PromoteHelper(t.Context(), f.cfg, source, digest)
	if err != nil || got.Digest != digest {
		t.Fatalf("retry after restored receipt = %+v, %v", got, err)
	}
}

func TestPromoteHelperRejectsChangingProcess(t *testing.T) {
	f := newHelperUpgradeFixture(t, "systemd", "v0.1.170")
	f.commit(t, "v0.1.171", "")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	checks := 0
	f.cfg.Probe = func(ctx context.Context, installed HelperInstallation) (int, error) {
		if installed == f.prior {
			return fixtureHelperProbe(ctx, installed)
		}
		checks++
		if checks == 2 {
			cancel()
		}
		return checks, nil
	}
	source := currentHelperManifest(t, f)
	_, err := PromoteHelper(ctx, f.cfg, source, source.Artifacts[0].BinarySHA256)
	if err == nil || !strings.Contains(err.Error(), "process changed before readiness") {
		t.Fatalf("unstable process reported ready = %v", err)
	}
}

func TestPromoteHelperResumesInterruptedPublication(t *testing.T) {
	for _, boundary := range []string{"before-prepare", "receipt", "launcher", "running"} {
		t.Run(boundary, func(t *testing.T) { checkInterruptedHelperPromotion(t, boundary) })
	}
}

func checkInterruptedHelperPromotion(t *testing.T, boundary string) {
	t.Helper()
	f := newHelperUpgradeFixture(t, "systemd", "v0.1.170")
	f.commit(t, "v0.1.171", "")
	prior, err := recoveryHelperSnapshot(t.Context(), f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	source := currentHelperManifest(t, f)
	digest := source.Artifacts[0].BinarySHA256
	record, err := json.Marshal(helperPromotion{Prior: prior.installation, Receipt: prior.receipt, Service: prior.service, Target: digest})
	if err != nil {
		t.Fatal(err)
	}
	if err = atomicWrite(helperPromotionPath(f.cfg.StateDir), record, 0600); err != nil {
		t.Fatal(err)
	}
	if boundary != "before-prepare" {
		if _, err = prepareHelper(t.Context(), f.cfg, true, digest); err != nil {
			t.Fatal(err)
		}
	}
	if boundary == "receipt" {
		if err = replaceHelperLink(filepath.Join(transactionDir(f.cfg.StateDir), "helper", "current"), f.prior.Executable); err != nil {
			t.Fatal(err)
		}
	}
	if boundary == "running" {
		if err = os.Remove(helperActivationRecord(f.cfg.StateDir)); err != nil {
			t.Fatal(err)
		}
	}
	f.cfg.Probe = fixtureHelperProbe
	got, err := PromoteHelper(t.Context(), f.cfg, source, digest)
	if err != nil || got.Digest != digest {
		t.Fatalf("interrupted promotion = %+v, %v", got, err)
	}
	again, err := PromoteHelper(t.Context(), f.cfg, source, digest)
	if err != nil || again != got {
		t.Fatalf("repeat promotion = %+v, %v", again, err)
	}
}

func TestPromoteHelperRequiresFinishedJournalAndVerifiedImage(t *testing.T) {
	for _, problem := range []string{"active", "rollback-failed", "digest", "source", "decoder", "probe", "settings"} {
		t.Run(problem, func(t *testing.T) { checkHelperPromotionRefusal(t, problem) })
	}
}

func checkHelperPromotionRefusal(t *testing.T, problem string) {
	t.Helper()
	f := newHelperUpgradeFixture(t, "systemd", "v0.1.170")
	f.commit(t, "v0.1.171", "")
	f.cfg.Probe = fixtureHelperProbe
	source := currentHelperManifest(t, f)
	digest := source.Artifacts[0].BinarySHA256
	switch problem {
	case "active", "rollback-failed", "settings":
		changeHelperJournal(t, f, problem)
	case "digest":
		digest = strings.Repeat("0", 64)
	case "source":
		f.cfg.Executable = f.prior.Executable
	case "decoder":
		data, err := os.ReadFile(f.cfg.Executable)
		if err != nil {
			t.Fatal(err)
		}
		data = []byte(strings.Replace(string(data), "  exit 0\nfi", "  exit 1\nfi", 1))
		if err = atomicWrite(f.cfg.Executable, data, 0755); err != nil {
			t.Fatal(err)
		}
		digest = digestBytes(data)
		for i := range source.Artifacts {
			source.Artifacts[i].BinarySHA256 = digest
		}
	case "probe":
		f.cfg.Probe = nil
	}
	before := f.snapshot(t)
	if _, err := PromoteHelper(t.Context(), f.cfg, source, digest); err == nil {
		t.Fatal("invalid helper promotion accepted")
	}
	f.assertUnchanged(t, before)
}

func changeHelperJournal(t *testing.T, f helperUpgradeFixture, problem string) {
	t.Helper()
	status, err := Read(f.cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	status.Phase = Granted
	if problem == "rollback-failed" {
		status.Phase = RollbackFailed
	}
	if problem == "settings" {
		status.Phase, status.Settings.ClientOnly = Committed, false
		status.Settings.Service.Kind = "launchd"
	}
	data, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if err = atomicWrite(journalPath(f.cfg.StateDir), data, 0600); err != nil {
		t.Fatal(err)
	}
}
