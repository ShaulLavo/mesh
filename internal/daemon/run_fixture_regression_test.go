package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const daemonFixtureProbe = "MESH_DAEMON_FIXTURE_PROBE"

func TestDaemonRunFixtureCleanup(t *testing.T) {
	for _, scenario := range []string{"assertion", "readiness-timeout", "early-error", "early-nil", "join-timeout", "success"} {
		t.Run(scenario, func(t *testing.T) { checkDaemonFixtureProbe(t, scenario) })
	}
}

func checkDaemonFixtureProbe(t *testing.T, scenario string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDaemonRunFixtureProbe$", "-test.v", "-test.timeout=12s") //nolint:gosec // executes this test binary with a fixed probe selection
	cmd.Env = append(os.Environ(), daemonFixtureProbe+"="+scenario)
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if scenario == "success" && err != nil {
		t.Fatalf("successful fixture: %v\n%s", err, output)
	}
	if scenario != "success" && (!errors.As(err, &exitErr) || exitErr.ExitCode() != 1) {
		t.Fatalf("failure control exit = %v, want 1\n%s", err, output)
	}
	if strings.Contains(string(output), "fixture environment ") {
		t.Fatalf("environment cleanup regression for %s\n%s", scenario, output)
	}
	if !strings.Contains(string(output), "probe verified "+scenario) {
		t.Fatalf("cleanup regression for %s\n%s", scenario, output)
	}
	if scenario == "early-error" && !strings.Contains(string(output), "run returned before readiness: controlled startup error") {
		t.Fatalf("startup error was lost\n%s", output)
	}
	if scenario == "early-nil" && !strings.Contains(string(output), "run returned before readiness: <nil>") {
		t.Fatalf("early successful return was mistaken for readiness\n%s", output)
	}
	t.Logf("%s", output)
}

func TestDaemonRunFixtureProbe(t *testing.T) {
	scenario := os.Getenv(daemonFixtureProbe)
	if scenario == "" {
		t.Skip("subprocess failure control")
	}
	release := make(chan struct{})
	started := make(chan context.Context, 1)
	var fixture *daemonRunFixture
	var runCtx context.Context
	t.Cleanup(func() { checkFakeDaemonCleanup(runCtx, t, scenario, fixture, release) })
	fixture = newDaemonRunFixture(t)
	fixture.start(t, func(ctx context.Context) error {
		started <- ctx
		return runFiniteDaemonFake(ctx, fixture.root, scenario, release)
	})
	checkDaemonFixtureEnvironment(t, fixture, true)
	runCtx = <-started
	if scenario == "early-error" || scenario == "early-nil" {
		<-fixture.done
	}
	if strings.HasPrefix(scenario, "early-") || scenario == "readiness-timeout" {
		fixture.dial(t)
		t.Fatal("missing socket unexpectedly ready")
	}
	if scenario != "success" {
		t.Fatal("controlled assertion failure")
	}
}

func runFiniteDaemonFake(ctx context.Context, root, scenario string, release <-chan struct{}) error {
	if scenario == "early-error" {
		return errors.New("controlled startup error")
	}
	if scenario == "early-nil" {
		return nil
	}
	if scenario == "join-timeout" {
		<-release
		return nil
	}
	select {
	case <-ctx.Done():
	case <-release:
	}
	if _, err := os.Stat(filepath.Join(root, "state")); err != nil {
		return fmt.Errorf("private state removed before fake returned: %w", err)
	}
	return nil
}

func checkFakeDaemonCleanup(ctx context.Context, t *testing.T, scenario string, f *daemonRunFixture, release chan struct{}) {
	t.Helper()
	defer drainDaemonFake(t, f, release)
	checkDaemonFixtureEnvironment(t, f, scenario == "join-timeout")
	if f.joined && scenario != "early-error" && f.err != nil {
		t.Errorf("fake returned an unexpected cleanup error: %v", f.err)
		return
	}
	if ctx.Err() != context.Canceled {
		t.Errorf("fake was not canceled before directory cleanup")
		return
	}
	_, err := os.Stat(f.root)
	if scenario == "join-timeout" {
		if err != nil || f.joined {
			t.Errorf("unknown join lost private state: stat=%v joined=%v", err, f.joined)
			return
		}
		t.Log("probe verified " + scenario)
		return
	}
	if !f.joined || !errors.Is(err, os.ErrNotExist) {
		t.Errorf("cleanup did not join before removal: joined=%v stat=%v", f.joined, err)
		return
	}
	t.Log("probe verified " + scenario)
}

func drainDaemonFake(t *testing.T, f *daemonRunFixture, release chan struct{}) {
	t.Helper()
	close(release)
	select {
	case <-f.done:
	case <-time.After(runtimeTestTimeout):
		t.Fatal("finite fake did not finish after release")
	}
	if err := os.RemoveAll(f.root); err != nil { //nolint:gosec // probe owns the fixture root after joining its finite fake
		t.Error(err)
	}
}

func checkDaemonFixtureEnvironment(t *testing.T, f *daemonRunFixture, retained bool) {
	t.Helper()
	for key, previous := range f.env {
		want, wantSet := previous.value, previous.exists
		if retained {
			want, wantSet = previous.private, true
		}
		value, exists := os.LookupEnv(key)
		if value != want || exists != wantSet {
			t.Errorf("fixture environment %s = %q, set %v; want %q, set %v", key, value, exists, want, wantSet)
		}
	}
}
