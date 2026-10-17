package install

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/identity"
)

func TestInstallersGrantOnlySelectedControllersAndPreserveApprovals(t *testing.T) {
	binary, source, sourceKey := darwinFixtureBinary(t)
	controller := installerControllerID(t)
	preexisting := installerControllerID(t)
	unselected := installerControllerID(t)
	for _, platform := range []string{"linux", "darwin"} {
		t.Run(platform, func(t *testing.T) {
			checkInstallerControllerGrants(t, platform, binary, source, sourceKey, controller, preexisting, unselected)
		})
	}
}

func checkInstallerControllerGrants(t *testing.T, platform, binary, source, sourceKey, controller, preexisting, unselected string) {
	t.Helper()
	home := t.TempDir()
	state := filepath.Join(home, ".local", "state", "mesh")
	bin := filepath.Join(home, "fixture-bin")
	for _, dir := range []string{state, bin, filepath.Join(home, ".local", "bin")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	writeDarwinFixture(t, filepath.Join(home, ".local", "bin", "mesh"), binary, 0755)
	writeDarwinFixture(t, filepath.Join(bin, "systemctl"), "#!/bin/sh\nexit 0\n", 0755)
	writeDarwinFixture(t, filepath.Join(bin, "loginctl"), "#!/bin/sh\necho Linger=yes\n", 0755)
	writeDarwinFixture(t, filepath.Join(bin, "launchctl"), selectedControllerLaunchctl, 0755)
	if err := identity.ApproveDevice(state, preexisting); err != nil {
		t.Fatal(err)
	}
	prior, ok := identity.BindIdentity(state, preexisting)
	if !ok {
		t.Fatal("preexisting grant was not approved")
	}
	first := runControllerInstaller(t, platform, home, bin, sourceKey, controller)
	if !strings.Contains(first, "MESH_INSTALL_RESULT=configured") {
		t.Fatalf("first installation = %s", first)
	}
	assertInstallerControllers(t, state, []string{source, controller, preexisting}, unselected)
	selected, ok := identity.BindIdentity(state, controller)
	if !ok {
		t.Fatal("selected controller was not approved")
	}
	before, err := os.ReadFile(filepath.Join(state, "authorized_keys")) //nolint:gosec // task-owned installer fixture policy
	if err != nil {
		t.Fatal(err)
	}
	second := runControllerInstaller(t, platform, home, bin, sourceKey, controller)
	after, err := os.ReadFile(filepath.Join(state, "authorized_keys")) //nolint:gosec // task-owned installer fixture policy
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second, "MESH_INSTALL_RESULT=unchanged") || !slices.Equal(before, after) || !prior() || !selected() {
		t.Fatalf("repeated installation changed approvals: before=%q after=%q result=%s", before, after, second)
	}
}

func installerControllerID(t *testing.T) string {
	t.Helper()
	actor, _, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return actor.ID
}

func assertInstallerControllers(t *testing.T, state string, selected []string, unselected string) {
	t.Helper()
	for _, id := range selected {
		if !identity.GrantedIdentity(state, id) {
			t.Fatalf("selected or existing controller %s was not approved", id)
		}
	}
	grants, err := identity.DeviceGrants(filepath.Join(state, "authorized_keys"))
	if err != nil || len(grants) != len(selected) || identity.GrantedIdentity(state, unselected) {
		t.Fatalf("unexpected grant selection: %d grants, unselected approved=%t, error=%v", len(grants), identity.GrantedIdentity(state, unselected), err)
	}
}

func runControllerInstaller(t *testing.T, platform, home, bin, sourceKey, controller string) string {
	t.Helper()
	script, ok := Script(platform)
	if !ok {
		t.Fatal("installer missing")
	}
	service, err := RenderService(platform, ServiceOptions{DaemonPort: 7337, SSHPort: 2222, WebSocketPath: "/mesh"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-s", "--", "", "7337", "2222", "/mesh", base64.StdEncoding.EncodeToString([]byte(sourceKey)), base64.StdEncoding.EncodeToString([]byte(service)), controller) //nolint:gosec // embedded installer and task-owned fixture arguments
	cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	cmd.Stdin = strings.NewReader(script)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s selected controller installation: %v\n%s", platform, err, output)
	}
	return string(output)
}

const selectedControllerLaunchctl = `#!/bin/sh
set -eu
case "$1" in
print)
    case "$2" in
    gui/*/dev.shaulavo.mesh) [ -f "$HOME/loaded" ] ;;
    user/*/dev.shaulavo.mesh) exit 1 ;;
    gui/*) exit 0 ;;
    *) exit 1 ;;
    esac
    ;;
bootstrap) touch "$HOME/loaded" ;;
kickstart) [ -f "$HOME/loaded" ] ;;
bootout) rm -f "$HOME/loaded" ;;
*) exit 1 ;;
esac
`
