package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/updateinstall"
	"golang.org/x/sys/unix"
)

func TestDashboardRestartRecoversAfterInstallationLockSettlement(t *testing.T) {
	root, path, build := dashboardLockedInstallation(t)
	lock := dashboardHoldInstallationLock(t, root)
	defer func() { _ = lock.Close() }()
	cleaned, attempts, executions := false, 0, 0
	argv, env := []string{"mesh", "dashboard", "--wall"}, []string{"TERM=linux", "KEEP=value"}
	restart := dashboardRestart{current: release.Build{Digest: "old"}, argv: argv, env: env,
		installed: func(build release.Build) (string, error) { return dashboardInstalledTarget(root, "local", build, path) },
		exec: func(ctx context.Context, selected release.Build, selectedPath string, args, environment []string) error {
			attempts++
			if !cleaned {
				t.Fatal("handoff before terminal cleanup")
			}
			// The healthy receipt is visible while the real updater lock remains held.
			handoff, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
			defer cancel()
			err := updateinstall.WithCommittedExecutable(handoff, root, "local", selected, func(installed string) error {
				executions++
				if installed != path || selectedPath != path || !reflect.DeepEqual(args, argv) || !reflect.DeepEqual(environment, env) {
					t.Fatal("handoff changed installed pathname, argv or env")
				}
				contender := dashboardOpenInstallationLock(t, root)
				defer func() { _ = contender.Close() }()
				if err := unix.Flock(int(contender.Fd()), unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
					t.Fatalf("installed image is unlocked during exec: %v", err)
				}
				return nil
			})
			if attempts == 1 {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("held updater lock did not time out: %v", err)
				}
				if err := unix.Flock(int(lock.Fd()), unix.LOCK_UN); err != nil {
					t.Fatal(err)
				}
			}
			return dashboardHandoffResult(err)
		},
	}
	runs := 0
	input := DashboardInput{Watch: func(ctx context.Context, publish func(DashboardHostView)) error {
		publish(DashboardHostView{Host: DashboardHost{Local: true}, Build: build, Connection: StateReachable, LastReply: time.Now()})
		if runs == 1 {
			<-ctx.Done()
		}
		return nil
	}}
	err := restart.run(t.Context(), input, func(ctx context.Context, input DashboardInput) error {
		runs++
		err := input.Watch(ctx, func(DashboardHostView) {})
		cleaned = true
		return err
	})
	if err != nil || attempts != 2 || executions != 1 || runs != 1 {
		t.Fatalf("settled handoff: attempts=%d executions=%d runs=%d err=%v", attempts, executions, runs, err)
	}
}

func dashboardHandoffResult(err error) error {
	if err != nil {
		return fmt.Errorf("dashboard installation handoff: %w", err)
	}
	return nil
}

func dashboardLockedInstallation(t *testing.T) (root, path string, build release.Build) {
	t.Helper()
	root = t.TempDir()
	path = filepath.Join(root, "mesh")
	contents := []byte("healthy installed mesh")
	sum := sha256.Sum256(contents)
	build = release.Build{Digest: hex.EncodeToString(sum[:])}
	if err := os.WriteFile(path, contents, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "update"), 0700); err != nil {
		t.Fatal(err)
	}
	dashboardSaveInstallation(t, root, path, build)
	return root, path, build
}

