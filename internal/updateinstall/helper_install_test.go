package updateinstall

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/release"
)

type freshHelperFixture struct {
	cfg      HelperConfig
	source   release.Manifest
	commands string
	active   string
}

// These unit fixtures simulate manager state. Native installed proofs use the
// real HelperProcessProbe and service manager in Unit 8.
func newFreshHelperFixture(t *testing.T, kind string) freshHelperFixture {
	t.Helper()
	root := t.TempDir()
	f := freshHelperFixture{cfg: HelperConfig{StateDir: filepath.Join(root, "state"), Executable: filepath.Join(root, "mesh"), ServiceDir: filepath.Join(root, "services"), Kind: kind, Domain: "gui/501"}, commands: filepath.Join(root, "commands"), active: filepath.Join(root, "active")}
	binary := testExecutable("v0.1.400")
	if err := os.WriteFile(f.cfg.Executable, binary, 0700); err != nil { //nolint:gosec // executable fixture in an isolated test directory
		t.Fatal(err)
	}
	f.source = release.Manifest{Schema: 1, Version: "v0.1.400", Commit: strings.Repeat("a", 40), Compatibility: release.Compatibility{StateReadMin: 7, StateReadMax: 7, StateWrite: 7, WorkerMin: 1, WorkerMax: 1, WorkerWrite: 1, JournalVersion: release.CurrentJournalVersion}}
	for _, platform := range []release.Platform{{OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"}, {OS: "darwin", Arch: "arm64"}} {
		f.source.Artifacts = append(f.source.Artifacts, release.Artifact{Platform: platform, Archive: "mesh_" + platform.OS + "_" + platform.Arch + ".tar.gz", SHA256: strings.Repeat("b", 64), BinarySHA256: digestBytes(binary)})
	}
	tools := filepath.Join(root, "tools")
	if err := os.MkdirAll(tools, 0700); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\nset -eu\nprintf '%%s\\n' \"$*\" >> %q\ncase \"$*\" in\n*disable*|*stop*|bootout*) rm -f %q ;;\n*enable*|*restart*|bootstrap*) touch %q ;;\nesac\n", f.commands, f.active, f.active)
	for _, tool := range []string{"systemctl", "launchctl"} {
		if err := os.WriteFile(filepath.Join(tools, tool), []byte(script), 0700); err != nil { //nolint:gosec // isolated service manager fixture
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	f.cfg.State = func(context.Context) (HelperProcessState, error) {
		_, err := os.Stat(f.active)
		if errors.Is(err, os.ErrNotExist) {
			return HelperProcessState{}, nil
		}
		if err != nil {
			return HelperProcessState{}, fmt.Errorf("read simulated helper state: %w", err)
		}
		return HelperProcessState{Loaded: true, PID: 123}, nil
	}
	f.cfg.Probe = func(ctx context.Context, installed HelperInstallation) (int, error) {
		if _, err := os.Stat(f.active); err != nil {
			return 0, fmt.Errorf("simulated helper is stopped: %w", err)
		}
		return fixtureHelperProbe(ctx, installed)
	}
	return f
}

func TestFreshHelperStartsWithoutJournalOrAuthority(t *testing.T) {
	for _, kind := range []string{promotionSystemd, promotionLaunchd} {
		t.Run(kind, func(t *testing.T) { checkFreshHelperStarts(t, kind) })
	}
}

func checkFreshHelperStarts(t *testing.T, kind string) {
	t.Helper()
	f := newFreshHelperFixture(t, kind)
	installed, err := EnsureInstalledHelper(t.Context(), f.cfg, f.source)
	if err != nil {
		t.Fatal(err)
	}
	if err := stableHelperProcess(t.Context(), f.cfg, installed); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{journalPath(f.cfg.StateDir), filepath.Join(f.cfg.StateDir, "updates", "host-policy.json"), filepath.Join(f.cfg.StateDir, "updates", "administrators.json"), freshHelperPath(f.cfg.StateDir), helperActivationRecord(f.cfg.StateDir)} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("fresh install created transaction/authority or retained pending work %s: %v", path, err)
		}
	}
	before, err := os.ReadFile(f.commands)
	if err != nil {
		t.Fatal(err)
	}
	again, err := EnsureInstalledHelper(t.Context(), f.cfg, f.source)
	if err != nil || again != installed {
		t.Fatalf("repeat installation = %+v, %v", again, err)
	}
	after, err := os.ReadFile(f.commands)
	if err != nil || string(before) != string(after) {
		t.Fatalf("repeat restarted ready helper: %s, %v", after, err)
	}
}

