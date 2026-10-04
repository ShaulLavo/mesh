package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/paths"
)

func TestDashboardConfigErrorAndSameInvocationRecovery(t *testing.T) {
	fixture := setupCommandTestHost(t)
	state, err := paths.StateDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := identity.LoadOrCreate(state); err != nil {
		t.Fatal(err)
	}
	path, err := ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	valid, err := readClientConfigTestFile(path)
	if err != nil {
		t.Fatal(err)
	}
	invalid := []byte(`{"version":1,"hosts":[],"unexpected":true}`)
	if err := os.WriteFile(path, invalid, 0o600); err != nil { //nolint:gosec // ConfigPath resolves the owned test configuration directory

		t.Fatal(err)
	}
	views, failures := 0, 0
	_, _, err = executeCommand(t, Dependencies{Dashboard: func(ctx context.Context, input DashboardInput) error {
		views++
		if views == 2 {
			if input.ConfigError != nil || input.ConfigWatch != nil || input.Watch == nil || len(input.Hosts) != 2 {
				t.Fatal("valid configuration did not recover the real inventory")
			}
			for _, host := range input.Hosts {
				if host.ID == fixture.host.ID {
					return nil
				}
			}
			t.Fatal("recovery lost the configured stable host ID")
		}
		if views != 1 || input.ConfigError == nil || !strings.Contains(input.ConfigError.Error(), `unknown field "unexpected"`) || input.ConfigWatch == nil || input.Watch != nil || len(input.Hosts) != 0 {
			t.Fatal("configuration failure reached a healthy or fabricated dashboard")
		}
		unchanged, err := readClientConfigTestFile(path)
		if err != nil || !bytes.Equal(unchanged, invalid) {
			t.Fatal("dashboard changed invalid configuration")
		}
		run, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		return input.ConfigWatch(run, func(problem error) {
			failures++
			if !strings.Contains(problem.Error(), `unknown field "unexpected"`) {
				t.Fatal("retry lost the observable configuration failure")
			}
			unchanged, err := readClientConfigTestFile(path)
			if err != nil || !bytes.Equal(unchanged, invalid) {
				t.Fatal("retry healed invalid configuration")
			}
			if err := os.WriteFile(path, valid, 0o600); err != nil { //nolint:gosec // restore only the owned test configuration file

				t.Fatal(err)
			}
		})
	}}, "dashboard", "--wall")
	if err != nil || views != 2 || failures != 1 {
		t.Fatalf("same invocation did not retain one failure and recover: views=%d failures=%d err=%v", views, failures, err)
	}
}

func TestDashboardConfigRetryCancellation(t *testing.T) {
	path := writeClientConfigFixture(t, []byte(`{"version":1,"hosts":[],"unexpected":true}`))
	before, err := readClientConfigTestFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := watchDashboardConfig(ctx, "", func(error) { t.Fatal("cancelled retry published") }); !errors.Is(err, context.Canceled) {
		t.Fatalf("retry ignored cancellation: %v", err)
	}
	after, err := readClientConfigTestFile(path)
	if err != nil || !bytes.Equal(after, before) {
		t.Fatal("cancelled retry changed invalid configuration")
	}
}

func TestDashboardConfigViewExitDoesNotRestartRenderer(t *testing.T) {
	writeClientConfigFixture(t, []byte(`{"version":1,"hosts":[],"unexpected":true}`))
	views := 0
	stopped := errors.New("renderer restarted")
	_, _, err := executeCommand(t, Dependencies{Dashboard: func(context.Context, DashboardInput) error {
		views++
		if views > 1 {
			return stopped
		}
		return nil
	}}, "dashboard", "--wall")
	if err != nil || views != 1 {
		t.Fatalf("configuration renderer exit restarted: views=%d err=%v", views, err)
	}
}
