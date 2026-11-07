package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/testenv"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/updateinstall"
	"github.com/shaul/mesh/internal/updatenotice"
)

type automaticProcessConfig struct {
	Certificate []byte
	StateDir    string
	Host        Host
	Key         ed25519.PrivateKey
	Fleet       Fleet
	BaseURL     string
	Build       release.Build
	Original    []byte
}
type automaticProcess struct {
	command *exec.Cmd
	config  automaticProcessConfig
	client  Client
}

// This fixture runs the real signed carrier, coordinator, consent owner and
// installer journal in separate processes. It never installs a native service.
func TestAutomaticProcessesKeepFailedAndCancelledAuthority(t *testing.T) {
	processes, _ := newAutomaticProcesses(t)

	c := processes[1]
	enable := AutomaticCommand{ID: mustAutomaticID(t), Action: automaticEnable}
	var status AutomaticStatus
	c.call(t, "automatic.change", enable, &status)
	c.call(t, "automatic.fixture-step", nil, &status)
	first := *status.Record
	if first.Canary == nil || first.Current == nil {
		encoded, _ := json.Marshal(first)
		t.Fatalf("no pinned canary: %s", encoded)
	}
	processes[0].call(t, "automatic.fixture-fail", nil, nil)
	c.call(t, "automatic.fixture-step", nil, &status)
	failed := *status.Record
	if failed.Stop == nil || failed.Stop.Kind != "failed" {
		t.Fatalf("canary failure not sticky: %+v", failed)
	}
	c.stop(t)
	c.start(t)
	for range 2 {
		c.call(t, "automatic.fixture-step", nil, &status)
	}
	if status.Record.Stop.ID != failed.Stop.ID || status.Record.Current.ID != first.Current.ID || status.Record.Attempt != 1 {
		t.Fatalf("process restart bypassed stop: %+v", status)
	}
	disable := AutomaticCommand{ID: mustAutomaticID(t), Action: "disable", ExpectedRevision: status.Record.Policy.Revision}
	c.call(t, "automatic.change", disable, &status)
	retry := AutomaticCommand{ID: mustAutomaticID(t), Action: automaticRetry, RunID: first.Current.ID, ExpectedRevision: status.Record.Policy.Revision}
	c.call(t, "automatic.change", retry, &status)
	c.call(t, "automatic.fixture-reserve-retry", nil, &status)
	if status.Record.Current.Targets[0].RetryAttempt != 2 {
		t.Fatal("retry reservation was not durable")
	}
	c.stop(t)
	c.start(t)
	c.call(t, "automatic.change", retry, &status)
	c.call(t, "automatic.fixture-step", nil, &status)
	if !activeContinuation(*status.Record) || status.Record.Attempt != 2 {
		t.Fatalf("disabled continuation lost across process death: %+v", status)
	}
	target, err := updateinstall.Read(processes[0].config.StateDir)
	if err != nil || target.RetryToken != 1 {
		t.Fatalf("real target retry token: %+v %v", target, err)
	}
	cancel := AutomaticCommand{ID: mustAutomaticID(t), Action: "cancel", RunID: first.Current.ID, ExpectedRevision: status.Record.Policy.Revision}
	c.call(t, "automatic.change", cancel, &status)
	cancelled := status.Record.Stop.ID
	c.stop(t)
	c.start(t)
	c.call(t, "automatic.fixture-step", nil, &status)
	if status.Record.Policy.Enabled || status.Record.Stop.ID != cancelled || activeContinuation(*status.Record) {
		t.Fatalf("process restart bypassed cancellation: %+v", status)
	}
	for _, p := range processes {
		p.stop(t)
	}
}
func automaticHash(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func automaticProcessRelease(t *testing.T, old, candidate []byte) (release.Manifest, []byte) {
	t.Helper()
	var buffer bytes.Buffer
	compressed := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(compressed)
	if err := archive.WriteHeader(&tar.Header{Name: "mesh", Mode: 0700, Size: int64(len(candidate))}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write(candidate); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	manifest := testManifest()
	for i := range manifest.Artifacts {
		manifest.Artifacts[i].SHA256 = automaticHash(buffer.Bytes())
		manifest.Artifacts[i].BinarySHA256 = automaticHash(candidate)
	}
	for i := range manifest.Compatibility.Transitions {
		manifest.Compatibility.Transitions[i].FromDigest = automaticHash(old)
		manifest.Compatibility.Transitions[i].ToDigest = automaticHash(candidate)
	}
	return manifest, buffer.Bytes()
}
func (p *automaticProcess) start(t *testing.T) {
	t.Helper()
	data, err := json.Marshal(p.config)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("/proc/self/exe", "-test.run=^TestAutomaticProcessHelper$")
	command.Env = append(os.Environ(), "MESH_AUTOMATIC_PROCESS_FIXTURE=1")
	command.Stdin = bytes.NewReader(data)
	out, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	p.command = command
	t.Cleanup(func() { p.stop(t) })
	ready := make(chan error, 1)
	go func() { var marker string; ready <- json.NewDecoder(out).Decode(&marker) }()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("automatic child did not listen")
	}
}
func (p *automaticProcess) stop(t *testing.T) {
	t.Helper()
	if p.command == nil {
		return
	}
	command := p.command
	p.command = nil
	if err := command.Process.Kill(); err != nil {
		t.Error(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("automatic child did not exit")
	}
}
func (p *automaticProcess) call(t *testing.T, action string, input, output any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.client.Call(ctx, p.config.Host, action, input, output); err != nil {
		t.Fatalf("process %s: %v", action, err)
	}
}
func TestAutomaticProcessHelper(t *testing.T) {
	if os.Getenv("MESH_AUTOMATIC_PROCESS_FIXTURE") != "1" {
		return
	}
	var cfg automaticProcessConfig
	if err := json.NewDecoder(os.Stdin).Decode(&cfg); err != nil {
		t.Fatal(err)
	}

	_ = os.Remove(strings.TrimPrefix(cfg.Host.Endpoint, "unix://"))
	listener, err := net.Listen("unix", strings.TrimPrefix(cfg.Host.Endpoint, "unix://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	authority, c, engine := automaticChildOwners(t, cfg)
	authority.Handle = func(ctx context.Context, actor VerifiedActor, action string, data json.RawMessage) (any, error) {
		return automaticChildControl(ctx, cfg, authority, c, engine, actor, action, data)
	}
	if err := json.NewEncoder(os.Stdout).Encode("ready"); err != nil {
		t.Fatal(err)
	}
	for {
		stream, err := listener.Accept()
		if err != nil {
			t.Fatal(err)
		}
		go serveAutomaticChild(stream, authority)

	}
}
func automaticChildOwners(t *testing.T, cfg automaticProcessConfig) (*Authority, *Coordinator, *updateinstall.Engine) {
	t.Helper()
	store, err := OpenStore(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	authority := &Authority{StateDir: cfg.StateDir, ID: cfg.Host.ID, Key: cfg.Key}
	cert, err := x509.ParseCertificate(cfg.Certificate)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	client := release.Client{BaseURL: cfg.BaseURL, HTTPClient: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}}}}
	c := &Coordinator{ID: cfg.Host.ID, Store: store, Remote: Client{ID: cfg.Host.ID, Key: cfg.Key}, Release: client, CacheDir: filepath.Join(cfg.StateDir, "cache"), SavedFleet: func() (Fleet, error) { return ReadFleet(filepath.Join(cfg.StateDir, "fleet.json")) }, Trusted: authority.IsAdministrator}
	notice := updatenotice.New(updatenotice.Config{Directory: filepath.Join(cfg.StateDir, "notice")})
	c.CheckedRelease = notice.CheckedRelease
	engine, err := updateinstall.New(updateinstall.Config{StateDir: cfg.StateDir, Executable: filepath.Join(cfg.StateDir, "mesh"), CacheDir: c.CacheDir, Client: client, ClientOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	return authority, c, engine
}
func automaticChildControl(ctx context.Context, cfg automaticProcessConfig, authority *Authority, c *Coordinator, engine *updateinstall.Engine, actor VerifiedActor, action string, data json.RawMessage) (any, error) {
	engine = engine.WithAcceptance(c.Store.AuthorizeAcceptance(actor, authority.IsAdministrator))
	switch action {
	case "automatic.change":
		var command AutomaticCommand
		if err := json.Unmarshal(data, &command); err != nil {
			return nil, fmt.Errorf("decode fixture automatic command: %w", err)
		}
		return c.ChangeAutomatic(ctx, actor, command)
	case "automatic.consent":
		return c.Store.RecipientConsent(ctx)
	case "automatic.fixture-reserve-retry":
		return automaticChildReserveRetry(ctx, c)
	case "automatic.fixture-admit":
		if err := c.refreshAutomatic(ctx); err != nil {
			return nil, err
		}
		return c.Store.ReadAutomatic(ctx)
	case "automatic.fixture-step":
		if err := c.Step(ctx); err != nil {
			return nil, err
		}
		return c.Store.ReadAutomatic(ctx)
	case "info":
		return automaticChildInfo(cfg)
	case "stage":
		var request updateinstall.Request
		if err := json.Unmarshal(data, &request); err != nil {
			return nil, fmt.Errorf("decode fixture stage: %w", err)
		}
		status, err := engine.Stage(ctx, request)
		if err != nil {
			return nil, fmt.Errorf("fixture stage: %w", err)
		}
		return status, nil
	case "automatic.fixture-fail":
		return automaticChildFail(ctx, cfg, engine)
	default:
		return automaticChildOperation(ctx, engine, action, data)
	}
}
func automaticChildInfo(cfg automaticProcessConfig) (Info, error) {
	info := Info{Health: updateinstall.Health{HostID: cfg.Host.ID, Build: cfg.Build}}
	status, err := updateinstall.Read(cfg.StateDir)
	if errors.Is(err, os.ErrNotExist) {
		return info, nil
	}
	if err != nil {
		return info, fmt.Errorf("read fixture installation info: %w", err)
	}
	info.Installation = &status
	return info, nil
}
func automaticChildFail(ctx context.Context, cfg automaticProcessConfig, engine *updateinstall.Engine) (any, error) {
	if err := writeAutomaticProcessImage(cfg.StateDir, []byte("changed")); err != nil {
		return nil, err
	}
	_, _ = engine.Run(ctx)
	if err := writeAutomaticProcessImage(cfg.StateDir, cfg.Original); err != nil {
		return nil, err
	}
	status, err := engine.Read()
	if err != nil {
		return nil, fmt.Errorf("read failed fixture journal: %w", err)
	}
	return status, nil
}
func automaticChildOperation(ctx context.Context, engine *updateinstall.Engine, action string, data json.RawMessage) (any, error) {
	var op Operation
	if err := json.Unmarshal(data, &op); err != nil {
		return nil, fmt.Errorf("decode fixture operation: %w", err)
	}
	var result updateinstall.Status
	var err error
	switch action {
	case "grant":
		result, err = engine.Grant(ctx, op.ID, op.Generation)
	case "install-retry":
		result, err = engine.RetryWithToken(ctx, op.ID, op.Generation, op.RetryToken)
	case "install-cancel":
		result, err = engine.Cancel(ctx, op.ID, op.Generation)
	default:
		return nil, fmt.Errorf("unknown automatic fixture action %s", action)
	}
	if err != nil {
		return nil, fmt.Errorf("fixture installation operation: %w", err)
	}
	return result, nil
}

func newAutomaticProcesses(t *testing.T) ([]*automaticProcess, release.Manifest) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("isolated process fixture uses the Linux shell executable format")
	}
	root := testenv.SocketTempDir(t)
	fleet := testFleet(t, 2)
	keys := make([]ed25519.PrivateKey, 2)
	for i := range fleet.Members {
		fleet.Members[i].ID, keys[i] = testIdentity(t)
		fleet.Members[i].Endpoint = "unix://" + filepath.Join(root, fmt.Sprintf("h%d.sock", i))
	}
	coordinator := fleet.Members[1].ID
	owner, key := testIdentity(t)
	old := []byte("#!/bin/sh\nexit 0\n")
	candidate := []byte("#!/bin/sh\nexit 1\n")
	manifest, archive := automaticProcessRelease(t, old, candidate)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "mesh-release.json") {
			_ = json.NewEncoder(w).Encode(manifest)
			return
		}
		_, _ = w.Write(archive)
	}))
	t.Cleanup(server.Close)
	processes := make([]*automaticProcess, 0, 2)
	for i, host := range fleet.Members {
		state := filepath.Join(root, fmt.Sprintf("state-%d", i))
		store, err := OpenStore(state)
		if err != nil {
			t.Fatal(err)
		}
		if err := writeJSON(filepath.Join(state, "fleet.json"), fleet); err != nil {
			t.Fatal(err)
		}
		if err := Trust(state, coordinator, false); err != nil {
			t.Fatal(err)
		}
		if err := Trust(state, owner, false); err != nil {
			t.Fatal(err)
		}
		if _, err := store.SetRecipientConsent(context.Background(), 0, ConsentChange{LocalHostID: host.ID, CoordinatorID: coordinator, Fleet: fleet, Epoch: 1, Enabled: true}); err != nil {
			t.Fatal(err)
		}
		build := release.Build{Version: "v0.1.0", Digest: automaticHash(old), Commit: manifest.Commit, Platform: release.CurrentPlatform(), StateVersion: 7, WorkerProtocol: 1, UpdateProtocol: 1}
		p := &automaticProcess{config: automaticProcessConfig{StateDir: state, Host: host, Key: keys[i], Fleet: fleet, BaseURL: server.URL, Build: build, Original: old, Certificate: server.Certificate().Raw}, client: Client{ID: owner, Key: key}}
		if err := writeAutomaticProcessImage(state, old); err != nil {
			t.Fatal(err)
		}
		notice := updatenotice.New(updatenotice.Config{Directory: filepath.Join(state, "notice")})
		if err := notice.Record(context.Background(), manifest); err != nil {
			t.Fatal(err)
		}
		p.start(t)
		processes = append(processes, p)
	}
	return processes, manifest
}

