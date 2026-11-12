package install

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const installFixtureBinary = `#!/bin/sh
set -eu
case "$*" in
  'device approve '*) printf '{"changed":false}\n' ;;
  'daemon prepare-updates '*)
    printf 'helper %s\n' "$MESH_STATE_DIR" >> "$INSTALL_LOG"
    test "${FAIL_HELPER:-0}" != 1 ;;
  'daemon installation-check '*)
    printf 'check %s\n' "$MESH_STATE_DIR" >> "$INSTALL_LOG"
    test "${FAIL_CHECK:-0}" != 1 ;;
esac
`

type installerDefaultsFixture struct {
	home, tools, log, active string
	goos                     string
}

func newInstallerDefaultsFixture(t *testing.T, goos string) installerDefaultsFixture {
	t.Helper()
	root := t.TempDir()
	f := installerDefaultsFixture{home: filepath.Join(root, "home"), tools: filepath.Join(root, "tools"), log: filepath.Join(root, "log"), active: filepath.Join(root, "active"), goos: goos}
	if err := os.MkdirAll(f.tools, 0700); err != nil {
		t.Fatal(err)
	}
	f.write(t, filepath.Join(f.tools, "id"), "#!/bin/sh\ncase \"$1\" in -u) echo 501 ;; -un) echo fixture ;; esac\n")
	f.write(t, filepath.Join(f.tools, "loginctl"), "#!/bin/sh\ncase \"$*\" in *--value*) echo yes ;; *) echo Linger=yes ;; esac\n")
	f.write(t, filepath.Join(f.tools, "systemctl"), `#!/bin/sh
set -eu
case "$*" in
  '--user is-active '*) test -f "$INSTALL_ACTIVE" ;;
  '--user start mesh.service'|'--user restart mesh.service')
    printf 'daemon\n' >> "$INSTALL_LOG"; touch "$INSTALL_ACTIVE" ;;
esac
`)
	f.write(t, filepath.Join(f.tools, "launchctl"), `#!/bin/sh
set -eu
case "$1" in
  print)
    case "$2" in
      gui/501|user/501) exit 0 ;;
      gui/501/dev.shaulavo.mesh) test -f "$INSTALL_ACTIVE" ;;
      *) exit 113 ;;
    esac ;;
  bootstrap) printf 'daemon\n' >> "$INSTALL_LOG"; touch "$INSTALL_ACTIVE" ;;
  bootout) rm -f "$INSTALL_ACTIVE" ;;
esac
`)
	return f
}

func (f installerDefaultsFixture) write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0700); err != nil { //nolint:gosec // executable installer fixture in an isolated directory
		t.Fatal(err)
	}
}

func (f installerDefaultsFixture) run(t *testing.T, failure string) (string, error) {
	t.Helper()
	service, err := RenderService(f.goos, ServiceOptions{DaemonPort: 8444, SSHPort: 2223, WebSocketPath: "/custom"})
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(filepath.Dir(f.home), "upload")
	f.write(t, source, installFixtureBinary)
	script, _ := Script(f.goos)
	run := exec.CommandContext(t.Context(), "/bin/sh", "-s", "--", source, "8444", "2223", "/custom", base64.StdEncoding.EncodeToString([]byte("fixture-key")), base64.StdEncoding.EncodeToString([]byte(service))) //nolint:gosec // embedded installer and isolated test arguments
	run.Stdin = strings.NewReader(script)
	run.Env = append(os.Environ(), "HOME="+f.home, "PATH="+f.tools+string(os.PathListSeparator)+os.Getenv("PATH"), "INSTALL_LOG="+f.log, "INSTALL_ACTIVE="+f.active, "FAIL_HELPER=0", "FAIL_CHECK=0", failure+"=1")
	output, err := run.CombinedOutput()
	return string(output), err
}

func TestInstallersPrepareHelperBeforeFirstDaemonStart(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) { checkInstallerFirstStart(t, goos) })
	}
}

func checkInstallerFirstStart(t *testing.T, goos string) {
	t.Helper()
	f := newInstallerDefaultsFixture(t, goos)
	if output, err := f.run(t, "NO_FAILURE"); err != nil {
		t.Fatalf("install: %v\n%s", err, output)
	}
	log, err := os.ReadFile(f.log)
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(f.home, ".local", "state", "mesh")
	want := "helper " + state + "\ndaemon\ncheck " + state + "\n"
	if string(log) != want {
		t.Fatalf("first-start ordering/state changed: %q, want %q", log, want)
	}
	if _, err := os.Stat(filepath.Join(state, "updates", "host-policy.json")); !os.IsNotExist(err) {
		t.Fatalf("installer wrote over policy defaults: %v", err)
	}
}

func TestInstallersHoldDaemonUntilHelperReadyAndRetry(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) { checkInstallerHelperFailure(t, goos) })
	}
}

