package install

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDarwinInstallerLaunchdActivation(t *testing.T) {
	for _, tool := range []string{"sh", "cat", "install", "base64", "awk", "cmp", "grep", "chmod", "mv", "id", "mkdir", "rm"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("Darwin shell installer fixture requires %s: %v", tool, err)
		}
	}
	tests := []struct {
		name, mode, problem string
		removal, failures   int
		wantBootstraps      int
	}{
		{name: "immediate", wantBootstraps: 1},
		{name: "delayed removal", removal: 3, wantBootstraps: 1},
		{name: "transient bootstrap", failures: 2, wantBootstraps: 3},
		{name: "delayed removal and transient bootstrap", removal: 3, failures: 2, wantBootstraps: 3},
		{name: "permanent bootstrap", mode: "bootstrap-fails", problem: "Bootstrap failed: 5: fixture input/output error"},
		{name: "shared retry budget", removal: 3, mode: "bootstrap-fails", problem: "Bootstrap failed: 5: fixture input/output error"},
		{name: "permanent removal", removal: 1000, problem: "launchctl bootout timed out"},
		{name: "bootout failure stderr", mode: "bootout-fails", problem: "Bootout failed: fixture permission denied"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			bin := filepath.Join(home, "fixture-bin")
			if err := os.MkdirAll(filepath.Join(home, ".local", "bin"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(bin, 0700); err != nil {
				t.Fatal(err)
			}
			writeDarwinFixture(t, filepath.Join(bin, "launchctl"), launchctlFixture, 0755)
			writeDarwinFixture(t, filepath.Join(bin, "sleep"), "#!/bin/sh\nprintf '%s\\n' \"sleep $*\" >>\"$LAUNCHCTL_FIXTURE/calls\"\n", 0755)
			writeDarwinFixture(t, filepath.Join(home, "loaded"), "", 0600)
			writeDarwinFixture(t, filepath.Join(home, "removal"), strconv.Itoa(tt.removal), 0600)
			writeDarwinFixture(t, filepath.Join(home, "failures"), strconv.Itoa(tt.failures), 0600)
			writeDarwinFixture(t, filepath.Join(home, ".local", "bin", "mesh"), "old binary\n", 0755)
			source := filepath.Join(home, "candidate")
			writeDarwinFixture(t, source, "new binary\n", 0755)
			service, err := RenderService("darwin", ServiceOptions{DaemonPort: 7337, SSHPort: 2222, WebSocketPath: "/mesh"})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "sh", "-s", "--", source, "7337", "2222", "/mesh", base64.StdEncoding.EncodeToString([]byte("ssh-ed25519 fixture")), base64.StdEncoding.EncodeToString([]byte(service))) //nolint:gosec // embedded installer, fixture-only arguments, and an external launchctl stub
			cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "LAUNCHCTL_FIXTURE="+home, "LAUNCHCTL_MODE="+tt.mode)
			cmd.Stdin = strings.NewReader(darwin)
			output, runErr := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("installer exceeded fixture deadline: %v\n%s", ctx.Err(), output)
			}
			if tt.problem == "" && runErr != nil {
				t.Fatalf("installer failed: %v\n%s", runErr, output)
			}
			if tt.problem != "" && (runErr == nil || !strings.Contains(string(output), tt.problem) || !strings.Contains(string(output), "MESH_BOOTSTRAP_ERROR=service_install")) {
				t.Fatalf("want bounded failure containing %q; got %v\n%s", tt.problem, runErr, output)
			}
			calls, err := os.ReadFile(filepath.Join(home, "calls")) //nolint:gosec // fixture call log beneath t.TempDir
			if err != nil {
				t.Fatal(err)
			}
			bootstraps := strings.Count(string(calls), "bootstrap ")
			waits := strings.Count(string(calls), "sleep ")
			if tt.problem == "" && bootstraps != tt.wantBootstraps {
				t.Fatalf("bootstrap attempts = %d, want %d:\n%s", bootstraps, tt.wantBootstraps, calls)
			}
			if tt.removal > 0 && tt.problem == "" && waits < tt.removal {
				t.Fatalf("bootstrap preceded teardown observation:\n%s", calls)
			}
			if waits > 19 || bootstraps > 20 {
				t.Fatalf("activation exceeded shared retry budget:\n%s", calls)
			}
			if tt.mode == "bootstrap-fails" && (waits != 19 || bootstraps != 20-tt.removal) {
				t.Fatalf("teardown and bootstrap did not share their retry budget:\n%s", calls)
			}
			if tt.name == "permanent removal" && (waits != 19 || bootstraps != 0) {
				t.Fatalf("bootstrap ran before permanent teardown settled:\n%s", calls)
			}
			if tt.problem != "" {
				return
			}
			installed, err := os.ReadFile(filepath.Join(home, ".local", "bin", "mesh")) //nolint:gosec // fixture installation beneath t.TempDir
			if err != nil || string(installed) != "new binary\n" {
				t.Fatalf("installed binary = %q, error %v", installed, err)
			}
			if _, err := os.Stat(filepath.Join(home, "loaded")); err != nil {
				t.Fatalf("daemon was not loaded: %v", err)
			}
			if _, err := os.Stat(filepath.Join(home, ".local", "state", "mesh", "activation.pending")); !os.IsNotExist(err) {
				t.Fatalf("activation marker was not cleared: %v", err)
			}
		})
	}
}

func writeDarwinFixture(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

const launchctlFixture = `#!/bin/sh
set -eu
printf '%s\n' "$*" >>"$LAUNCHCTL_FIXTURE/calls"
case "$1" in
print)
    case "$2" in
    gui/*/dev.shaulavo.mesh)
        [ ! -f "$LAUNCHCTL_FIXTURE/loaded" ] || exit 0
        remaining=$(cat "$LAUNCHCTL_FIXTURE/removal")
        if [ "$remaining" -gt 0 ]; then
            printf '%s\n' "$((remaining - 1))" >"$LAUNCHCTL_FIXTURE/removal"
            exit 0
        fi
        exit 1
        ;;
    user/*/dev.shaulavo.mesh) exit 1 ;;
    gui/*) exit 0 ;;
    *) exit 1 ;;
    esac
    ;;
bootout)
    if [ "$LAUNCHCTL_MODE" = bootout-fails ]; then
        echo 'Bootout failed: fixture permission denied' >&2
        exit 1
    fi
    rm "$LAUNCHCTL_FIXTURE/loaded"
    ;;
bootstrap)
    remaining=$(cat "$LAUNCHCTL_FIXTURE/removal")
    failures=$(cat "$LAUNCHCTL_FIXTURE/failures")
    if [ "$remaining" -gt 0 ] || [ "$failures" -gt 0 ] || [ "$LAUNCHCTL_MODE" = bootstrap-fails ]; then
        [ "$failures" -eq 0 ] || printf '%s\n' "$((failures - 1))" >"$LAUNCHCTL_FIXTURE/failures"
        echo 'Bootstrap failed: 5: fixture input/output error' >&2
        exit 1
    fi
    : >"$LAUNCHCTL_FIXTURE/loaded"
    ;;
kickstart) [ -f "$LAUNCHCTL_FIXTURE/loaded" ] ;;
*) exit 1 ;;
esac
`