func TestAutomaticProcessesCancelledSlotSurvivesSettledRestartAndNewObservation(t *testing.T) {
	for _, mode := range []string{"before grant", "after settled grant"} {
		t.Run(mode, func(t *testing.T) { testAutomaticProcessCancel(t, mode) })
	}
}
func testAutomaticProcessCancel(t *testing.T, mode string) {
	processes, manifest := newAutomaticProcesses(t)
	c := processes[1]
	var status AutomaticStatus
	c.call(t, "automatic.change", AutomaticCommand{ID: mustAutomaticID(t), Action: automaticEnable}, &status)
	action := "automatic.fixture-admit"
	if mode != "before grant" {
		action = "automatic.fixture-step"
	}
	c.call(t, action, nil, &status)
	runID := status.Record.Current.ID
	c.call(t, "automatic.change", AutomaticCommand{ID: mustAutomaticID(t), Action: "cancel", RunID: runID, ExpectedRevision: status.Record.Policy.Revision}, &status)
	if mode != "before grant" {
		processes[0].call(t, "automatic.fixture-fail", nil, nil)
	}
	c.call(t, "automatic.fixture-step", nil, &status)
	if !automaticSettled(*status.Record.Current) {
		t.Fatalf("cancel did not settle owned work: %+v", status.Record.Current)
	}
	stopID := status.Record.Stop.ID
	c.stop(t)
	manifest.Version = "v0.3.0"
	notice := updatenotice.New(updatenotice.Config{Directory: filepath.Join(c.config.StateDir, "notice")})
	if err := notice.Record(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	c.start(t)
	c.call(t, "automatic.fixture-step", nil, &status)
	if status.Record.Current.ID != runID || status.Record.Stop.ID != stopID || status.Record.Policy.Enabled {
		t.Fatalf("settled cancellation was replaced: %+v", status)
	}
	c.call(t, "automatic.change", AutomaticCommand{ID: mustAutomaticID(t), Action: "acknowledge-stop", RunID: runID, ExpectedRevision: status.Record.Policy.Revision}, &status)
	if status.Record.Current != nil || status.Record.Policy.Enabled {
		t.Fatalf("acknowledgement enabled new work: %+v", status)
	}
	for _, process := range processes {
		process.stop(t)
	}
}

func writeAutomaticProcessImage(directory string, data []byte) error {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return fmt.Errorf("open fixture executable root: %w", err)
	}
	defer func() { _ = root.Close() }()
	if err := root.WriteFile("mesh", data, 0600); err != nil {
		return fmt.Errorf("write fixture executable: %w", err)
	}
	if err := root.Chmod("mesh", 0700); err != nil {
		return fmt.Errorf("publish fixture executable permissions: %w", err)
	}
	return nil
}

