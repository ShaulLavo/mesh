package update

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/updatebootstrap"
	"github.com/shaul/mesh/internal/updateinstall"
	"github.com/shaul/mesh/internal/worker"
)

type adoptionNativeFixture struct {
	t         *testing.T
	root      string
	state     string
	install   string
	signer    *signingFixture
	proxy     *adoptionNativeProxy
	manifests map[string]release.Manifest
	images    map[string]string
	workers   []updateinstall.Worker
	session   string
}

func newAdoptionNativeFixture(t *testing.T, source string) *adoptionNativeFixture {
	t.Helper()
	root, err := os.MkdirTemp("", "an-")
	if err != nil {
		t.Fatal(err)
	}
	f := &adoptionNativeFixture{t: t, root: root, state: filepath.Join(root, "s"), install: filepath.Join(root, "installed", "mesh"), manifests: make(map[string]release.Manifest), images: make(map[string]string)}
	t.Cleanup(f.cleanup)
	for _, directory := range []string{f.state, filepath.Dir(f.install), filepath.Join(root, "home", ".config", "systemd", "user"), filepath.Join(root, "bin"), filepath.Join(root, "cache")} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	f.signer = newSigningFixture(t)
	f.proxy = newAdoptionNativeProxy(t, f.signer, root)
	pool, err := x509.SystemCertPool()
	if err != nil {
		t.Fatal(err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	t.Cleanup(transport.CloseIdleConnections)
	for _, version := range []string{"v0.1.196", source, "v0.1.249"} {
		f.acquire(version, client)
	}
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("MESH_STATE_DIR", f.state)
	t.Setenv("MESH_CONFIG_DIR", filepath.Join(root, "config"))
	t.Setenv("MESH_UPDATE_CACHE_DIR", filepath.Join(root, "cache"))
	t.Setenv("MESH_UPDATE_REQUIRED_MOUNT", "")
	t.Setenv("PATH", filepath.Join(root, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HTTPS_PROXY", f.proxy.server.URL)
	t.Setenv("HTTP_PROXY", f.proxy.server.URL)
	t.Setenv("NO_PROXY", "")
	t.Setenv("SSL_CERT_FILE", filepath.Join(root, "fixture-ca.pem"))
	t.Setenv("SSL_CERT_DIR", filepath.Join(root, "empty-certs"))
	f.write("bin/systemctl", []byte("#!/bin/sh\nroot="+adoptionShellQuote(root)+"\n"+adoptionNativeManager), true)
	f.write("daemon.source", []byte(f.install), false)
	f.write("home/.config/systemd/user/mesh.service", []byte("[Service]\nExecStart="+f.install+" daemon --tailnet-port=0 --ssh-port=0\nKillMode=process\n"), false)
	f.copyImage(f.images[source], f.install)
	f.manager("start", "mesh.service")
	f.awaitDaemon()
	f.createSession()
	_, err = updateinstall.PrepareHelper(t.Context(), updateinstall.HelperConfig{StateDir: f.state, Executable: f.images["v0.1.196"], ServiceDir: filepath.Join(root, "home", ".config", "systemd", "user"), Kind: "systemd"})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *adoptionNativeFixture) acquire(version string, client *http.Client) {
	f.t.Helper()
	if f.images[version] != "" {
		return
	}
	manifest, err := (release.Client{HTTPClient: client}).Manifest(f.t.Context(), version)
	if err != nil {
		f.t.Fatal(err)
	}
	artifact, err := manifest.Artifact(release.CurrentPlatform())
	if err != nil {
		f.t.Fatal(err)
	}
	archive, err := downloadAdoptionNativeArchive(f.t.Context(), client, release.OfficialBaseURL+"/download/"+version+"/"+artifact.Archive)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := release.VerifyDigest(archive, artifact.SHA256); err != nil {
		f.t.Fatal(err)
	}
	root, err := os.OpenRoot(f.root)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	name := version + "-mesh"
	file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		f.t.Fatal(err)
	}
	format, err := artifact.Format()
	if err != nil {
		f.t.Fatal(err)
	}
	executable, err := release.ReadExecutable(f.t.Context(), bytes.NewReader(archive), format, file)
	closeErr := file.Close()
	if err != nil || closeErr != nil || executable.SHA256 != artifact.BinarySHA256 {
		f.t.Fatalf("published archive extraction: %v %v", err, closeErr)
	}
	if err := root.Chmod(name, 0700); err != nil {
		f.t.Fatal(err)
	}
	f.images[version], f.manifests[version] = filepath.Join(f.root, name), manifest
	f.proxy.put(version+"/"+release.ManifestAssetName, mustHostJSON(manifest))
	f.proxy.put(version+"/"+artifact.Archive, archive)
}

func (f *adoptionNativeFixture) write(name string, data []byte, executable bool) {
	f.t.Helper()
	root, err := os.OpenRoot(f.root)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := root.WriteFile(name, data, 0600); err != nil {
		f.t.Fatal(err)
	}
	if executable {
		if err := root.Chmod(name, 0700); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *adoptionNativeFixture) copyImage(source, destination string) {
	f.t.Helper()
	command := exec.CommandContext(f.t.Context(), "cp", "--")
	command.Args = append(command.Args, source, destination)
	if output, err := command.CombinedOutput(); err != nil {
		f.t.Fatalf("copy native fixture image: %s %v", output, err)
	}
}

func (f *adoptionNativeFixture) manager(action, unit string) {
	f.t.Helper()
	command := exec.CommandContext(f.t.Context(), "systemctl", "--user")
	command.Args = append(command.Args, action, unit)
	if output, err := command.CombinedOutput(); err != nil {
		f.t.Fatalf("fixture manager %s: %s %v", action, output, err)
	}
}

func (f *adoptionNativeFixture) awaitDaemon() updatebootstrap.Observation {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(f.t.Context(), 5*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		observed, err := updatebootstrap.Inspect(ctx, f.state)
		if err == nil {
			return observed
		}
		time.Sleep(20 * time.Millisecond)
	}
	f.t.Fatal("native daemon did not become ready in its existing startup bound")
	return updatebootstrap.Observation{}
}

func (f *adoptionNativeFixture) localClient() (Client, Host) {
	f.t.Helper()
	host, key, err := identity.LoadPrivate(f.state)
	if err != nil {
		f.t.Fatal(err)
	}
	return Client{ID: host.ID, Key: key}, LocalHost(f.state, host.ID)
}

func (f *adoptionNativeFixture) createSession() {
	f.t.Helper()
	connection, err := net.DialTimeout("unix", filepath.Join(f.state, "daemon.sock"), time.Second)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
	if err := protocol.NewWriter(connection).WriteControlMsg(protocol.Control{Type: protocol.TypeCreate, RequestID: "native-adoption-worker", Command: []string{"/bin/cat"}, Cols: 80, Rows: 24}); err != nil {
		f.t.Fatal(err)
	}
	frame, err := protocol.NewReader(connection).ReadFrame()
	if err != nil {
		f.t.Fatal(err)
	}
	response, err := protocol.DecodeControl(frame.Payload)
	if err != nil || response.Type != protocol.TypeCreated || response.SessionID == "" {
		f.t.Fatalf("create native worker: %+v %v", response, err)
	}
	f.session = response.SessionID
	for range 100 {
		observed := f.awaitDaemon()
		if len(observed.Health.Workers) > 0 {
			f.workers = observed.Health.Workers
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatal("native worker did not become probeable")
}

func (f *adoptionNativeFixture) roundTrip() {
	f.t.Helper()
	connection, err := net.DialTimeout("unix", filepath.Join(f.state, "s", f.session, "sock"), time.Second)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
	writer, reader := protocol.NewWriter(connection), protocol.NewReader(connection)
	if err := writer.WriteControlMsg(protocol.Control{Type: protocol.TypeAttach, SessionID: f.session, Cols: 80, Rows: 24}); err != nil {
		f.t.Fatal(err)
	}
	id, err := protocol.NewSessionID(f.session)
	if err != nil {
		f.t.Fatal(err)
	}
	marker := []byte("adoption-worker-alive-" + strconv.FormatInt(time.Now().UnixNano(), 36))
	if err := writer.WriteInput(id, append(marker, '\n')); err != nil {
		f.t.Fatal(err)
	}
	var output []byte
	for len(output) < 4096 {
		frame, err := reader.ReadFrame()
		if err != nil {
			f.t.Fatal(err)
		}
		if frame.Kind == protocol.KindData {
			output = append(output, frame.Payload...)
		}
		if bytes.Contains(output, marker) {
			return
		}
	}
	f.t.Fatal("native worker did not retain terminal I/O")
}

func (f *adoptionNativeFixture) cleanup() {
	var pids []int
	for _, name := range []string{"helper.pid", "daemon.pid"} {
		data, err := readNativeFixtureFile(filepath.Join(f.root, name))
		if err != nil {
			continue
		}
		pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
		if pid > 0 {
			pids = append(pids, pid)
		}
	}
	for _, entry := range f.workers {
		pids = append(pids, entry.PID, entry.ShellPID)
	}
	if f.session != "" {
		meta, err := worker.ReadMeta(filepath.Join(f.state, "s", f.session))
		if err == nil {
			pids = append(pids, meta.PID)
		}
	}
	for _, pid := range pids {
		if pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !nativeFixtureProcessesLive(pids) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if nativeFixtureProcessesLive(pids) {
		f.t.Errorf("native fixture processes remain; retained %s", f.root)
		return
	}
	if err := os.RemoveAll(f.root); err != nil {
		f.t.Error(err)
	}
}

func nativeFixtureProcessesLive(pids []int) bool {
	for _, pid := range pids {
		if pid <= 0 {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		_, tail, ok := strings.Cut(string(data), ") ")
		if err != nil || !ok || !strings.HasPrefix(tail, "Z ") {
			return true
		}
	}
	return false
}

func readNativeFixtureFile(path string) ([]byte, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, hostError(err)
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile(filepath.Base(path))
	return data, hostError(err)
}

const adoptionNativeManager = `set -eu
test "$1" = --user
shift
action=$1
shift
mkdir -p "$root/home/.config/systemd/user"
pid_value() { test ! -f "$1" || cat "$1"; }
process_running() {
  test -n "$1" && test "$1" -gt 0 || return 1
  status=$(ps -o stat= -p "$1" 2>/dev/null || true)
  case "$status" in ''|Z*) return 1 ;; esac
  return 0
}
stop_process() {
  pid=$(pid_value "$1")
  if test -n "$pid" && test "$pid" -gt 0; then kill "$pid" 2>/dev/null || true; fi
  count=0
  while process_running "$pid"; do
    count=$((count + 1))
    test "$count" -lt 500 || exit 24
    sleep 0.01
  done
  printf '0\n' > "$1"
}
start_daemon() {
  pid=$(pid_value "$root/daemon.pid")
  if process_running "$pid"; then return; fi
  image=$(cat "$root/daemon.source")
  if test -f "$root/home/.config/systemd/user/mesh.service.d/90-mesh-managed.conf"; then image="$root/home/.local/bin/mesh"; fi
  if test -f "$root/fail-candidate" && test -f "$root/candidate.sha"; then
    image_hash=$(sha256sum "$image"); image_hash=${image_hash%% *}
    if test "$image_hash" = "$(cat "$root/candidate.sha")"; then exit 17; fi
  fi
  if test -f "$root/fail-managed" && test "$image" = "$root/home/.local/bin/mesh"; then exit 18; fi
  env -u SHLVL -u _ "$image" daemon --tailnet-port=0 --ssh-port=0 </dev/null >>"$root/daemon.log" 2>&1 &
  printf '%s\n' "$!" > "$root/daemon.pid"
}
start_helper() {
  pid=$(pid_value "$root/helper.pid")
  if process_running "$pid"; then return; fi
  if test -f "$root/hold-old-helper"; then return; fi
  "$root/s/update/helper/current" update-helper --state-dir "$root/s" </dev/null >>"$root/helper.log" 2>&1 &
  printf '%s\n' "$!" > "$root/helper.pid"
  touch "$root/helper.loaded"
}
case "$action" in
whoami)
  test "$1" = "$(pid_value "$root/daemon.pid")"
  printf 'mesh.service\n'
  ;;
show)
  unit=mesh.service
  value=false
  properties=
  for argument in "$@"; do
    case "$argument" in
      mesh-update-helper.service) unit=$argument ;;
      --value) value=true ;;
      --property=*) properties="$properties ${argument#--property=}" ;;
    esac
  done
  for property in $properties; do
    case "$property" in
      MainPID)
        if test "$unit" = mesh.service; then result=$(pid_value "$root/daemon.pid"); else result=$(pid_value "$root/helper.pid"); fi
        result=${result:-0}
        ;;
      LoadState)
        result=not-found
        if test -f "$root/helper.loaded"; then result=loaded; fi
        ;;
      KillMode) result=process ;;
      FragmentPath) result="$root/home/.config/systemd/user/mesh.service" ;;
      DropInPaths)
        result=
        if test -f "$root/home/.config/systemd/user/mesh.service.d/90-mesh-managed.conf"; then result="$root/home/.config/systemd/user/mesh.service.d/90-mesh-managed.conf"; fi
        ;;
      *) exit 23 ;;
    esac
    if test "$value" = true; then printf '%s\n' "$result"; else printf '%s=%s\n' "$property" "$result"; fi
  done
  ;;
daemon-reload) ;;
enable) start_helper ;;
restart) stop_process "$root/helper.pid"; start_helper ;;
start)
  if test "$1" = mesh.service; then start_daemon; else start_helper; fi
  ;;
stop)
  if test "$1" = mesh.service; then stop_process "$root/daemon.pid"; else stop_process "$root/helper.pid"; fi
  ;;
disable) stop_process "$root/helper.pid"; rm -f "$root/helper.loaded" ;;
*) exit 22 ;;
esac
`
