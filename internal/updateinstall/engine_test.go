package updateinstall

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/updategate"
)

type fixture struct {
	t       *testing.T
	engine  *Engine
	request Request
	manager *processService
	server  *httptest.Server
	worker  *exec.Cmd
	input   io.WriteCloser
	output  io.ReadCloser
}

type processService struct {
	executable, stateDir, healthPath string
	process                          *exec.Cmd
	failCandidate                    bool
	stops, starts                    int
	worker                           Worker
}

func (s *processService) Stop(ctx context.Context) error {
	if err := updategate.Check(s.stateDir); !errors.Is(err, updategate.ErrUpdating) {
		return fmt.Errorf("daemon stopped without persisted gate: %v", err)
	}
	s.stops++
	if s.process == nil {
		return nil
	}
	_ = s.process.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- s.process.Wait() }()
	select {
	case <-done:
		s.process = nil
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *processService) Start(ctx context.Context) error {
	if s.process != nil {
		return nil
	}
	s.starts++
	s.process = exec.Command(s.executable, "daemon", s.healthPath) //nolint:gosec // isolated executable fixture under the test directory
	if err := s.process.Start(); err != nil {
		s.process = nil
		return err
	}
	return nil
}

func (s *processService) probe(ctx context.Context) (Health, error) {
	data, err := os.ReadFile(s.healthPath)
	if err != nil {
		return Health{}, err
	}
	var build release.Build
	if err = json.Unmarshal(data, &build); err != nil {
		return Health{}, err
	}
	if s.failCandidate && build.Version == "v0.2.0" {
		return Health{}, errors.New("candidate reports an unhealthy worker connection")
	}
	if err = syscall.Kill(s.worker.PID, 0); err != nil {
		return Health{}, err
	}
	return Health{HostID: "test-host", Build: build, Workers: []Worker{s.worker}}, nil
}

func testExecutable(version string) []byte {
	platform := release.CurrentPlatform()
	return []byte(fmt.Sprintf(`#!/bin/sh
set -eu
if [ "$1" = version ]; then
  if command -v sha256sum >/dev/null 2>&1; then digest=$(sha256sum "$0"); else digest=$(shasum -a 256 "$0"); fi
  digest=${digest%%%% *}
  printf '{"version":"%s","commit":"%s","digest":"%%s","platform":{"os":"%s","arch":"%s"},"stateVersion":7,"workerProtocol":1,"updateProtocol":1}\n' "$digest"
  exit 0
fi
health=$2
"$0" version --json > "$health"
trap 'rm -f "$health"; exit 0' TERM INT
while :; do sleep 0.05; done
`, version, strings.Repeat("a", 40), platform.OS, platform.Arch))
}

func digestBytes(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

func archiveBytes(t *testing.T, binary []byte) []byte {
	t.Helper()
	var data bytes.Buffer
	compressed := gzip.NewWriter(&data)
	archive := tar.NewWriter(compressed)
	if err := archive.WriteHeader(&tar.Header{Name: "mesh", Mode: 0755, Size: int64(len(binary))}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write(binary); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	old, candidate := testExecutable("v0.1.0"), testExecutable("v0.2.0")
	executable := filepath.Join(root, "mesh")
	if err := os.WriteFile(executable, old, 0755); err != nil { //nolint:gosec // executable test fixture
		t.Fatal(err)
	}
	archive := archiveBytes(t, candidate)
	manifest := release.Manifest{Schema: 1, Version: "v0.2.0", Commit: strings.Repeat("a", 40), Compatibility: release.Compatibility{StateReadMin: 7, StateReadMax: 7, StateWrite: 7, WorkerMin: 1, WorkerMax: 1, WorkerWrite: 1, JournalVersion: 1}}
	for _, platform := range []release.Platform{{OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"}, {OS: "darwin", Arch: "arm64"}} {
		manifest.Artifacts = append(manifest.Artifacts, release.Artifact{Platform: platform, Archive: "mesh_" + platform.OS + "_" + platform.Arch + ".tar.gz", SHA256: digestBytes(archive), BinarySHA256: digestBytes(candidate)})
		manifest.Compatibility.Transitions = append(manifest.Compatibility.Transitions, release.Transition{FromDigest: digestBytes(old), ToDigest: digestBytes(candidate), Platform: platform, Proof: strings.Repeat("b", 64)})
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "mesh-release.json") {
			_ = json.NewEncoder(w).Encode(manifest)
			return
		}
		_, _ = w.Write(archive)
	}))
	t.Cleanup(server.Close)
	worker := exec.Command("cat")
	input, err := worker.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := worker.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = worker.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = worker.Process.Kill(); _ = worker.Wait() })
	manager := &processService{executable: executable, stateDir: filepath.Join(root, "state"), healthPath: filepath.Join(root, "health.json"), worker: Worker{ID: "session-1", PID: worker.Process.Pid, Protocol: 1}}
	if err = manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if manager.process != nil {
			_ = manager.process.Process.Kill()
			_ = manager.process.Wait()
		}
	})
	config := Config{StateDir: manager.stateDir, Executable: executable, CacheDir: filepath.Join(root, "cache"), Service: manager, Probe: manager.probe, HealthTimeout: 300 * time.Millisecond, Client: release.Client{BaseURL: server.URL, HTTPClient: server.Client()}}
	engine, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	request := Request{ID: "operation-1", TargetID: "test-host", Generation: 1, Manifest: manifest, Current: release.Build{Version: "v0.1.0", Commit: manifest.Commit, Digest: digestBytes(old), Platform: release.CurrentPlatform(), StateVersion: 7, WorkerProtocol: 1, UpdateProtocol: 1}}
	f := &fixture{t: t, engine: engine, request: request, manager: manager, server: server, worker: worker, input: input, output: output}
	f.awaitOriginal()
	return f
}