func TestAutomaticProcessesAcknowledgeBeforeFirstRun(t *testing.T) {
	for _, kind := range []string{"membership", "owner"} {
		t.Run(kind, func(t *testing.T) { testAutomaticProcessPendingStop(t, kind) })
	}
}
func testAutomaticProcessPendingStop(t *testing.T, kind string) {
	processes, _ := newAutomaticProcesses(t)
	c := processes[1]
	var status AutomaticStatus
	c.call(t, "automatic.change", AutomaticCommand{ID: mustAutomaticID(t), Action: automaticEnable}, &status)
	if kind == "membership" {
		fleet := c.config.Fleet
		fleet.Revision++
		if err := writeJSON(filepath.Join(c.config.StateDir, "fleet.json"), fleet); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := writeJSON(filepath.Join(c.config.StateDir, "updates", "administrators.json"), Policy{Version: 1, Keys: []string{c.config.Host.ID}}); err != nil {
			t.Fatal(err)
		}
	}
	c.client = Client{ID: c.config.Host.ID, Key: c.config.Key}
	c.call(t, "automatic.fixture-step", nil, &status)
	if status.Record.Stop == nil || status.Record.Current != nil {
		t.Fatalf("no-current process stop: %+v", status)
	}
	stopID := status.Record.Stop.ID
	c.stop(t)
	c.start(t)
	c.call(t, "automatic.change", AutomaticCommand{ID: mustAutomaticID(t), Action: "disable", ExpectedRevision: status.Record.Policy.Revision}, &status)
	ack := AutomaticCommand{ID: mustAutomaticID(t), Action: "acknowledge-stop", RunID: status.Record.Policy.ID, ExpectedRevision: status.Record.Policy.Revision}
	c.call(t, "automatic.change", ack, &status)
	c.stop(t)
	c.start(t)
	c.call(t, "automatic.change", ack, &status)
	c.call(t, "automatic.fixture-step", nil, &status)
	if status.Record.Stop != nil || status.Record.Policy.Enabled || status.Record.Current != nil || status.Record.LastAcknowledgement == nil || status.Record.LastAcknowledgement.Stop.ID != stopID {
		t.Fatalf("pending acknowledgement did not survive process restart: %+v", status)
	}
	c.call(t, "automatic.change", AutomaticCommand{ID: mustAutomaticID(t), Action: automaticEnable, TakeOwnership: true, ExpectedRevision: status.Record.Policy.Revision}, &status)
	c.call(t, "automatic.fixture-step", nil, &status)
	if status.Record.Policy.OwnerID != c.config.Host.ID || status.Record.Policy.Epoch != 2 || status.Record.Current != nil {
		t.Fatalf("ownership recovery reused prior consent: %+v", status)
	}
	for _, p := range processes {
		p.stop(t)
	}
}

func serveAutomaticChild(stream net.Conn, authority *Authority) {
	defer func() { _ = stream.Close() }()
	conn, err := transport.NewStreamConn(stream)
	if err != nil {
		return
	}
	for range 2 {
		if err := exchangeDownloadPlan(context.Background(), conn, authority); err != nil {
			return
		}
	}
}

func automaticChildReserveRetry(ctx context.Context, c *Coordinator) (any, error) {
	status, err := c.Store.ReadAutomatic(ctx)
	if err != nil {
		return nil, err
	}
	if status.Record == nil || status.Record.Current == nil {
		return nil, errors.New("missing fixture automatic run")
	}
	_, issued, err := c.reserveAutomatic(ctx, status.Record.Current.ID, 0, automaticRetry)
	if err != nil {
		return nil, fmt.Errorf("reserve fixture retry: %w", err)
	}
	if !issued {
		return nil, errors.New("fixture retry was not issued")
	}
	return c.Store.ReadAutomatic(ctx)
}
