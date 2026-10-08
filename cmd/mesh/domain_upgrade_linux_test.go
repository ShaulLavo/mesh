//go:build linux

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/dnsname"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/serve"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/testenv"
	"github.com/shaul/mesh/internal/updatebootstrap"
	"github.com/shaul/mesh/internal/updateinstall"
)

const upgradePrivateHost = "pc.mesh.shaulavo.dev"

type deploymentService struct {
	executable  string
	arguments   []string
	environment []string
	process     *exec.Cmd
	log         *os.File
}

func (s *deploymentService) Start(context.Context) error {
	command := exec.Command(s.executable, s.arguments...) //nolint:gosec // execute only the test-built binary in isolated state
	command.Env, command.Stdout, command.Stderr = s.environment, s.log, s.log
	if err := command.Start(); err != nil {
		return fmt.Errorf("start deployment fixture: %w", err)
	}
	s.process = command
	return nil
}

func (s *deploymentService) Stop(ctx context.Context) error {
	if s.process == nil {
		return nil
	}
	command := s.process
	s.process = nil
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("stop deployment fixture: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("wait for deployment fixture: %w", err)
		}
		return nil
	case <-ctx.Done():
		_ = command.Process.Kill()
		<-done
		return fmt.Errorf("stop deployment fixture: %w", ctx.Err())
	}
}

func buildDeploymentFixture(t *testing.T, path, version string) release.Build {
	t.Helper()
	command := exec.CommandContext(t.Context(), "go", "build", "-ldflags=-X github.com/shaul/mesh/internal/release.Version="+version, "-o", path, ".") //nolint:gosec // build this package into the isolated test directory
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build deployment fixture: %v\n%s", err, output)
	}
	output, err := exec.CommandContext(t.Context(), path, "version", "--json").Output() //nolint:gosec // inspect the test-built executable
	if err != nil {
		t.Fatal(err)
	}
	var build release.Build
	if err := json.Unmarshal(output, &build); err != nil {
		t.Fatal(err)
	}
	return build
}

func TestPrePolicyDeploymentSurvivesReplacementAndManagedActivation(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("Go is required to build executable transition fixtures")
	}
	root := t.TempDir()
	oldPath, candidatePath := filepath.Join(root, "old-mesh"), filepath.Join(root, "candidate-mesh")
	oldBuild := buildDeploymentFixture(t, oldPath, "v0.0.1")
	candidateBuild := buildDeploymentFixture(t, candidatePath, "v0.0.2")
	if candidateBuild.Commit == "" {
		t.Skip("managed-release proof requires a source checkout with Git build metadata")
	}
	for _, managed := range []bool{false, true} {
		t.Run(strconv.FormatBool(managed), func(t *testing.T) {
			verifyDeploymentTransition(t, oldPath, candidatePath, oldBuild, candidateBuild, managed)
		})
	}
}

func verifyDeploymentTransition(t *testing.T, oldPath, candidatePath string, oldBuild, candidateBuild release.Build, managed bool) {
	t.Helper()
	root := t.TempDir()
	state, err := os.MkdirTemp(os.TempDir(), "domain-upgrade-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(state) })
	config := filepath.Join(root, "config")
	if err := os.MkdirAll(config, 0o700); err != nil {
		t.Fatal(err)
	}
	policyPath := filepath.Join(config, "domains.json")
	if err := os.WriteFile(policyPath, []byte(`{"primary":"shaulavo.dev","legacyCertificateDomain":"shaulavo.dev"}`), 0o600); err != nil { //nolint:gosec // read or write only files owned by this isolated executable-transition fixture
		t.Fatal(err)
	}
	host, _, err := identity.LoadOrCreate(state)
	if err != nil {
		t.Fatal(err)
	}
	renewer, _, err := identity.LoadOrCreate(filepath.Join(root, "renewer"))
	if err != nil {
		t.Fatal(err)
	}
	certificate := seedDeploymentState(t, state, root)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' '{\"TCP\":{\"443\":{\"TCPForward\":\"127.0.0.1:%d\"}}}'\n", port)
	if err := os.WriteFile(filepath.Join(bin, "tailscale"), []byte(script), 0o700); err != nil { //nolint:gosec // read or write only files owned by this isolated executable-transition fixture
		t.Fatal(err)
	} //nolint:gosec // executable fixture replaces external Tailscale
	installed := filepath.Join(root, "mesh")
	oldBytes, err := os.ReadFile(oldPath) //nolint:gosec // read or write only files owned by this isolated executable-transition fixture
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installed, oldBytes, 0o700); err != nil { //nolint:gosec // read or write only files owned by this isolated executable-transition fixture
		t.Fatal(err)
	} //nolint:gosec // isolated executable replacement fixture
	log, err := os.Create(filepath.Join(root, "daemon.log")) //nolint:gosec // fixture log is below the test-owned temporary root
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	service := &deploymentService{executable: installed, arguments: []string{"daemon", "--https-port", strconv.Itoa(port), "--certificate-renewer-id", renewer.ID}, log: log,
		environment: append(testenv.ForProcess(root), "MESH_STATE_DIR="+state, "MESH_CONFIG_DIR="+config, "PATH="+bin+":"+os.Getenv("PATH"))}
	if err := service.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := service.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	probe := updatebootstrap.Probe(state)
	awaitDeployment(t, probe)
	verifyOldDeploymentURL(t, port, certificate)
	// The running old installation keeps its immutable policy; activation starts
	// with only the pre-policy certificate, private name, and service state.
	if err := os.Remove(policyPath); err != nil {
		t.Fatal(err)
	}
	if managed {
		activateDeployment(t, service, state, root, probe, host.ID, oldBuild, candidateBuild, candidatePath)
	} else {
		replaceDeployment(t, service, installed, candidatePath)
	}
	awaitDeployment(t, probe)
	contents, err := os.ReadFile(policyPath) //nolint:gosec // read or write only files owned by this isolated executable-transition fixture
	if err != nil {
		t.Fatalf("activation lost deployment policy: %v", err)
	}
	var policy struct {
		Primary                 string
		LegacyCertificateDomain string
	}
	if err := json.Unmarshal(contents, &policy); err != nil {
		t.Fatal(err)
	}
	if policy.Primary != "shaulavo.dev" || policy.LegacyCertificateDomain != "shaulavo.dev" {
		t.Fatalf("migrated policy: %s", contents)
	}
	verifyOldDeploymentURL(t, port, certificate)
}

