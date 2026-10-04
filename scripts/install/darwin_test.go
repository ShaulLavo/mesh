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

	"github.com/shaul/mesh/internal/identity"
	"golang.org/x/crypto/ssh"
)

func TestDarwinInstallerLaunchdActivation(t *testing.T) {
	for _, tool := range []string{"sh", "cat", "install", "base64", "cmp", "grep", "chmod", "mv", "id", "mkdir", "rm"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("Darwin shell installer fixture requires %s: %v", tool, err)
		}
	}
	newBinary, adopterID, authorizedKey := darwinFixtureBinary(t)
	tests := []struct {
		name, mode, problem                           string
		removal, otherRemoval, failures               int
		installed, binaryChanged, keyChanged, pending bool
		otherLoaded                                   bool
		wantBootstraps, wantBootouts, wantWaits       int
	}{
		{name: "immediate", wantBootstraps: 1, wantBootouts: 1},
		{name: "delayed removal", removal: 3, wantBootstraps: 1, wantBootouts: 1, wantWaits: 3},
		{name: "transient bootstrap", failures: 2, wantBootstraps: 3, wantBootouts: 1, wantWaits: 2},
		{name: "delayed removal and transient bootstrap", removal: 3, failures: 2, wantBootstraps: 3, wantBootouts: 1, wantWaits: 5},
		{name: "permanent bootstrap", mode: "bootstrap-fails", problem: "Bootstrap failed: 5: fixture input/output error", wantBootstraps: 20, wantBootouts: 1, wantWaits: 19},
		{name: "shared retry budget", removal: 3, mode: "bootstrap-fails", problem: "Bootstrap failed: 5: fixture input/output error", wantBootstraps: 17, wantBootouts: 1, wantWaits: 19},
		{name: "permanent removal", removal: 1000, problem: "launchctl bootout timed out", wantBootouts: 1, wantWaits: 19},
		{name: "bootout failure stderr", mode: "bootout-fails", problem: "Bootout failed: fixture permission denied", wantBootouts: 1},
		{name: "other domain teardown", otherLoaded: true, otherRemoval: 3, wantBootstraps: 1, wantBootouts: 2, wantWaits: 3},
		{name: "both domains share retry budget", otherLoaded: true, otherRemoval: 3, removal: 3, mode: "bootstrap-fails", problem: "Bootstrap failed: 5: fixture input/output error", wantBootstraps: 14, wantBootouts: 2, wantWaits: 19},
		{name: "permanent other domain teardown preserves selected job", otherLoaded: true, otherRemoval: 1000, problem: "launchctl bootout timed out", wantBootouts: 1, wantWaits: 19},
		{name: "other domain removal preserves configured job", installed: true, otherLoaded: true, otherRemoval: 3, mode: "bootstrap-fails", wantBootouts: 1, wantWaits: 3},
		{name: "unchanged loaded service", installed: true},
		{name: "binary update keeps job loaded", installed: true, binaryChanged: true, mode: "bootstrap-fails"},
		{name: "authorized key update keeps job loaded", installed: true, keyChanged: true, mode: "bootstrap-fails"},
		{name: "failed restart keeps job loaded", installed: true, binaryChanged: true, mode: "kickstart-fails", problem: "Kickstart failed: fixture permission denied"},
		{name: "pending activation reloads configuration", installed: true, pending: true, wantBootstraps: 1, wantBootouts: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Command names in a valid path must not inflate the call counts.
			home := filepath.Join(t.TempDir(), "bootstrap sleep fixture")
			bin := filepath.Join(home, "fixture-bin")
			for _, dir := range []string{bin, filepath.Join(home, ".local", "bin"), filepath.Join(home, ".local", "state", "mesh"), filepath.Join(home, "Library", "LaunchAgents")} {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			writeDarwinFixture(t, filepath.Join(bin, "launchctl"), launchctlFixture, 0755)
			writeDarwinFixture(t, filepath.Join(bin, "sleep"), "#!/bin/sh\nprintf '%s\\n' \"sleep $*\" >>\"$LAUNCHCTL_FIXTURE/calls\"\n", 0755)
			writeDarwinFixture(t, filepath.Join(home, "loaded"), "", 0600)
			if tt.otherLoaded {
				writeDarwinFixture(t, filepath.Join(home, "other-loaded"), "", 0600)
			}
			writeDarwinFixture(t, filepath.Join(home, "other-removal"), strconv.Itoa(tt.otherRemoval), 0600)
			writeDarwinFixture(t, filepath.Join(home, "removal"), strconv.Itoa(tt.removal), 0600)
			writeDarwinFixture(t, filepath.Join(home, "failures"), strconv.Itoa(tt.failures), 0600)
			binary := "old binary\n"
			if tt.installed && !tt.binaryChanged {
				binary = newBinary
			}
			writeDarwinFixture(t, filepath.Join(home, ".local", "bin", "mesh"), binary, 0755)
			source := filepath.Join(home, "candidate")
			writeDarwinFixture(t, source, newBinary, 0755)
			service, err := RenderService("darwin", ServiceOptions{DaemonPort: 7337, SSHPort: 2222, WebSocketPath: "/mesh"})
			if err != nil {
				t.Fatal(err)
			}
			if tt.installed {
				writeDarwinFixture(t, filepath.Join(home, "Library", "LaunchAgents", "dev.shaulavo.mesh.plist"), service, 0644)
				approveDarwinFixture(t, filepath.Join(home, ".local", "state", "mesh"), adopterID, tt.keyChanged)
			}
			marker := filepath.Join(home, ".local", "state", "mesh", "activation.pending")
			if tt.pending {
				writeDarwinFixture(t, marker, "", 0600)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "sh", "-s", "--", source, "7337", "2222", "/mesh", base64.StdEncoding.EncodeToString([]byte(authorizedKey)), base64.StdEncoding.EncodeToString([]byte(service))) //nolint:gosec // embedded installer, fixture-only arguments, and an external launchctl stub
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
			counts := make(map[string]int)
			for line := range strings.SplitSeq(string(calls), "\n") {
				command, _, _ := strings.Cut(line, " ")
				counts[command]++
				if command == "sleep" && line != "sleep 0.25" {
					t.Fatalf("unexpected activation backoff %q", line)
				}
			}
			if counts["bootstrap"] != tt.wantBootstraps || counts["bootout"] != tt.wantBootouts || counts["sleep"] != tt.wantWaits {
				t.Fatalf("activation calls = bootstrap %d, bootout %d, sleep %d; want %d, %d, %d:\n%s", counts["bootstrap"], counts["bootout"], counts["sleep"], tt.wantBootstraps, tt.wantBootouts, tt.wantWaits, calls)
			}
			if tt.mode == "bootout-fails" || tt.wantBootouts == 0 || tt.otherRemoval >= 20 || (tt.installed && tt.otherLoaded) {
				if _, err := os.Stat(filepath.Join(home, "loaded")); err != nil {
					t.Fatalf("existing job was unloaded: %v", err)
				}
			}
			if tt.problem != "" {
				if _, err := os.Stat(marker); err != nil {
					t.Fatalf("failed activation lost its recovery marker: %v", err)
				}
				return
			}
			installed, err := os.ReadFile(filepath.Join(home, ".local", "bin", "mesh")) //nolint:gosec // fixture installation beneath t.TempDir
			if err != nil || string(installed) != newBinary {
				t.Fatalf("installed binary = %q, error %v", installed, err)
			}
			if _, err := os.Stat(filepath.Join(home, "loaded")); err != nil {
				t.Fatalf("daemon was not loaded: %v", err)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("activation marker was not cleared: %v", err)
			}
			wantResult := "MESH_INSTALL_RESULT=configured"
			if tt.installed && !tt.binaryChanged && !tt.keyChanged && !tt.pending && !tt.otherLoaded {
				wantResult = "MESH_INSTALL_RESULT=unchanged"
				if counts["kickstart"] != 0 {
					t.Fatalf("unchanged install restarted its job:\n%s", calls)
				}
			} else if counts["kickstart"] != 1 {
				t.Fatalf("activation did not restart its job:\n%s", calls)
			}
			if !strings.Contains(string(output), wantResult) {
				t.Fatalf("installer did not report %q:\n%s", wantResult, output)
			}
		})
	}
}

