package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/updateinstall"
)

func TestDashboardRestartAfterHealthyLocalBuildAndTerminalCleanup(t *testing.T) {
	old, next := release.Build{Digest: "old"}, release.Build{Digest: "new"}
	cleaned, calls := false, 0
	argv, env := []string{"mesh", "dashboard", "--wall", "--theme", "nord"}, []string{"TERM=linux", "KEEP=value"}
	restart := dashboardRestart{current: old, argv: argv, env: env,
		installed: func(build release.Build) (string, error) {
			if build != next {
				t.Fatalf("unexpected target build: %+v", build)
			}
			return "/installed/mesh", nil
		},
		exec: func(path string, args, environment []string) error {
			calls++
			if !cleaned || path != "/installed/mesh" || !reflect.DeepEqual(args, argv) || !reflect.DeepEqual(environment, env) {
				t.Fatal("exec lost terminal cleanup, installed path, argv or env")
			}
			return nil
		},
	}
	input := DashboardInput{Watch: func(ctx context.Context, publish func(DashboardHostView)) error {
		for _, view := range []DashboardHostView{
			{Host: DashboardHost{Local: false}, Build: next, Connection: StateReachable, LastReply: time.Now()},
			{Host: DashboardHost{Local: true}, Build: next, Connection: StateUnreachable},
			{Host: DashboardHost{Local: true}, Build: old, Connection: StateReachable, LastReply: time.Now()},
			{Host: DashboardHost{Local: true}, Build: next, Connection: StateReachable, LastReply: time.Now()},
		} {
			publish(view)
		}
		<-ctx.Done()
		return nil
	}}
	runner := func(ctx context.Context, input DashboardInput) error {
		err := input.Watch(ctx, func(DashboardHostView) {})
		cleaned = true
		return err
	}
	if err := restart.run(t.Context(), input, runner); err != nil || calls != 1 {
		t.Fatalf("restart: calls=%d err=%v", calls, err)
	}
}

func TestDashboardFailedExecContinuesOnceWithNotice(t *testing.T) {
	attempts, runs := 0, 0
	restart := dashboardRestart{current: release.Build{Digest: "old"},
		installed: func(release.Build) (string, error) { return "/installed/mesh", nil },
		exec: func(string, []string, []string) error {
			attempts++
			return errors.New("permission denied\nsecond line")
		},
	}
	input := DashboardInput{Watch: func(ctx context.Context, publish func(DashboardHostView)) error {
		publish(DashboardHostView{Host: DashboardHost{Local: true}, Build: release.Build{Digest: "new"}, Connection: StateReachable, LastReply: time.Now()})
		if runs == 1 {
			<-ctx.Done()
		}
		return nil
	}}
	runner := func(ctx context.Context, input DashboardInput) error {
		runs++
		if runs == 2 && (!strings.Contains(input.Notice, "permission denied") || strings.ContainsAny(input.Notice, "\r\n")) {
			t.Fatalf("missing one-line failure notice: %q", input.Notice)
		}
		if runs > 2 {
			t.Fatal("restart loop")
		}
		return input.Watch(ctx, func(DashboardHostView) {})
	}
	if err := restart.run(t.Context(), input, runner); err != nil || attempts != 1 || runs != 2 {
		t.Fatalf("failed exec: attempts=%d runs=%d err=%v", attempts, runs, err)
	}
}

func TestDashboardRestartWaitsForInstalledHealthCommit(t *testing.T) {
	checks, attempts := 0, 0
	restart := dashboardRestart{current: release.Build{Digest: "old"},
		installed: func(release.Build) (string, error) {
			checks++
			if checks == 1 {
				return "", nil
			}
			return "/installed/mesh", nil
		},
		exec: func(string, []string, []string) error { attempts++; return nil },
	}
	input := DashboardInput{Watch: func(ctx context.Context, publish func(DashboardHostView)) error {
		view := DashboardHostView{Host: DashboardHost{Local: true}, Build: release.Build{Digest: "new"}, Connection: StateReachable, LastReply: time.Now()}
		publish(view)
		if ctx.Err() != nil {
			t.Fatal("restarted before health committed")
		}
		publish(view)
		<-ctx.Done()
		return nil
	}}
	if err := restart.run(t.Context(), input, func(ctx context.Context, input DashboardInput) error {
		return input.Watch(ctx, func(DashboardHostView) {})
	}); err != nil || attempts != 1 || checks != 2 {
		t.Fatalf("health gate: checks=%d attempts=%d err=%v", checks, attempts, err)
	}
}

