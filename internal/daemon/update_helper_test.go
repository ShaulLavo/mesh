package daemon

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/updateinstall"
)

func helperReadinessSource(t *testing.T, executable, version string) release.Manifest {
	t.Helper()
	platform := release.CurrentPlatform()
	hash := "sha256sum"
	if runtime.GOOS == "darwin" {
		hash = "shasum -a 256"
	}
	image := []byte(fmt.Sprintf("#!/bin/sh\nset -eu\ndigest=$(%s \"$0\")\ndigest=${digest%%%% *}\nprintf '{\"version\":\"%s\",\"commit\":\"%s\",\"digest\":\"%%s\",\"platform\":{\"os\":\"%s\",\"arch\":\"%s\"},\"stateVersion\":7,\"workerProtocol\":1,\"updateProtocol\":1}\\n' \"$digest\"\n", hash, version, strings.Repeat("a", 40), platform.OS, platform.Arch))
	root, err := os.OpenRoot(filepath.Dir(executable))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := root.WriteFile(filepath.Base(executable), image, 0700); err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(image))
	source := release.Manifest{Schema: 1, Version: version, Commit: strings.Repeat("a", 40), Compatibility: release.Compatibility{StateReadMin: 7, StateReadMax: 7, StateWrite: 7, WorkerMin: 1, WorkerMax: 1, WorkerWrite: 1, JournalVersion: release.CurrentJournalVersion}}
	for _, target := range []release.Platform{{OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"}, {OS: "darwin", Arch: "arm64"}} {
		source.Artifacts = append(source.Artifacts, release.Artifact{Platform: target, Archive: "mesh_" + target.OS + "_" + target.Arch + ".tar.gz", SHA256: strings.Repeat("b", 64), BinarySHA256: digest})
	}
	return source
}

func TestHostFreshReadinessPreservesNewerHelper(t *testing.T) {
	root := t.TempDir()
	files, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = files.Close() }()
	if err := files.WriteFile("systemctl", []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	cfg := updateinstall.HelperConfig{StateDir: filepath.Join(root, "state"), Executable: filepath.Join(root, "mesh"), ServiceDir: filepath.Join(root, "services"), Kind: "systemd"}
	cfg.Probe = func(ctx context.Context, installed updateinstall.HelperInstallation) (int, error) {
		return 123, release.VerifyExecutable(ctx, installed.Executable, installed.Digest)
	}
	cfg.State = func(context.Context) (updateinstall.HelperProcessState, error) {
		return updateinstall.HelperProcessState{}, nil
	}
	newer := helperReadinessSource(t, cfg.Executable, "v0.1.401")
	installed, err := updateinstall.EnsureInstalledHelper(t.Context(), cfg, newer)
	if err != nil {
		t.Fatal(err)
	}
	older := helperReadinessSource(t, cfg.Executable, "v0.1.400")
	if err := ensureHostHelper(t.Context(), cfg, older); err != nil {
		t.Fatalf("fresh readiness rejected the preserved newer helper: %v", err)
	}
	retained, err := updateinstall.InspectHelper(t.Context(), cfg)
	if err != nil || retained != installed {
		t.Fatalf("first hop changed the newer helper: %+v, %v", retained, err)
	}
	if err := inspectHostHelper(t.Context(), cfg, older); err == nil {
		t.Fatal("unfinished-transaction readiness accepted a different helper image")
	}
}
