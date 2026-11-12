package updateinstall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

type helperDispositionFixture struct {
	helperUpgradeFixture
	active string
	failed string
}

func newHelperDispositionFixture(t *testing.T, running bool) helperDispositionFixture {
	t.Helper()
	f := helperDispositionFixture{helperUpgradeFixture: newHelperUpgradeFixture(t, promotionSystemd, "v0.1.170")}
	f.active, f.failed = filepath.Join(filepath.Dir(f.commands), "active"), filepath.Join(filepath.Dir(f.commands), "failed")
	tool, err := exec.LookPath("systemctl")
	if err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(transactionDir(f.cfg.StateDir), "helper", "current")
	script := fmt.Sprintf("#!/bin/sh\nset -eu\nprintf '%%s\\n' \"$*\" >> %q\ncase \"$2\" in\nrestart) rm -f %q; image=$(readlink %q); if test \"$image\" != %q; then touch %q; exit 19; fi; printf '%%s' \"$image\" > %q ;;\nstop) rm -f %q ;;\nesac\n", f.commands, f.active, launcher, f.prior.Executable, f.failed, f.active, f.active)
	if err := atomicWrite(tool, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if running {
		if err := atomicWrite(f.active, []byte(f.prior.Executable), 0600); err != nil {
			t.Fatal(err)
		}
	}
	f.cfg.State = func(context.Context) (HelperProcessState, error) {
		_, err := os.Stat(f.active)
		if errors.Is(err, os.ErrNotExist) {
			return HelperProcessState{Loaded: true}, nil
		}
		if err != nil {
			return HelperProcessState{}, fmt.Errorf("inspect fixture process state: %w", err)
		}
		return HelperProcessState{Loaded: true, PID: 123}, nil
	}
	f.cfg.Probe = func(ctx context.Context, installed HelperInstallation) (int, error) {
		root, err := os.OpenRoot(filepath.Dir(f.active))
		if err != nil {
			return 0, fmt.Errorf("open fixture process state: %w", err)
		}
		defer func() { _ = root.Close() }()
		active, err := root.ReadFile(filepath.Base(f.active))
		if err != nil || string(active) != installed.Executable {
			return 0, errors.Join(errors.New("fixture image is not running"), err)
		}
		return fixtureHelperProbe(ctx, installed)
	}
	f.commit(t, "v0.1.171", "")
	return f
}

func TestPromoteHelperFailurePreservesEntryDisposition(t *testing.T) {
	for _, running := range []bool{false, true} {
		t.Run(strconv.FormatBool(running), func(t *testing.T) { testHelperFailureDisposition(t, running) })
	}
}

func testHelperFailureDisposition(t *testing.T, running bool) {
	f := newHelperDispositionFixture(t, running)
	before := f.snapshot(t)
	source := currentHelperManifest(t, f.helperUpgradeFixture)
	if _, err := PromoteHelper(t.Context(), f.cfg, source, source.Artifacts[0].BinarySHA256); err == nil || !strings.Contains(err.Error(), "19") {
		t.Fatalf("expected failed candidate promotion: %v", err)
	}
	f.assertRestoredBytes(t, before)
	state, err := f.cfg.State(t.Context())
	if err != nil || (state.PID > 0) != running {
		t.Fatalf("entry disposition changed: %+v %v", state, err)
	}
	if _, err := os.Stat(helperPromotionPath(f.cfg.StateDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("successful disposition restoration retained its checkpoint")
	}
}

func TestPromoteHelperInterruptedRestoreRetainsStoppedDisposition(t *testing.T) {
	f := newHelperDispositionFixture(t, false)
	before := f.snapshot(t)
	originalState := f.cfg.State
	faulted := false
	f.cfg.State = func(ctx context.Context) (HelperProcessState, error) {
		if !faulted && helperDispositionRestorePublished(ctx, f) {
			faulted = true
			return HelperProcessState{}, errors.New("interrupted restored-state verification")
		}
		return originalState(ctx)
	}
	source := currentHelperManifest(t, f.helperUpgradeFixture)
	if _, err := PromoteHelper(t.Context(), f.cfg, source, source.Artifacts[0].BinarySHA256); err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("missing interrupted restoration: %v", err)
	}
	data, err := readMetadata(t.Context(), f.cfg.StateDir, filepath.Join("update", "helper", "promotion.json"), true)
	if err != nil {
		t.Fatal(err)
	}
	var checkpoint helperPromotion
	if err := json.Unmarshal(data, &checkpoint); err != nil {
		t.Fatal(err)
	}
	if !checkpoint.Restoring || checkpoint.Disposition != helperStopped {
		t.Fatal("restoration checkpoint lost stopped entry disposition")
	}
	f.cfg.State = originalState
	if err := resumeHelperPromotion(t.Context(), f.cfg); err != nil {
		t.Fatal(err)
	}
	f.assertRestoredBytes(t, before)
	state, err := f.cfg.State(t.Context())
	if err != nil || state.PID != 0 {
		t.Fatal("interrupted restoration started the stopped prior helper")
	}
	if _, err := os.Stat(helperPromotionPath(f.cfg.StateDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("resumed restoration retained its checkpoint")
	}
}

func helperDispositionRestorePublished(ctx context.Context, f helperDispositionFixture) bool {
	if _, err := os.Stat(f.failed); err != nil {
		return false
	}
	var installed HelperInstallation
	return readHelperInstallation(ctx, f.cfg.StateDir, &installed) == nil && installed == f.prior
}

func (f helperDispositionFixture) assertRestoredBytes(t *testing.T, expected map[string]string) {
	t.Helper()
	for path, actual := range f.snapshot(t) {
		if actual != expected[path] {
			t.Fatalf("helper recovery changed retained bytes or link at %s", filepath.Base(path))
		}
	}
}
