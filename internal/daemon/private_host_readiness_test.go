package daemon

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/dnsname"
	"github.com/shaul/mesh/internal/identity"
	meshserve "github.com/shaul/mesh/internal/serve"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/tailnet"
)

func TestRunWithdrawsPrivateHostMachineMountWithAndWithoutCertificate(t *testing.T) {
	state := compactSocketTempDir(t)
	target, _, err := identity.LoadOrCreate(state)
	if err != nil {
		t.Fatal(err)
	}
	renewer, signer, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("short host available"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(t.Context(), filepath.Join(state, databaseName))
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.UpsertService(t.Context(), meshserve.Service{Name: "platform", Kind: meshserve.Static, Target: root, PrivateHost: "fregat.mesh.test"})
	closeErr := store.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("seed service: %v, close: %v", err, closeErr)
	}
	for _, ready := range []bool{false, true} {
		listener, port := newTCPListener(t, "127.0.0.1:0")
		httpsListener, httpsPort := newTCPListener(t, "127.0.0.1:0")
		if ready {
			installReadinessCertificate(t, certificateRuntimeConfig{StateDir: state, TargetID: target.ID, OriginHTTPSPort: httpsPort, OriginRenewerID: renewer.ID}, signer)
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			done <- run(ctx, Config{StateDir: state, TailnetPort: port, HTTPSPort: httpsPort, CertificateRenewerID: renewer.ID}, runOptions{
				tailscaleTimeout: time.Second, verifyServeForward: func(context.Context, uint16) error { return nil },
				now: time.Now, bootID: func() string { return "fixture-boot" },
				discoverSelf: func(context.Context) (tailnet.Peer, error) {
					return tailnet.Peer{Name: "pc.example.ts.net", Addrs: []string{"127.0.0.1"}}, nil
				},
				discoverPeers: func(context.Context) ([]tailnet.Peer, error) { return nil, nil },
				listen:        useTCPListeners(listener, httpsListener), reconcileInterval: time.Hour,
			})
		}()
		url := fmt.Sprintf("http://127.0.0.1:%d/platform/", port)
		want := http.StatusNotFound
		client := &http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		deadline := time.Now().Add(runtimeTestTimeout)
		var matched bool
		for time.Now().Before(deadline) {
			response, err := client.Get(url)
			if err != nil {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			_ = response.Body.Close()
			matched = response.StatusCode == want && response.Header.Get("Location") == "" && privateHostRootAvailable(client, port)
			if matched {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
		if err := waitRuntime(t, done); err != nil {
			t.Fatal(err)
		}
		if !matched {
			t.Fatalf("ready=%t: daemon did not return %d", ready, want)
		}
	}
}

func installReadinessCertificate(t *testing.T, config certificateRuntimeConfig, signer ed25519.PrivateKey) {
	t.Helper()
	runtime, err := configureCertificates(certificateRuntimeConfig{StateDir: config.StateDir, TargetID: config.TargetID, OriginHTTPSPort: config.OriginHTTPSPort, OriginRenewerID: config.OriginRenewerID})
	if err != nil {
		t.Fatal(err)
	}
	certificate, key := daemonTestNamedCertificate(t, 991, time.Now().UTC(), "*.mesh.test")
	bundle, err := dnsname.ValidateBundle(certificate, key, "*.mesh.test", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	signed, err := dnsname.SignBundle(bundle, config.TargetID, dnsname.ProfilePrivateService, dnsname.EnvironmentLive, "", signer)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := runtime.Controller.(*certificateController).installers[dnsname.ProfilePrivateService].Install(signed); err != nil {
		t.Fatal(err)
	}

}

func privateHostRootAvailable(client *http.Client, port uint16) bool {
	request, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/", port), nil)
	if err != nil {
		return false
	}
	request.Host = "fregat.mesh.test"
	response, err := client.Do(request)
	if err != nil {
		return false
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	return err == nil && response.StatusCode == http.StatusOK && string(body) == "short host available"
}
