package updateinstall

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLaunchdStopHandlesAbsentAndRemovedJobs(t *testing.T) {
	for _, mode := range []string{"absent", "removed", "bootout-failed"} {
		t.Run(mode, func(t *testing.T) {
			service, root := launchdRemovalFixture(t, mode)
			err := service.Stop(context.Background())
			if mode == "bootout-failed" {
				if err == nil || !strings.Contains(err.Error(), "bootout refused") {
					t.Fatalf("bootout failure = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			_, err = os.Stat(filepath.Join(root, "bootout"))
			if mode == "absent" && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("absent job received bootout: %v", err)
			}
			if mode == "removed" && err != nil {
				t.Fatalf("loaded job missed bootout: %v", err)
			}
			if err = service.Stop(context.Background()); err != nil {
				t.Fatalf("repeated Stop = %v", err)
			}
		})
	}
}

func TestLaunchdStopStartsCandidateAfterImmediateRemoval(t *testing.T) {
	service, root := launchdRemovalFixture(t, "removed")
	if err := service.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "bootstrapped")); err != nil {
		t.Fatalf("Start skipped candidate bootstrap after immediate removal: %v", err)
	}
}

func TestLaunchdStopRejectsUnexpectedPrintFailures(t *testing.T) {
	for _, mode := range []string{"inspect-failed", "removal-failed"} {
		t.Run(mode, func(t *testing.T) {
			service, root := launchdRemovalFixture(t, mode)
			err := service.Stop(context.Background())
			if err == nil || !strings.Contains(err.Error(), "query refused") {
				t.Fatalf("unexpected print failure accepted: %v", err)
			}
			_, err = os.Stat(filepath.Join(root, "bootout"))
			if mode == "inspect-failed" && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed inspection received bootout: %v", err)
			}
		})
	}
}

func TestLaunchdStartRejectsUnexpectedPrintFailure(t *testing.T) {
	service, root := launchdRemovalFixture(t, "inspect-failed")
	err := service.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "query refused") {
		t.Fatalf("unexpected print failure accepted: %v", err)
	}
	if _, err = os.Stat(filepath.Join(root, "bootstrapped")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed inspection received bootstrap: %v", err)
	}
}

func TestLaunchdStopRejectsMissingLaunchctl(t *testing.T) {
	service, root := launchdRemovalFixture(t, "removed")
	if err := os.Remove(filepath.Join(root, "launchctl")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root)
	if err := service.Stop(context.Background()); !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("missing launchctl accepted: %v", err)
	}
}

func TestLaunchdStopBoundsOutgoingRegistration(t *testing.T) {
	service, _ := launchdRemovalFixture(t, "stuck")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started := time.Now()
	err := service.Stop(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("registered outgoing job returned %v", err)
	}
	if elapsed := time.Since(started); elapsed > 8*time.Second {
		t.Fatalf("Stop exceeded its five-second removal budget: %s", elapsed)
	}
	if ctx.Err() != nil {
		t.Fatalf("Stop exhausted the caller's longer deadline: %v", ctx.Err())
	}
}

func TestLaunchdStopCancelsOutgoingRegistrationWait(t *testing.T) {
	for _, mode := range []string{"stuck", "blocked-print"} {
		t.Run(mode, func(t *testing.T) {
			service, root := launchdRemovalFixture(t, mode)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- service.Stop(ctx) }()
			probeCtx, stopProbe := context.WithTimeout(context.Background(), 2*time.Second)
			defer stopProbe()
			for {
				if _, err := os.Stat(filepath.Join(root, "polled")); err == nil {
					break
				}
				if err := waitContext(probeCtx, time.Millisecond); err != nil {
					t.Fatalf("Stop never checked the outgoing job: %v", err)
				}
			}
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled removal returned %v", err)
				}
			case <-probeCtx.Done():
				t.Fatal("Stop ignored caller cancellation")
			}
		})
	}
}

func launchdRemovalFixture(t *testing.T, mode string) (*SystemService, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MESH_TEST_LAUNCHD_STATE", root)
	t.Setenv("MESH_TEST_LAUNCHD_MODE", mode)
	commands := map[string]string{
		"plutil": "#!/bin/sh\nprintf 'true\\n'\n",
		"launchctl": `#!/bin/sh
set -eu
state=$MESH_TEST_LAUNCHD_STATE
mode=$MESH_TEST_LAUNCHD_MODE
case "$1" in
print)
    if [ "$mode" = inspect-failed ]; then
        printf 'query refused\n' >&2
        exit 113
    fi
    if [ ! -f "$state/loaded" ]; then
        printf 'Bad request.\nCould not find service "dev.fixture.mesh" in domain for user gui: 123\n' >&2
        exit 113
    fi
    if [ -f "$state/bootout" ]; then
        : >"$state/polled"
        if [ "$mode" = removal-failed ]; then
            printf 'query refused\n' >&2
            exit 113
        fi
        if [ "$mode" = blocked-print ]; then
            exec sleep 10
        fi
    fi
    ;;
bootout)
    : >"$state/bootout"
    if [ "$mode" = bootout-failed ]; then
        printf 'bootout refused\n' >&2
        exit 1
    fi
    if [ "$mode" = removed ]; then
        rm "$state/loaded"
    fi
    ;;
bootstrap)
    : >"$state/loaded"
    : >"$state/bootstrapped"
    ;;
*) exit 1 ;;
esac
`,
	}
	for name, contents := range commands {
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0700); err != nil { //nolint:gosec // executable service-manager fixture beneath t.TempDir
			t.Fatal(err)
		}
	}
	if mode != "absent" {
		if err := os.WriteFile(filepath.Join(root, "loaded"), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	service, err := NewSystemService(ServiceSpec{Kind: "launchd", Name: "dev.fixture.mesh", Domain: "gui/123", ConfigPath: filepath.Join(root, "daemon.plist")})
	if err != nil {
		t.Fatal(err)
	}
	return service, root
}