func TestFreshHelperReadinessFailureRollsBackAndRetries(t *testing.T) {
	f := newFreshHelperFixture(t, promotionSystemd)
	probe := f.cfg.Probe
	f.cfg.Probe = func(context.Context, HelperInstallation) (int, error) { return 0, errors.New("no mapped image") }
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, err := EnsureInstalledHelper(ctx, f.cfg, f.source); err == nil {
		t.Fatal("configured manager counted as running helper")
	}
	// A live unverified PID cannot be stopped as ours. Once the manager reports
	// it stopped, the durable checkpoint lets the same install retry safely.
	if err := os.Remove(f.active); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	f.cfg.Probe = probe
	installed, err := EnsureInstalledHelper(t.Context(), f.cfg, f.source)
	if err != nil {
		t.Fatal(err)
	}
	if err := stableHelperProcess(t.Context(), f.cfg, installed); err != nil {
		t.Fatal(err)
	}
}

func TestFreshHelperRejectsUnverifiedBytesAndCompetingAdoption(t *testing.T) {
	for _, variant := range []string{"digest", "modified", "adoption", "unmanaged", "probe"} {
		t.Run(variant, func(t *testing.T) { checkFreshHelperRefusal(t, variant) })
	}
}

func TestFreshHelperFailedStartRemovesOwnedFiles(t *testing.T) {
	f := newFreshHelperFixture(t, promotionSystemd)
	state, probe := f.cfg.State, f.cfg.Probe
	f.cfg.State = func(context.Context) (HelperProcessState, error) { return HelperProcessState{}, nil }
	f.cfg.Probe = func(context.Context, HelperInstallation) (int, error) {
		return 0, errors.New("manager never started helper")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, err := EnsureInstalledHelper(ctx, f.cfg, f.source); err == nil {
		t.Fatal("failed start was accepted")
	}
	for _, path := range []string{helperRecord(f.cfg.StateDir), filepath.Join(f.cfg.ServiceDir, "mesh-update-helper.service"), filepath.Join(transactionDir(f.cfg.StateDir), "helper", "current"), freshHelperPath(f.cfg.StateDir)} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed start retained owned file %s: %v", path, err)
		}
	}
	f.cfg.State, f.cfg.Probe = state, probe
	if _, err := EnsureInstalledHelper(t.Context(), f.cfg, f.source); err != nil {
		t.Fatalf("retry after rollback: %v", err)
	}
}

func TestFreshHelperDoesNotReplaceAnOlderUnjournaledHelper(t *testing.T) {
	f := newFreshHelperFixture(t, promotionSystemd)
	prior, err := EnsureInstalledHelper(t.Context(), f.cfg, f.source)
	if err != nil {
		t.Fatal(err)
	}
	binary := testExecutable("v0.1.401")
	if err := os.WriteFile(f.cfg.Executable, binary, 0700); err != nil { //nolint:gosec // isolated executable fixture
		t.Fatal(err)
	}
	f.source.Version = "v0.1.401"
	for i := range f.source.Artifacts {
		f.source.Artifacts[i].BinarySHA256 = digestBytes(binary)
	}
	if _, err := EnsureInstalledHelper(t.Context(), f.cfg, f.source); err == nil || !strings.Contains(err.Error(), "requires adoption") {
		t.Fatalf("older unjournaled helper was replaced: %v", err)
	}
	got, err := InspectHelper(t.Context(), f.cfg)
	if err != nil || got != prior {
		t.Fatalf("old helper receipt/image changed: %+v, %v", got, err)
	}
}

func checkFreshHelperRefusal(t *testing.T, variant string) {
	t.Helper()
	f := newFreshHelperFixture(t, promotionSystemd)
	switch variant {
	case "digest":
		f.source.Artifacts[0].BinarySHA256 = strings.Repeat("c", 64)
		f.source.Artifacts[1].BinarySHA256 = strings.Repeat("c", 64)
		f.source.Artifacts[2].BinarySHA256 = strings.Repeat("c", 64)
	case "modified":
		f.source.Commit = strings.Repeat("d", 40)
	case "adoption":
		if err := atomicWrite(filepath.Join(f.cfg.StateDir, "updates", "host-adoption.json"), []byte("owned by adoption"), 0600); err != nil {
			t.Fatal(err)
		}
	case "unmanaged":
		if err := atomicWrite(filepath.Join(f.cfg.ServiceDir, "mesh-update-helper.service"), []byte("unmanaged service"), 0600); err != nil {
			t.Fatal(err)
		}
	case "probe":
		f.cfg.Probe = nil
	}
	if _, err := EnsureInstalledHelper(t.Context(), f.cfg, f.source); err == nil {
		t.Fatal("invalid initial installation was accepted")
	}
	if _, err := os.Stat(f.commands); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid initial installation reached manager actions: %v", err)
	}
}