func checkInstallerHelperFailure(t *testing.T, goos string) {
	t.Helper()
	f := newInstallerDefaultsFixture(t, goos)
	if output, err := f.run(t, "FAIL_HELPER"); err == nil || !strings.Contains(output, "cannot prepare") {
		t.Fatalf("failed helper was accepted: %v\n%s", err, output)
	}
	if _, err := os.Stat(f.active); !os.IsNotExist(err) {
		t.Fatalf("daemon started before helper readiness: %v", err)
	}
	if output, err := f.run(t, "NO_FAILURE"); err != nil {
		t.Fatalf("retry: %v\n%s", err, output)
	}
	if _, err := os.Stat(f.active); err != nil {
		t.Fatal(err)
	}
}

func TestInstallersRetainPendingMarkerWhenHelperCheckFails(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) { checkInstallerReadinessMarker(t, goos) })
	}
}

func checkInstallerReadinessMarker(t *testing.T, goos string) {
	t.Helper()
	f := newInstallerDefaultsFixture(t, goos)
	if output, err := f.run(t, "FAIL_CHECK"); err == nil {
		t.Fatalf("failed installation check was accepted: %s", output)
	}
	marker := filepath.Join(f.home, ".local", "state", "mesh", "activation.pending")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("failed helper check cleared retry marker: %v", err)
	}
	if output, err := f.run(t, "NO_FAILURE"); err != nil {
		t.Fatalf("check retry: %v\n%s", err, output)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("successful retry retained marker: %v", err)
	}
}

func (f installerDefaultsFixture) rootRelease(t *testing.T) string {
	t.Helper()
	directory := filepath.Join(filepath.Dir(f.home), "release")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	architecture := "amd64"
	if f.goos == "darwin" {
		architecture = "arm64"
	}
	name := "mesh_" + f.goos + "_" + architecture + ".tar.gz"
	path := filepath.Join(directory, name)
	file, err := os.Create(path) //nolint:gosec // path is an archive fixture inside the test directory
	if err != nil {
		t.Fatal(err)
	}
	compressed := gzip.NewWriter(file)
	archive := tar.NewWriter(compressed)
	if err := archive.WriteHeader(&tar.Header{Name: "mesh", Size: int64(len(installFixtureBinary)), Mode: 0700}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write([]byte(installFixtureBinary)); err != nil {
		t.Fatal(err)
	}
	for _, close := range []func() error{archive.Close, compressed.Close, file.Close} {
		if err := close(); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path) //nolint:gosec // checksum the archive fixture just created
	if err != nil {
		t.Fatal(err)
	}
	f.write(t, filepath.Join(directory, "checksums.txt"), fmt.Sprintf("%x  %s\n", sha256.Sum256(data), name))
	osName := "Linux"
	if f.goos == "darwin" {
		osName = "Darwin"
	}
	f.write(t, filepath.Join(f.tools, "uname"), fmt.Sprintf("#!/bin/sh\ncase \"$1\" in -s) echo %s ;; -m) echo %s ;; esac\n", osName, architecture))
	return directory
}

func (f installerDefaultsFixture) runRoot(t *testing.T, failure string, clientOnly bool) (string, error) {
	t.Helper()
	release := f.rootRelease(t)
	assets, err := filepath.Abs("assets")
	if err != nil {
		t.Fatal(err)
	}
	run := exec.CommandContext(t.Context(), "/bin/sh", "../install.sh")
	run.Env = append(os.Environ(), "HOME="+f.home, "PATH="+f.tools+string(os.PathListSeparator)+os.Getenv("PATH"), "INSTALL_LOG="+f.log, "INSTALL_ACTIVE="+f.active, "MESH_INSTALL_YES=1", "MESH_VERSION=v0.1.400", "MESH_RELEASE_DIR="+release, "MESH_SERVICE_ASSET_DIR="+assets, "FAIL_HELPER=0", "FAIL_CHECK=0", failure+"=1", fmt.Sprintf("MESH_SKIP_SERVICE=%d", boolToInstallInt(clientOnly)))
	output, err := run.CombinedOutput()
	return string(output), err
}

func boolToInstallInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestLiveRootInstallerDefersHelperPreparationUntilSignedPublication(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) { checkLiveRootInstaller(t, goos) })
	}
}

func checkLiveRootInstaller(t *testing.T, goos string) {
	t.Helper()
	f := newInstallerDefaultsFixture(t, goos)
	if output, err := f.runRoot(t, "FAIL_HELPER", false); err != nil {
		t.Fatalf("live root installer requires unpublished helper API: %v\n%s", err, output)
	}
	log, err := os.ReadFile(f.log)
	if err != nil {
		t.Fatal(err)
	}
	if string(log) != "daemon\n" {
		t.Fatalf("live root installer reached unpublished helper API: %q", log)
	}
	if _, err := os.Stat(f.active); err != nil {
		t.Fatalf("live root installer did not start daemon: %v", err)
	}
}

func TestRootClientOnlyInstallerDoesNotPrepareHelper(t *testing.T) {
	f := newInstallerDefaultsFixture(t, "linux")
	if output, err := f.runRoot(t, "FAIL_HELPER", true); err != nil {
		t.Fatalf("client-only install: %v\n%s", err, output)
	}
	if _, err := os.Stat(f.log); !os.IsNotExist(err) {
		t.Fatalf("client-only install reached helper or daemon: %v", err)
	}
}