func approveDarwinFixture(t *testing.T, state, id string, keyChanged bool) {
	t.Helper()
	if keyChanged {
		other, _, err := identity.LoadOrCreate(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		id = other.ID
	}
	if err := identity.ApproveDevice(state, id); err != nil {
		t.Fatal(err)
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
loaded_file=$LAUNCHCTL_FIXTURE/loaded
removal_file=$LAUNCHCTL_FIXTURE/removal
case "$2" in
user/*)
    loaded_file=$LAUNCHCTL_FIXTURE/other-loaded
    removal_file=$LAUNCHCTL_FIXTURE/other-removal
    ;;
esac
case "$1" in
print)
    case "$2" in
    gui/*/dev.shaulavo.mesh|user/*/dev.shaulavo.mesh)
        [ ! -f "$loaded_file" ] || exit 0
        remaining=$(cat "$removal_file")
        if [ "$remaining" -gt 0 ]; then
            printf '%s\n' "$((remaining - 1))" >"$removal_file"
            exit 0
        fi
        exit 1
        ;;
    gui/*) exit 0 ;;
    *) exit 1 ;;
    esac
    ;;
bootout)
    if [ "$LAUNCHCTL_MODE" = bootout-fails ]; then
        echo 'Bootout failed: fixture permission denied' >&2
        exit 1
    fi
    rm "$loaded_file"
    ;;
bootstrap)
    remaining=$(cat "$LAUNCHCTL_FIXTURE/removal")
    other_remaining=$(cat "$LAUNCHCTL_FIXTURE/other-removal")
    failures=$(cat "$LAUNCHCTL_FIXTURE/failures")
    if [ "$remaining" -gt 0 ] || [ "$other_remaining" -gt 0 ] || [ "$failures" -gt 0 ] || [ "$LAUNCHCTL_MODE" = bootstrap-fails ]; then
        [ "$failures" -eq 0 ] || printf '%s\n' "$((failures - 1))" >"$LAUNCHCTL_FIXTURE/failures"
        echo 'Bootstrap failed: 5: fixture input/output error' >&2
        exit 1
    fi
    : >"$LAUNCHCTL_FIXTURE/loaded"
    ;;
kickstart)
    if [ "$LAUNCHCTL_MODE" = kickstart-fails ]; then
        echo 'Kickstart failed: fixture permission denied' >&2
        exit 1
    fi
    [ -f "$LAUNCHCTL_FIXTURE/loaded" ]
    ;;
*) exit 1 ;;
esac
`

func darwinFixtureBinary(t *testing.T) (string, string, string) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("native installer fixture needs Go: %v", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(t.TempDir(), "mesh")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", candidate, "./cmd/mesh") //nolint:gosec // fresh binary built from this checkout into a fixture-owned path
	build.Dir = filepath.Clean(filepath.Join(cwd, "../.."))
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build native fixture: %v %s", err, output)
	}
	contents, err := os.ReadFile(candidate) //nolint:gosec // fixture-owned native binary
	if err != nil {
		t.Fatal(err)
	}
	actor, key, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	public, err := ssh.NewPublicKey(key.Public())
	if err != nil {
		t.Fatal(err)
	}
	return string(contents), actor.ID, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(public)))
}