func (f *fixture) awaitOriginal() {
	f.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := f.manager.probe(context.Background()); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatal("fixture daemon did not report its executing build")
}

func (f *fixture) stage() Status {
	f.t.Helper()
	status, err := f.engine.Stage(context.Background(), f.request)
	if err != nil {
		f.t.Fatal(err)
	}
	if status.Phase != Staged {
		f.t.Fatalf("stage phase = %s", status.Phase)
	}
	return status
}

func (f *fixture) roundTrip() {
	f.t.Helper()
	message := []byte("session is still interactive\n")
	if _, err := f.input.Write(message); err != nil {
		f.t.Fatal(err)
	}
	result := make([]byte, len(message))
	if _, err := io.ReadFull(f.output, result); err != nil {
		f.t.Fatal(err)
	}
	if !bytes.Equal(result, message) {
		f.t.Fatalf("worker output = %q", result)
	}
}

func TestGrantSeparatesStagingFromActivationAndPreservesWorkerIO(t *testing.T) {
	f := newFixture(t)
	f.roundTrip()
	f.stage()
	if _, err := f.engine.Run(context.Background()); !errors.Is(err, ErrNoGrant) {
		t.Fatalf("ungranted run = %v", err)
	}
	if f.manager.stops != 0 {
		t.Fatal("staging restarted daemon")
	}
	if _, err := f.engine.Grant(context.Background(), f.request.ID, 1); err != nil {
		t.Fatal(err)
	}
	status, err := f.engine.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Phase != Committed || status.Verified.Build.Version != "v0.2.0" {
		t.Fatalf("result = %+v", status)
	}
	if f.manager.stops != 1 {
		t.Fatalf("daemon stops = %d", f.manager.stops)
	}
	f.roundTrip()
	if err = updategate.Check(f.engine.cfg.StateDir); err != nil {
		t.Fatal(err)
	}
	if _, err = f.engine.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.manager.stops != 1 {
		t.Fatal("receipt replay restarted daemon")
	}
}

func TestFailedCandidateRestoresExecutingBuildAndLiveWorker(t *testing.T) {
	f := newFixture(t)
	f.stage()
	f.manager.failCandidate = true
	if _, err := f.engine.Grant(context.Background(), f.request.ID, 1); err != nil {
		t.Fatal(err)
	}
	status, err := f.engine.Run(context.Background())
	if err == nil || status.Phase != RolledBack {
		t.Fatalf("rollback = %s, %v", status.Phase, err)
	}
	if status.Verified.Build.Digest != f.request.Current.Digest {
		t.Fatal("restored daemon runs wrong executable")
	}
	f.roundTrip()
	if err = updategate.Check(f.engine.cfg.StateDir); err != nil {
		t.Fatal(err)
	}
	f.manager.failCandidate = false
	status, err = f.engine.Retry(context.Background(), f.request.ID, 1)
	if err != nil || status.Phase != Staged {
		t.Fatalf("retry = %s,%v", status.Phase, err)
	}
	if _, err = f.engine.Run(context.Background()); !errors.Is(err, ErrNoGrant) {
		t.Fatalf("retry reused old activation grant: %v", err)
	}
}

func TestCancellationFencesDelayedGrant(t *testing.T) {
	f := newFixture(t)
	f.stage()
	if _, err := f.engine.Cancel(context.Background(), f.request.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.Grant(context.Background(), f.request.ID, 1); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("late grant = %v", err)
	}
	if _, err := f.engine.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.manager.stops != 0 {
		t.Fatal("cancelled update activated")
	}
}

func TestGrantedWorkCannotBeCancelledAndConflictsAreFenced(t *testing.T) {
	f := newFixture(t)
	f.stage()
	if _, err := f.engine.Grant(context.Background(), f.request.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.Cancel(context.Background(), f.request.ID, 1); !errors.Is(err, ErrAlreadyGranted) {
		t.Fatalf("granted cancellation = %v", err)
	}
	other := f.request
	other.Generation = 2
	if _, err := f.engine.Stage(context.Background(), other); !errors.Is(err, ErrConflict) {
		t.Fatalf("competing update = %v", err)
	}
}

func TestPreflightRejectsUnprovenTransitionAndMissingDataMount(t *testing.T) {
	f := newFixture(t)
	unproven := f.request
	unproven.Manifest.Compatibility.Transitions = nil
	if _, err := f.engine.Stage(context.Background(), unproven); err == nil {
		t.Fatal("unproven rollback accepted")
	}
	f.engine.cfg.RequiredMount = filepath.Join(t.TempDir(), "unmounted")
	if _, err := f.engine.Stage(context.Background(), f.request); err == nil {
		t.Fatal("missing configured mount accepted")
	}
	if f.manager.stops != 0 {
		t.Fatal("preflight restarted daemon")
	}
}
