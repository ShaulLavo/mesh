package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/updateinstall"
)

type localHelperFixture struct {
	cfg      updateinstall.HelperConfig
	commands string
}

func newLocalHelperFixture(t *testing.T) localHelperFixture {
	t.Helper()
	root := t.TempDir()
	f := localHelperFixture{cfg: updateinstall.HelperConfig{StateDir: filepath.Join(root, "state"),
		Executable: filepath.Join(root, "mesh"), ServiceDir: filepath.Join(root, "services"), Kind: "systemd"},
		commands: filepath.Join(root, "commands")}
	tools := filepath.Join(root, "tools")
	if err := os.MkdirAll(tools, 0700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$MESH_LOCAL_HELPER_COMMANDS\"\n"
	if err := writeLocalHelperExecutable(filepath.Join(tools, "systemctl"), []byte(script)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MESH_LOCAL_HELPER_COMMANDS", f.commands)
	f.cfg.Probe = func(ctx context.Context, installed updateinstall.HelperInstallation) (int, error) {
		if _, err := os.Stat(f.commands); err != nil {
			return 0, fmt.Errorf("fixture helper is stopped: %w", err)
		}
		if err := release.VerifyExecutable(ctx, installed.Executable, installed.Digest); err != nil {
			return 0, fmt.Errorf("fixture running helper image: %w", err)
		}
		return 123, nil
	}
	return f
}

func (f localHelperFixture) executable(t *testing.T, version string) string {
	t.Helper()
	platform := release.CurrentPlatform()
	binary := fmt.Sprintf(`#!/bin/sh
if [ "$1" = update-helper ]; then exit 0; fi
digest=$(shasum -a 256 "$0")
printf '{"version":"%s","commit":"%s","platform":{"os":"%s","arch":"%s"},"digest":"%%s","stateVersion":7,"workerProtocol":1,"updateProtocol":1}\n' "${digest%%%% *}"
`, version, strings.Repeat("a", 40), platform.OS, platform.Arch)
	if err := writeLocalHelperExecutable(f.cfg.Executable, []byte(binary)); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(binary))
	return hex.EncodeToString(digest[:])
}

func writeLocalHelperExecutable(path string, data []byte) error {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("open local helper fixture directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	name := filepath.Base(path)
	if err = root.WriteFile(name, data, 0600); err != nil {
		return fmt.Errorf("write local helper fixture: %w", err)
	}
	if err = root.Chmod(name, 0700); err != nil {
		return fmt.Errorf("make local helper fixture executable: %w", err)
	}
	return nil
}

func (f localHelperFixture) journal(t *testing.T, phase updateinstall.Phase, version, digest string) {
	t.Helper()
	manifest := testManifest()
	manifest.Version = version
	for i := range manifest.Artifacts {
		manifest.Artifacts[i].BinarySHA256 = digest
	}
	for i := range manifest.Compatibility.Transitions {
		manifest.Compatibility.Transitions[i].ToDigest = digest
	}
	status := updateinstall.Status{Schema: 1, Phase: phase, Request: updateinstall.Request{Manifest: manifest},
		Settings: updateinstall.Settings{StateDir: f.cfg.StateDir, Executable: f.cfg.Executable,
			CacheDir: filepath.Join(f.cfg.StateDir, "cache"), ClientOnly: true}}
	data, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(f.cfg.StateDir, "update", "installation.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureLocalHelperFirstInstallThenCommittedPromotion(t *testing.T) {
	f := newLocalHelperFixture(t)
	f.executable(t, "v0.1.170")
	first, err := ensureLocalHelper(t.Context(), f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	digest := f.executable(t, "v0.1.171")
	f.journal(t, updateinstall.Committed, "v0.1.171", digest)
	got, err := ensureLocalHelper(t.Context(), f.cfg)
	if err != nil || got.Digest != digest || got.Digest == first.Digest {
		t.Fatalf("committed helper promotion = %+v, %v", got, err)
	}
}

func TestEnsureLocalHelperLeavesOwnedTransactionUnchanged(t *testing.T) {
	for _, phase := range []updateinstall.Phase{updateinstall.Staged, updateinstall.Granted, updateinstall.Activating, updateinstall.RolledBack} {
		t.Run(string(phase), func(t *testing.T) { checkLocalHelperOwnedTransaction(t, phase) })
	}
}

func TestEnsureLocalHelperStartsStoppedNewerHelper(t *testing.T) {
	f := newLocalHelperFixture(t)
	f.executable(t, "v0.1.180")
	newer, err := updateinstall.PrepareHelper(t.Context(), f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	digest := f.executable(t, "v0.1.170")
	f.journal(t, updateinstall.Committed, "v0.1.170", digest)
	got, err := ensureLocalHelper(t.Context(), f.cfg)
	if err != nil || got != newer {
		t.Fatalf("newer adoption helper = %+v, %v", got, err)
	}
	commands, err := os.ReadFile(f.commands)
	if err != nil || !strings.Contains(string(commands), "restart") {
		t.Fatalf("stopped helper was not started = %q, %v", commands, err)
	}
}

func checkLocalHelperOwnedTransaction(t *testing.T, phase updateinstall.Phase) {
	t.Helper()
	f := newLocalHelperFixture(t)
	f.executable(t, "v0.1.170")
	prior, err := updateinstall.PrepareHelper(t.Context(), f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	digest := f.executable(t, "v0.1.171")
	f.journal(t, phase, "v0.1.171", digest)
	got, err := ensureLocalHelper(t.Context(), f.cfg)
	if err != nil || got != prior {
		t.Fatalf("owned helper = %+v, %v", got, err)
	}
	if _, err = os.Stat(f.commands); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned helper service was changed = %v", err)
	}
}

func TestEnsureLocalHelperCommittedBindingMismatchFailsClosed(t *testing.T) {
	f := newLocalHelperFixture(t)
	digest := f.executable(t, "v0.1.170")
	if _, err := updateinstall.PrepareHelper(t.Context(), f.cfg); err != nil {
		t.Fatal(err)
	}
	f.journal(t, updateinstall.Committed, "v0.1.170", digest)
	f.executable(t, "v0.1.171")
	if _, err := ensureLocalHelper(t.Context(), f.cfg); !errors.Is(err, release.ErrExecutableChecksumMismatch) {
		t.Fatalf("committed mismatch = %v", err)
	}
	if _, err := os.Stat(f.commands); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("committed mismatch changed service = %v", err)
	}
}

func TestEnsureLocalHelperMalformedJournalFailsClosed(t *testing.T) {
	f := newLocalHelperFixture(t)
	f.executable(t, "v0.1.170")
	if _, err := updateinstall.PrepareHelper(t.Context(), f.cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.cfg.StateDir, "update", "installation.json"), []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureLocalHelper(t.Context(), f.cfg); err == nil {
		t.Fatal("malformed journal accepted")
	}
	if _, err := os.Stat(f.commands); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("malformed journal changed service = %v", err)
	}
}