func awaitDeployment(t *testing.T, probe updateinstall.Probe) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var err error
	for time.Now().Before(deadline) {
		if _, err = probe(t.Context()); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("deployment did not become healthy: %v", err)
}

func seedDeploymentState(t *testing.T, state, root string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(771), DNSNames: []string{"*.mesh.shaulavo.dev"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	encodedKey, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	slot := filepath.Join(state, "private-tls", "live")
	store, err := dnsname.NewBundleStore(slot, "*.mesh.shaulavo.dev")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Install(certificate, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encodedKey})); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(slot, "private-name"), []byte(upgradePrivateHost+"\n"), 0o600); err != nil { //nolint:gosec // read or write only files owned by this isolated executable-transition fixture
		t.Fatal(err)
	}
	site := filepath.Join(root, "site")
	if err := os.Mkdir(site, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(site, "index.html"), []byte("legacy-route"), 0o600); err != nil { //nolint:gosec // read or write only files owned by this isolated executable-transition fixture
		t.Fatal(err)
	}
	database, err := storage.Open(t.Context(), filepath.Join(state, "mesh.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.UpsertService(t.Context(), serve.Service{Name: "site", Kind: serve.Static, Target: site}); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	return certificate
}

func verifyOldDeploymentURL(t *testing.T, port int, certificate []byte) {
	t.Helper()
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certificate) {
		t.Fatal("fixture certificate rejected")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	response, err := client.Get("https://" + upgradePrivateHost + "/site/")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close() //nolint:errcheck // read result decides the proof
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK || string(body) != "legacy-route" {
		t.Fatalf("old deployment URL: status=%d body=%q err=%v", response.StatusCode, body, err)
	}
}

func activateDeployment(t *testing.T, service *deploymentService, state, root string, probe updateinstall.Probe, host string, oldBuild, candidateBuild release.Build, candidatePath string) {
	t.Helper()
	candidate, err := os.ReadFile(candidatePath) //nolint:gosec // read or write only files owned by this isolated executable-transition fixture
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	compressed := gzip.NewWriter(&archive)
	writer := tar.NewWriter(compressed)
	if err := writer.WriteHeader(&tar.Header{Name: "mesh", Mode: 0o700, Size: int64(len(candidate))}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(candidate); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(archive.Bytes())
	manifest := release.Manifest{Schema: 1, Version: candidateBuild.Version, Commit: candidateBuild.Commit,
		Compatibility: release.Compatibility{StateReadMin: oldBuild.StateVersion, StateReadMax: candidateBuild.StateVersion, StateWrite: candidateBuild.StateVersion, WorkerMin: 1, WorkerMax: 1, WorkerWrite: 1, JournalVersion: 1}}
	for _, platform := range []release.Platform{{OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"}, {OS: "darwin", Arch: "arm64"}} {
		manifest.Artifacts = append(manifest.Artifacts, release.Artifact{Platform: platform, Archive: "mesh_" + platform.OS + "_" + platform.Arch + ".tar.gz", SHA256: hex.EncodeToString(sum[:]), BinarySHA256: candidateBuild.Digest})
		manifest.Compatibility.Transitions = append(manifest.Compatibility.Transitions, release.Transition{FromDigest: oldBuild.Digest, ToDigest: candidateBuild.Digest, Platform: platform, Proof: hex.EncodeToString(sum[:])})
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if filepath.Base(r.URL.Path) == "mesh-release.json" {
			_ = json.NewEncoder(w).Encode(manifest)
			return
		}
		_, _ = w.Write(archive.Bytes())
	}))
	defer server.Close()
	engine, err := updateinstall.New(updateinstall.Config{StateDir: state, Executable: service.executable, CacheDir: filepath.Join(root, "cache"), Service: service, Probe: probe, HealthTimeout: 10 * time.Second, Client: release.Client{BaseURL: server.URL, HTTPClient: server.Client()}})
	if err != nil {
		t.Fatal(err)
	}
	request := updateinstall.Request{ID: "domain-policy-upgrade", TargetID: host, Generation: 1, Manifest: manifest, Current: oldBuild}
	if status, err := engine.Stage(t.Context(), request); err != nil || status.Phase != updateinstall.Staged {
		t.Fatalf("stage: %s %v", status.Phase, err)
	}
	if _, err := engine.Grant(t.Context(), request.ID, 1); err != nil {
		t.Fatal(err)
	}
	status, err := engine.Run(t.Context())
	if err != nil || status.Phase != updateinstall.Committed {
		t.Fatalf("activation rolled back: phase=%s err=%v", status.Phase, err)
	}
}

func replaceDeployment(t *testing.T, service *deploymentService, installed, candidatePath string) {
	t.Helper()
	if err := service.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	candidate, err := os.ReadFile(candidatePath) //nolint:gosec // read or write only files owned by this isolated executable-transition fixture
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installed, candidate, 0o700); err != nil { //nolint:gosec // read or write only files owned by this isolated executable-transition fixture
		t.Fatal(err)
	} //nolint:gosec // direct replacement stays inside the isolated installation
	if err := service.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
}
