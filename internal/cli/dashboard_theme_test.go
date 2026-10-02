package cli

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/paths"
)

func TestDashboardThemeConfigAndFlag(t *testing.T) {
	for _, example := range []struct{ name, config, flag, want string }{
		{name: "default", want: "oled"},
		{name: "config", config: "rose-pine", want: "rose-pine"},
		{name: "override", config: "rose-pine", flag: "kanagawa", want: "kanagawa"},
		{name: "override-invalid-config", config: "invalid", flag: "current", want: "current"},
	} {
		t.Run(example.name, func(t *testing.T) {
			t.Setenv("MESH_CONFIG_DIR", t.TempDir())
			t.Setenv("MESH_STATE_DIR", t.TempDir())
			if example.config != "" {
				writeDashboardThemeFixture(t, example.config)
			}
			state, err := paths.StateDir()
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := identity.LoadOrCreate(state); err != nil {
				t.Fatal(err)
			}
			args := []string{"dashboard"}
			if example.flag != "" {
				args = append(args, "--theme", example.flag)
			}
			called := false
			_, _, err = executeCommand(t, Dependencies{Dashboard: func(_ context.Context, input DashboardInput) error {
				called = true
				if input.Theme != example.want {
					t.Errorf("theme = %q, want %q", input.Theme, example.want)
				}
				return nil
			}}, args...)
			if err != nil {
				t.Fatal(err)
			}
			if !called {
				t.Fatal("dashboard was not invoked")
			}
		})
	}
}

func TestDashboardUnknownThemeListsNames(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(map[bool]string{false: "flag", true: "config"}[configured], func(t *testing.T) {
			t.Setenv("MESH_CONFIG_DIR", t.TempDir())
			t.Setenv("MESH_STATE_DIR", t.TempDir())
			args := []string{"dashboard", "--theme", "missing"}
			if configured {
				writeDashboardThemeFixture(t, "missing")
				args = []string{"dashboard"}
			}
			_, _, err := executeCommand(t, Dependencies{Dashboard: func(context.Context, DashboardInput) error { t.Fatal("invalid theme reached dashboard"); return nil }}, args...)
			if err == nil || !strings.Contains(err.Error(), `unknown dashboard theme "missing"`) {
				t.Fatalf("error = %v", err)
			}
			for _, name := range []string{"current", "rose-pine", "rose-pine-moon", "oled", "kanagawa", "gruvbox-material"} {
				if !strings.Contains(err.Error(), name) {
					t.Errorf("missing valid theme %q: %v", name, err)
				}
			}
		})
	}
}

func TestSaveHostPreservesDashboardTheme(t *testing.T) {
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	writeDashboardThemeFixture(t, "gruvbox-material")
	if err := SaveHost(HostRecord{Alias: "pc", ID: "host-key", MeshIdentity: "host-key", Endpoint: "ws://100.64.0.2:7777/mesh"}); err != nil {
		t.Fatal(err)
	}
	if _, err := RenameHost("pc", "desktop"); err != nil {
		t.Fatal(err)
	}
	config, err := loadHostConfig()
	if err != nil {
		t.Fatal(err)
	}
	if config.Dashboard == nil || config.Dashboard.Theme != "gruvbox-material" || len(config.Hosts) != 1 {
		t.Fatalf("setting lost on host save: %+v", config)
	}
}

func writeDashboardThemeFixture(t *testing.T, theme string) {
	t.Helper()
	path, err := ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	contents := `{"version":1,"hosts":[],"dashboard":{"theme":"` + theme + `"}}`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
