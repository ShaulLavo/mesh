package updateinstall

import (
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