func dashboardSaveInstallation(t *testing.T, root, path string, build release.Build) {
	t.Helper()
	data, err := json.Marshal(updateinstall.Status{Schema: 1, Phase: updateinstall.Committed, Settings: updateinstall.Settings{Executable: path}, Verified: &updateinstall.Health{HostID: "local", Build: build}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "update", "installation.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func dashboardOpenInstallationLock(t *testing.T, root string) *os.File {
	t.Helper()
	lock, err := os.OpenFile(filepath.Join(root, "update", "installation.lock"), os.O_CREATE|os.O_RDWR, 0600) //nolint:gosec // fixture lock beneath t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	return lock
}

func dashboardHoldInstallationLock(t *testing.T, root string) *os.File {
	t.Helper()
	lock := dashboardOpenInstallationLock(t, root)
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	return lock
}

func TestDashboardInstallationLockRetryBoundsAndRevalidation(t *testing.T) {
	for _, scenario := range []string{"persistent", "release-during-retry", "cancel-during-retry", "cancel", "parent-deadline", "exec-failure", "exec-deadline", "image-replaced", "receipt-replaced", "path-replaced"} {
		t.Run(scenario, func(t *testing.T) {
			root, path, build := dashboardLockedInstallation(t)
			lock := dashboardHoldInstallationLock(t, root)
			defer func() { _ = lock.Close() }()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			attempts, executions, runs := 0, 0, 0
			cleaned := false
			done := make(chan struct{})
			var wait sync.WaitGroup
			defer func() { close(done); wait.Wait() }()
			restart := dashboardRestart{current: release.Build{Digest: "old"},
				installed: func(build release.Build) (string, error) { return dashboardInstalledTarget(root, "local", build, path) },
				exec: func(ctx context.Context, selected release.Build, selectedPath string, _, _ []string) error {
					attempts++
					if !cleaned {
						t.Fatal("exec before cleanup")
					}
					timeout := 40 * time.Millisecond
					if scenario == "parent-deadline" {
						timeout = time.Second
					}
					if (scenario == "release-during-retry" || scenario == "cancel-during-retry") && attempts == 2 {
						timeout = time.Second
						wait.Add(1)
						go func() {
							defer wait.Done()
							timer := time.NewTimer(50 * time.Millisecond)
							defer timer.Stop()
							select {
							case <-done:
								return
							case <-timer.C:
							}
							if scenario == "cancel-during-retry" {
								cancel()
								return
							}
							_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
						}()
					}
					if scenario == "cancel" {
						cancel()
					}
					handoff, stop := ctx, func() {}
					// Callback errors use the existing handoff bound after lock acquisition settles.
					if attempts != 2 || (scenario != "exec-failure" && scenario != "exec-deadline") {
						handoff, stop = context.WithTimeout(ctx, timeout)
					}
					defer stop()
					err := updateinstall.WithCommittedExecutable(handoff, root, "local", selected, func(installed string) error {
						if installed != selectedPath {
							return errors.New("installed pathname changed")
						}
						executions++
						if scenario == "exec-failure" {
							return errors.New("injected exec failure\nsecond line")
						}
						if scenario == "exec-deadline" {
							return context.DeadlineExceeded
						}
						return nil
					})
					if attempts != 1 || scenario == "persistent" || scenario == "release-during-retry" || scenario == "cancel-during-retry" || scenario == "cancel" || scenario == "parent-deadline" {
						return dashboardHandoffResult(err)
					}
					switch scenario {
					case "image-replaced":
						replacement := filepath.Join(root, "replacement")
						if err := os.WriteFile(replacement, []byte("unvalidated replacement"), 0600); err != nil {
							t.Fatal(err)
						}
						if err := os.Rename(replacement, path); err != nil {
							t.Fatal(err)
						}
					case "receipt-replaced":
						dashboardSaveInstallation(t, root, path, release.Build{Digest: "different"})
					case "path-replaced":
						replacement := filepath.Join(root, "other-mesh")
						contents, err := os.ReadFile(path) //nolint:gosec // fixture image beneath t.TempDir
						if err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(replacement, contents, 0600); err != nil { //nolint:gosec // fixture copy beneath t.TempDir
							t.Fatal(err)
						}
						dashboardSaveInstallation(t, root, replacement, build)
					}
					if err := unix.Flock(int(lock.Fd()), unix.LOCK_UN); err != nil {
						t.Fatal(err)
					}
					return dashboardHandoffResult(err)
				},
			}
			input := DashboardInput{Watch: func(ctx context.Context, publish func(DashboardHostView)) error {
				publish(DashboardHostView{Host: DashboardHost{Local: true}, Build: build, Connection: StateReachable, LastReply: time.Now()})
				<-ctx.Done()
				return nil
			}}
			if scenario == "parent-deadline" {
				ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
			}
			started := time.Now()
			err := restart.run(ctx, input, func(ctx context.Context, input DashboardInput) error {
				runs++
				if runs == 2 {
					if input.Notice == "" || strings.ContainsAny(input.Notice, "\r\n") {
						t.Fatalf("missing one-line failure notice: %q", input.Notice)
					}
					return nil
				}
				err := input.Watch(ctx, func(DashboardHostView) {})
				cleaned = true
				return err
			})
			if scenario == "parent-deadline" && attempts == 0 {
				if err != nil || runs != 1 || executions != 0 || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
					t.Fatalf("early parent deadline: runs=%d exec=%d err=%v", runs, executions, err)
				}
				return
			}
			if scenario == "cancel" || scenario == "parent-deadline" || scenario == "cancel-during-retry" {
				expectedAttempts := 1
				if scenario == "cancel-during-retry" {
					expectedAttempts = 2
				}
				if !errors.Is(err, ctx.Err()) || attempts != expectedAttempts || runs != 1 || executions != 0 {
					t.Fatalf("cancellation retried or resumed: attempts=%d runs=%d exec=%d err=%v", attempts, runs, executions, err)
				}
				return
			}
			expectedRuns, expectedExec := 2, 0
			if scenario == "release-during-retry" {
				expectedRuns, expectedExec = 1, 1
			}
			if scenario == "exec-failure" || scenario == "exec-deadline" {
				expectedExec = 1
			}
			if err != nil || attempts != 2 || runs != expectedRuns || executions != expectedExec {
				t.Fatalf("retry bound: attempts=%d runs=%d exec=%d err=%v", attempts, runs, executions, err)
			}
			if scenario == "persistent" && time.Since(started) > time.Second {
				t.Fatal("persistent lock exceeded bounded handoff waits")
			}
		})
	}
}

func TestDashboardRestartHandoffHasOneBoundedSettlementRetry(t *testing.T) {
	attempts, runs := 0, 0
	restart := dashboardRestart{current: release.Build{Digest: "old"},
		installed: func(release.Build) (string, error) { return "/installed/mesh", nil },
		exec: func(ctx context.Context, _ release.Build, _ string, _, _ []string) error {
			attempts++
			deadline, ok := ctx.Deadline()
			expected := dashboardHandoffTimeout
			if attempts == 2 {
				expected = dashboardSettlementTimeout
			}
			remaining := time.Until(deadline)
			if !ok || remaining > expected || remaining < expected-time.Second {
				t.Fatalf("handoff %d lacks its bounded deadline: %v", attempts, remaining)
			}
			return updateinstall.ErrHandoffTimeout
		},
	}
	input := DashboardInput{Watch: func(ctx context.Context, publish func(DashboardHostView)) error {
		publish(DashboardHostView{Host: DashboardHost{Local: true}, Build: release.Build{Digest: "new"}, Connection: StateReachable, LastReply: time.Now()})
		<-ctx.Done()
		return nil
	}}
	err := restart.run(t.Context(), input, func(ctx context.Context, input DashboardInput) error {
		runs++
		if runs == 2 {
			return nil
		}
		return input.Watch(ctx, func(DashboardHostView) {})
	})
	if err != nil || attempts != 2 || runs != 2 {
		t.Fatalf("bounded handoff: attempts=%d runs=%d err=%v", attempts, runs, err)
	}
}
