package updateinstall

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestUpgradeHelperRetriesFailedActivation(t *testing.T) {
	for _, step := range []string{"daemon-reload", "restart", "kickstart"} {
		t.Run(step, func(t *testing.T) {
			checkHelperActivationRetry(t, step)
		})
	}
}

func checkHelperActivationRetry(t *testing.T, step string) {
	t.Helper()
	kind := "systemd"
	if step == "kickstart" {
		kind = "launchd"
	}
	f := newHelperUpgradeFixture(t, kind, "v0.1.170")
	f.commit(t, "v0.1.171", "")
	tool := "systemctl"
	if kind == "launchd" {
		tool = "launchctl"
	}
	command, err := exec.LookPath(tool)
	if err != nil {
		t.Fatal(err)
	}
	failure := "#!/bin/sh\ncase \"$*\" in *" + step + "*) exit 1 ;; esac\n"
	if err = os.WriteFile(command, []byte(failure), 0755); err != nil { //nolint:gosec // isolated service manager fixture
		t.Fatal(err)
	}
	if _, err = UpgradeHelper(t.Context(), f.cfg); err == nil {
		t.Fatal("fixture activation unexpectedly succeeded")
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$MESH_HELPER_TEST_COMMANDS\"\n"
	if err = os.WriteFile(command, []byte(script), 0755); err != nil { //nolint:gosec // isolated service manager fixture
		t.Fatal(err)
	}
	if _, err = UpgradeHelper(t.Context(), f.cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(f.commands)
	if err != nil {
		t.Fatalf("failed activation was never retried: %v", err)
	}
	if !strings.Contains(string(data), "restart") && !strings.Contains(string(data), "kickstart") {
		t.Fatalf("retry did not activate the helper: %q", data)
	}
	if _, err = UpgradeHelper(t.Context(), f.cfg); err != nil {
		t.Fatal(err)
	}
	again, err := os.ReadFile(f.commands)
	if err != nil || string(again) != string(data) {
		t.Fatalf("settled activation ran again: %q, %v", again, err)
	}
}

// The supervised helper calls UpgradeHelper on itself. Its own restart request
// terminates it before it can clear the activation record, so the replacement
// must recognize itself as the settled activation instead of restarting again.
func TestUpgradeHelperSelfRestartSettlesInReplacement(t *testing.T) {
	for _, kind := range []string{"systemd", "launchd"} {
		t.Run(kind, func(t *testing.T) {
			checkHelperSelfRestart(t, kind)
		})
	}
}

func checkHelperSelfRestart(t *testing.T, kind string) {
	t.Helper()
	f := newHelperUpgradeFixture(t, kind, "v0.1.170")
	f.commit(t, "v0.1.171", "")
	running := f.prior.Digest
	f.cfg.Probe = func(ctx context.Context, installed HelperInstallation) (int, error) {
		if installed.Digest != running {
			return 0, errors.New("helper service is executing another image")
		}
		return fixtureHelperProbe(ctx, installed)
	}
	tool := "systemctl"
	if kind == "launchd" {
		tool = "launchctl"
	}
	command, err := exec.LookPath(tool)
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := os.ReadFile(command) //nolint:gosec // isolated service manager fixture
	if err != nil {
		t.Fatal(err)
	}
	terminated := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$MESH_HELPER_TEST_COMMANDS\"\ncase \"$*\" in *restart*|*kickstart*) exit 143 ;; esac\n"
	if err = os.WriteFile(command, []byte(terminated), 0755); err != nil { //nolint:gosec // isolated service manager fixture
		t.Fatal(err)
	}
	if _, err = UpgradeHelper(t.Context(), f.cfg); err == nil {
		t.Fatal("terminated self-restart reported completion")
	}
	pending, err := os.ReadFile(helperActivationRecord(f.cfg.StateDir))
	if err != nil {
		t.Fatalf("terminated helper lost its activation checkpoint: %v", err)
	}
	if err = os.WriteFile(command, recorder, 0755); err != nil { //nolint:gosec // isolated service manager fixture
		t.Fatal(err)
	}
	if err = os.Remove(f.commands); err != nil {
		t.Fatal(err)
	}
	// The manager's restart started the replacement on the promoted image.
	running = string(pending)
	for range 2 {
		got, err := UpgradeHelper(t.Context(), f.cfg)
		if err != nil || got.Digest != running {
			t.Fatalf("replacement helper = %+v, %v", got, err)
		}
		if data, err := os.ReadFile(f.commands); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("settled replacement restarted itself: %q, %v", data, err)
		}
	}
	if _, err = os.Stat(helperActivationRecord(f.cfg.StateDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("settled replacement kept its activation checkpoint: %v", err)
	}
}

// Without a process executing the promoted image, the checkpoint still drives
// a restart rather than being cleared by observation.
func TestUpgradeHelperRestartsWhenPromotedImageIsNotRunning(t *testing.T) {
	f := newHelperUpgradeFixture(t, "systemd", "v0.1.170")
	f.commit(t, "v0.1.171", "")
	candidate, err := prepareHelper(t.Context(), f.cfg, true, "")
	if err != nil {
		t.Fatal(err)
	}
	f.cfg.Probe = func(ctx context.Context, installed HelperInstallation) (int, error) {
		if installed.Digest == candidate.Digest {
			return 0, errors.New("helper service is executing another image")
		}
		return fixtureHelperProbe(ctx, installed)
	}
	if _, err = UpgradeHelper(t.Context(), f.cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(f.commands)
	if err != nil || !strings.Contains(string(data), "restart") {
		t.Fatalf("unsettled activation did not restart the helper: %q, %v", data, err)
	}
}

func TestUpgradeHelperResumesActivationAfterPreparedPromotion(t *testing.T) {
	for _, kind := range []string{"systemd", "launchd"} {
		t.Run(kind, func(t *testing.T) {
			checkPreparedHelperActivation(t, kind)
		})
	}
}

func checkPreparedHelperActivation(t *testing.T, kind string) {
	t.Helper()
	f := newHelperUpgradeFixture(t, kind, "v0.1.170")
	f.commit(t, "v0.1.171", "")
	candidate, err := prepareHelper(t.Context(), f.cfg, true, "")
	if err != nil {
		t.Fatal(err)
	}
	f.commit(t, "v0.1.150", "")
	got, err := UpgradeHelper(t.Context(), f.cfg)
	if err != nil || got != candidate {
		t.Fatalf("prepared image did not finish activation across bridge commit: %+v, %v", got, err)
	}
	data, err := os.ReadFile(f.commands)
	if err != nil || !strings.Contains(string(data), "mesh-update-helper") {
		t.Fatalf("prepared activation was skipped: %q, %v", data, err)
	}
}
