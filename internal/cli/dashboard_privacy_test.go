package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/release"
)

func TestDashboardEnvironmentPrivacyKeepsCurrentExecutable(t *testing.T) {
	t.Setenv("MESH_PRIVACY", "1")
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	t.Setenv("MESH_STATE_DIR", t.TempDir())
	state, err := paths.StateDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := identity.LoadOrCreate(state); err != nil {
		t.Fatal(err)
	}
	called := false
	_, _, err = executeCommand(t, Dependencies{Dashboard: func(ctx context.Context, input DashboardInput) error {
		called = true
		if input.Privacy == nil {
			t.Fatal("environment-only dashboard lost its privacy policy")
		}
		input.Watch = func(_ context.Context, publish func(DashboardHostView)) error {
			publish(DashboardHostView{Host: DashboardHost{ID: "local", MachineName: "workstation", Local: true}, Build: release.Build{Digest: "older-installed-image"}, Connection: StateReachable, LastReply: time.Now()})
			return nil
		}
		restart := dashboardRestart{current: release.Build{Digest: "privacy-capable-image"},
			installed: func(release.Build) (string, error) {
				t.Fatal("privacy inspected a cross-binary restart target")
				return "", nil
			},
			exec: func(context.Context, release.Build, string, []string, []string) error {
				t.Fatal("privacy executed a potentially privacy-unaware image")
				return nil
			},
		}
		runs, updates := 0, 0
		err := restart.run(ctx, input, func(ctx context.Context, next DashboardInput) error {
			runs++
			if next.Privacy != input.Privacy {
				t.Fatal("dashboard restart wrapper changed the privacy policy")
			}
			return next.Watch(ctx, func(view DashboardHostView) {
				updates++
				if view.Host.MachineName != "workstation" || view.Build.Digest != "older-installed-image" {
					t.Fatal("skipped restart discarded ordinary host observations")
				}
				if !strings.Contains(view.Notice, "restart skipped in privacy mode") || strings.ContainsAny(view.Notice, "\r\n") {
					t.Fatalf("missing one-line skipped-restart notice: %q", view.Notice)
				}
			})
		})
		if runs != 1 || updates != 1 {
			t.Fatalf("privacy restarted its renderer: runs=%d updates=%d", runs, updates)
		}
		return err
	}}, "dashboard")
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("dashboard command did not run")
	}
}
