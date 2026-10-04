package updateinstall

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLaunchdStopWaitsForOutgoingRegistration(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MESH_LAUNCHD_FIXTURE", root)
	commands := map[string]string{
		"plutil": "#!/bin/sh\nprintf 'true\\n'\n",
		"launchctl": `#!/bin/sh
set -eu
state=$MESH_LAUNCHD_FIXTURE
case "$1" in
print)
    if [ -f "$state/outgoing" ]; then
        remaining=$(cat "$state/outgoing")
        if [ "$remaining" -gt 0 ]; then
            printf '%s\n' "$((remaining - 1))" >"$state/outgoing"
            exit 0
        fi
        rm "$state/outgoing" "$state/loaded"
    fi
    if [ ! -f "$state/loaded" ]; then
        printf 'Bad request.\nCould not find service "dev.fixture.mesh" in domain for user gui: 123\n' >&2
        exit 113
    fi
    ;;
bootout)
    printf '2\n' >"$state/outgoing"
    ;;
bootstrap)
    [ ! -f "$state/outgoing" ]
    : >"$state/loaded"
    : >"$state/bootstrapped"
    ;;
*) exit 1 ;;
esac
`,
	}
	for name, contents := range commands {
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0755); err != nil { //nolint:gosec // executable service-manager fixture beneath t.TempDir
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "loaded"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	service, err := NewSystemService(ServiceSpec{Kind: "launchd", Name: "dev.fixture.mesh", Domain: "gui/123", ConfigPath: filepath.Join(root, "daemon.plist")})
	if err != nil {
		t.Fatal(err)
	}
	if err = service.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(root, "bootstrapped")); err != nil {
		t.Fatal("Start skipped candidate bootstrap because Stop returned while the outgoing job was still registered")
	}
}
