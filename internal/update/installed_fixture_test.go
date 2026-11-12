//go:build mesh_installed_fixture

package update

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/updatebootstrap"
	"github.com/shaul/mesh/internal/updateinstall"
)

type installedSignRequest struct {
	Files   []string          `json:"files"`
	Output  string            `json:"output"`
	Stop    bool              `json:"stop"`
	Put     map[string]string `json:"put"`
	Seed    *installedSeed    `json:"seed"`
	Inspect string            `json:"inspect"`
	Ledger  string            `json:"ledger"`
}

type installedSeed struct {
	State   string `json:"state"`
	Version string `json:"version"`
}

// The exporter reuses the reader fixtures' genuine Sigstore and TUF evidence.
// Private signing keys stay in this explicitly tagged test process.
func TestInstalledFixtureExport(t *testing.T) {
	directory := os.Getenv("MESH_INSTALLED_FIXTURE_DIR")
	if directory == "" {
		t.Skip("installed fixture exporter requires its owned scratch directory")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	fixture := newSigningFixture(t)
	proxy := newAdoptionNativeProxy(t, fixture, directory)
	exportInstalledTrust(t, root, fixture, proxy)
	listener, err := net.Listen("unix", filepath.Join(directory, "control.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	for {
		connection, err := listener.Accept()
		if err != nil {
			t.Fatal(err)
		}
		if installedSignConnection(t, root, fixture, proxy, connection) {
			return
		}
	}
}

func installedWrite(t *testing.T, root *os.Root, name string, contents []byte) {
	t.Helper()
	if err := root.WriteFile(name, contents, 0600); err != nil {
		t.Fatal(err)
	}
}

func exportInstalledTrust(t *testing.T, root *os.Root, fixture *signingFixture, proxy *adoptionNativeProxy) {
	t.Helper()
	installedWrite(t, root, "bootstrap-root.json", fixture.trust.BootstrapRoot)
	installedWrite(t, root, "trusted-root.json", fixture.trust.Trusted)
	installedWrite(t, root, "provenance.json", fixture.provenance)
	installedWrite(t, root, "proxy.json", mustHostJSON(map[string]string{"address": proxy.server.URL}))
}

func installedSignConnection(t *testing.T, root *os.Root, fixture *signingFixture, proxy *adoptionNativeProxy, connection net.Conn) bool {
	t.Helper()
	defer func() { _ = connection.Close() }()
	if err := connection.SetDeadline(time.Now().Add(90 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var request installedSignRequest
	decoder := json.NewDecoder(io.LimitReader(connection, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		t.Fatal(err)
	}
	if len(request.Files) > 0 {
		installedSignFiles(t, root, fixture, request)
	}
	for name, path := range request.Put {
		data, err := root.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		proxy.put(name, data)
	}
	if request.Seed != nil {
		installedSeedJournal(t, fixture, proxy, *request.Seed)
	}
	if request.Inspect != "" {
		installation, err := updatebootstrap.InspectInstallation(t.Context(), request.Inspect)
		if err != nil {
			t.Fatal(err)
		}
		installedWrite(t, root, request.Output, mustHostJSON(installation.Service))
		hashes, err := updatebootstrap.InstalledEnvironmentHashes(t.Context(), installation.Observation.PID)
		if err != nil {
			t.Fatal(err)
		}
		installedWrite(t, root, request.Output+".env-hashes.json", mustHostJSON(hashes))
	}
	if request.Ledger != "" {
		installedLedger(t, root, request)
	}
	if err := json.NewEncoder(connection).Encode(map[string]bool{"ok": true}); err != nil {
		t.Fatal(err)
	}
	return request.Stop
}

func installedLedger(t *testing.T, root *os.Root, request installedSignRequest) {
	t.Helper()
	store, err := OpenStore(request.Ledger)
	if err != nil {
		t.Fatal(err)
	}
	entries, more, err := store.HostLedger(t.Context(), 0, 64)
	if err != nil || more {
		t.Fatalf("bounded installed ledger: %v more=%v", err, more)
	}
	state, err := store.HostState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	failed, next, err := store.hostFailedPage("", 64)
	if err != nil || next != "" {
		t.Fatalf("bounded installed failed edges: %v next=%q", err, next)
	}
	installedWrite(t, root, request.Output, mustHostJSON(map[string]any{"entries": entries, "state": state, "failed": failed}))
}

// This is the delivered producer's signed stage/grant and Engine.Run sequence,
// with the actual native manager selected by the real daemon's journal settings.
func installedSeedJournal(t *testing.T, signer *signingFixture, proxy *adoptionNativeProxy, seed installedSeed) {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(signer.issuer)
	address, err := url.Parse(proxy.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyURL(address)
	transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	defer transport.CloseIdleConnections()
	client := release.Client{HTTPClient: &http.Client{Transport: transport, Timeout: 30 * time.Second}}
	manifest, err := client.Manifest(t.Context(), seed.Version)
	if err != nil {
		t.Fatal(err)
	}
	host, key, err := identity.LoadPrivate(seed.State)
	if err != nil {
		t.Fatal(err)
	}
	observed, err := updatebootstrap.Inspect(t.Context(), seed.State)
	if err != nil {
		t.Fatal(err)
	}
	request := updateinstall.Request{ID: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", TargetID: host.ID, Generation: 1, Current: observed.Health.Build, Manifest: manifest}
	caller, local := Client{ID: host.ID, Key: key}, LocalHost(seed.State, host.ID)
	var status updateinstall.Status
	if err := caller.Call(t.Context(), local, "stage", request, &status); err != nil {
		t.Fatal(err)
	}
	if err := caller.Call(t.Context(), local, "grant", Operation{ID: request.ID, Generation: 1}, &status); err != nil {
		t.Fatal(err)
	}
	configuration := status.Settings.Config()
	configuration.Client, configuration.Probe = client, updatebootstrap.Probe(seed.State)
	engine, err := updateinstall.New(configuration)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	result, err := engine.Run(ctx)
	if err != nil || result.Phase != updateinstall.Committed {
		t.Fatalf("real published prior commit: %s %v", result.Phase, err)
	}
}

func installedSignFiles(t *testing.T, root *os.Root, fixture *signingFixture, request installedSignRequest) {
	t.Helper()
	if len(request.Files) == 0 || len(request.Files) > 4 || !filepath.IsLocal(request.Output) {
		t.Fatal("invalid installed fixture signing request")
	}
	subjects := make([][]byte, 0, len(request.Files))
	for _, name := range request.Files {
		contents, err := root.ReadFile(name)
		if err != nil || len(contents) > 128<<20 {
			t.Fatalf("read bounded fixture subject %s: %v", name, err)
		}
		subjects = append(subjects, contents)
	}
	installedWrite(t, root, request.Output, fixture.envelope(t, subjects...))
}