func TestDashboardInstalledTargetRequiresCommittedMatchingImage(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "mesh")
	contents := []byte("new mesh executable")
	sum := sha256.Sum256(contents)
	build := release.Build{Digest: hex.EncodeToString(sum[:])}
	if err := os.WriteFile(path, contents, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "update"), 0700); err != nil {
		t.Fatal(err)
	}
	status := updateinstall.Status{Schema: 1, Phase: updateinstall.Validating, Settings: updateinstall.Settings{Executable: path}, Verified: &updateinstall.Health{HostID: "local", Build: build}, Candidate: filepath.Join(root, "staging"), Previous: filepath.Join(root, "previous")}
	save := func() {
		t.Helper()
		data, err := json.Marshal(status)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "update", "installation.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	save()
	if target, err := dashboardInstalledTarget(root, "local", build); target != "" || err != nil {
		t.Fatalf("validating daemon triggered restart: %q %v", target, err)
	}
	status.Phase = updateinstall.Committed
	save()
	if target, err := dashboardInstalledTarget(root, "local", build); target != path || err != nil {
		t.Fatalf("committed target: %q %v", target, err)
	}
	if target, err := dashboardInstalledTarget(root, "remote", build); target != "" || err != nil {
		t.Fatalf("remote host triggered restart: %q %v", target, err)
	}
	status.Settings.Executable = status.Candidate
	save()
	if _, err := dashboardInstalledTarget(root, "local", build); err == nil {
		t.Fatal("staging path accepted")
	}
	status.Settings.Executable = path
	save()
	if err := os.WriteFile(path, []byte("different image"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := dashboardInstalledTarget(root, "local", build); err == nil {
		t.Fatal("different installed image accepted")
	}
}

func TestDashboardWatchPublishesVerifiedBuildWithSnapshot(t *testing.T) {
	host := HostRecord{ID: "local", Alias: "local", MeshIdentity: "identity"}
	build := release.Build{Digest: "new"}
	dial := reviewControlDial(t, func(host HostRecord, request protocol.Control) *protocol.Control {
		if request.Type == protocol.TypeHostInfo {
			return &protocol.Control{Type: protocol.TypeHostInfoResult, Host: &protocol.HostInfo{ID: host.ID, MeshIdentity: host.MeshIdentity, Build: &build}}
		}
		return reviewSnapshot(host)
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	watcher := NewStateWatcher(dial)
	view := StateView{Sections: map[string]ObservedSection{}}
	err := watcher.watchOnce(ctx, host, protocol.StateWatch{Topics: []string{protocol.TopicSessions}}, &view, func(state StateView) {
		if state.Build != build || state.Connection != StateReachable || state.LastReply.IsZero() {
			t.Fatalf("snapshot lost verified build: %+v", state)
		}
		cancel()
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := view.Apply(*reviewSnapshot(host), time.Now(), 0); err != nil {
		t.Fatal(err)
	}
	if view.Build != build {
		t.Fatal("resync discarded build")
	}
}

func TestDashboardFailedWatchSetupClearsRetainedBuild(t *testing.T) {
	host := HostRecord{ID: "local", Alias: "local", MeshIdentity: "identity"}
	build := release.Build{Digest: "new"}
	dial := reviewControlDial(t, func(host HostRecord, request protocol.Control) *protocol.Control {
		if request.Type == protocol.TypeHostInfo {
			return &protocol.Control{Type: protocol.TypeHostInfoResult, Host: &protocol.HostInfo{ID: host.ID, MeshIdentity: host.MeshIdentity, Build: &build}}
		}
		return &protocol.Control{Type: protocol.TypeError, Message: "state unavailable"}
	})
	watcher := NewStateWatcher(dial)
	view := StateView{Build: build, LastReply: time.Now(), Sections: map[string]ObservedSection{}}
	if err := watcher.watchOnce(t.Context(), host, protocol.StateWatch{Topics: []string{protocol.TopicSessions}}, &view, func(StateView) { t.Fatal("failed setup published success") }); err == nil {
		t.Fatal("setup failure lost")
	}
	if view.Build.Digest != "" {
		t.Fatal("failed setup retained restart-eligible build")
	}
}

func TestDashboardParentCancellationSkipsExec(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	restart := dashboardRestart{current: release.Build{Digest: "old"},
		installed: func(release.Build) (string, error) { return "/installed/mesh", nil },
		exec:      func(string, []string, []string) error { t.Fatal("exec after parent cancellation"); return nil },
	}
	input := DashboardInput{Watch: func(ctx context.Context, publish func(DashboardHostView)) error {
		publish(DashboardHostView{Host: DashboardHost{Local: true}, Build: release.Build{Digest: "new"}, Connection: StateReachable, LastReply: time.Now()})
		<-ctx.Done()
		return nil
	}}
	if err := restart.run(ctx, input, func(ctx context.Context, input DashboardInput) error {
		err := input.Watch(ctx, func(DashboardHostView) {})
		cancel()
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
