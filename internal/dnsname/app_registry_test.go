package dnsname

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/tailnet"
	"github.com/shaul/mesh/internal/transport"
)

func TestServiceWildcardDistributionIncludesRegistryWithoutRegistryDNS(t *testing.T) {
	now := time.Now().UTC()
	signerID, signer := testEd25519Identity(t)
	registryID, _ := testEd25519Identity(t)
	originID, _ := testEd25519Identity(t)
	cert, key := testCertificate(t, 881, ServiceWildcardName(), now.Add(-time.Hour), now.Add(90*24*time.Hour))
	bundle, err := ValidateBundle(cert, key, ServiceWildcardName(), now)
	if err != nil {
		t.Fatal(err)
	}
	identities := map[string]string{
		"ws://100.64.0.4:7443/control/ws": registryID,
		"ws://100.64.0.9:7337/mesh":       originID,
	}
	var mu sync.Mutex
	dials := map[string]int{}
	distributor := mustTestDistributor(t, DistributorConfig{
		Profile: ProfilePrivateService, Signer: signer, Environment: EnvironmentLive,
		Now: func() time.Time { return now },
		Dial: func(_ context.Context, endpoint string) (transport.Conn, error) {
			mu.Lock()
			dials[endpoint]++
			mu.Unlock()
			return newDistributionTestConn(t, identities[endpoint], signerID, bundle.Fingerprint, now), nil
		},
	})
	provider := &memoryProvider{}
	renewer := &managerTestRenewer{bundle: bundle}
	manager, err := NewPrivateNamesManager(PrivateNamesManagerConfig{
		ServiceHosts: true, Provider: provider, Renewer: renewer, Distributor: distributor,
		Origins:     []PrivateOrigin{{Name: "pc", ServiceNames: []string{"fregat"}, Identity: originID, TailscaleName: "pc.example.ts.net", ControlPort: 7337, WebSocketPath: "/mesh"}},
		AppRegistry: &CertificateRecipient{Identity: registryID, TailscaleName: "registry.example.ts.net", ControlPort: 7443, WebSocketPath: "/control/ws"},
		DiscoverSelf: func(context.Context) (tailnet.Peer, error) {
			return tailnet.Peer{Name: "pc.example.ts.net", Addrs: []string{"100.64.0.9"}, Online: true}, nil
		},
		DiscoverPeers: func(context.Context) ([]tailnet.Peer, error) {
			return []tailnet.Peer{{Name: "registry.example.ts.net", Addrs: []string{"fd7a:115c:a1e0::4", "100.64.0.4"}, Online: true}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := manager.RunOnce(context.Background(), false); err != nil {
			t.Fatal(err)
		}
	}
	if len(provider.records) != 1 || provider.records[0].Name != "fregat.mesh.test" || provider.records[0].Content != "100.64.0.9" || provider.creates != 1 {
		t.Fatalf("private DNS records = %#v, creates = %d", provider.records, provider.creates)
	}
	if renewer.calls != 2 || !reflect.DeepEqual(dials, map[string]int{"ws://100.64.0.4:7443/control/ws": 2, "ws://100.64.0.9:7337/mesh": 2}) {
		t.Fatalf("renewal calls = %d, signed distribution dials = %#v", renewer.calls, dials)
	}
}

func TestRegistryOnlyRenewalHasNoDNSWritesAndRejectsUnsafeDiscovery(t *testing.T) {
	registry := CertificateRecipient{Identity: testIdentityID(t), TailscaleName: "registry.example.ts.net", ControlPort: 7443, WebSocketPath: "/mesh"}
	valid := tailnet.Peer{Name: registry.TailscaleName, Addrs: []string{"100.64.0.4"}, Online: true}
	for name, peers := range map[string][]tailnet.Peer{
		"valid":           {valid},
		"ipv6":            {{Name: registry.TailscaleName, Addrs: []string{"fd7a:115c:a1e0::4"}, Online: true}},
		"missing":         nil,
		"ambiguous":       {valid, valid},
		"offline":         {{Name: registry.TailscaleName, Addrs: valid.Addrs}},
		"public address":  {{Name: registry.TailscaleName, Addrs: []string{"203.0.113.4"}, Online: true}},
		"mixed addresses": {{Name: registry.TailscaleName, Addrs: []string{"100.64.0.4", "127.0.0.1"}, Online: true}},
		"malformed":       {{Name: registry.TailscaleName, Addrs: []string{"bad"}, Online: true}},
	} {
		t.Run(name, func(t *testing.T) {
			provider := &memoryProvider{}
			renewer := &managerTestRenewer{bundle: Bundle{Fingerprint: "current"}}
			distributor := &managerTestDistributor{}
			manager, err := NewPrivateNamesManager(PrivateNamesManagerConfig{
				ServiceHosts: true, AppRegistry: &registry, Provider: provider, Renewer: renewer, Distributor: distributor,
				DiscoverSelf:  func(context.Context) (tailnet.Peer, error) { return tailnet.Peer{Name: "other.example.ts.net"}, nil },
				DiscoverPeers: func(context.Context) ([]tailnet.Peer, error) { return peers, nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			err = manager.RunOnce(context.Background(), false)
			accepted := name == "valid" || name == "ipv6"
			if (err == nil) != accepted {
				t.Fatalf("discovery accepted = %v, error = %v", accepted, err)
			}
			if provider.creates+provider.updates+provider.deletes != 0 || renewer.calls != 1 {
				t.Fatalf("provider = %#v, renew calls = %d", provider, renewer.calls)
			}
			if len(distributor.calls) != 1 {
				t.Fatalf("distribution calls = %#v", distributor.calls)
			}
			targets := distributor.calls[0]
			if accepted && (len(targets) != 1 || targets[0].Identity != registry.Identity || targets[0].PrivateName != "") {
				t.Fatalf("registry targets = %#v", targets)
			}
			if !accepted && len(targets) != 0 {
				t.Fatalf("unsafe targets = %#v", targets)
			}
		})
	}
}

func TestRegistryCertificateConfigurationRequiresPrivateServiceAndCombinedLimit(t *testing.T) {
	registry := CertificateRecipient{Identity: testIdentityID(t), TailscaleName: "registry.example.ts.net", ControlPort: 7443, WebSocketPath: "/mesh"}
	base := PrivateNamesManagerConfig{
		ServiceHosts: true, AppRegistry: &registry, Provider: &memoryProvider{}, Renewer: &managerTestRenewer{},
		DiscoverSelf:  func(context.Context) (tailnet.Peer, error) { return tailnet.Peer{}, nil },
		DiscoverPeers: func(context.Context) ([]tailnet.Peer, error) { return nil, nil },
	}
	wrongProfile := base
	wrongProfile.ServiceHosts = false
	if _, err := NewPrivateNamesManager(wrongProfile); err == nil {
		t.Fatal("origin wildcard manager accepted registry recipient")
	}
	overLimit := base
	overLimit.Origins = make([]PrivateOrigin, maximumDistributionTargets)
	if _, err := NewPrivateNamesManager(overLimit); err == nil || !strings.Contains(err.Error(), "target count") {
		t.Fatalf("combined limit error = %v", err)
	}
	invalid := registry
	invalid.Identity = "invalid"
	base.AppRegistry = &invalid
	if _, err := NewPrivateNamesManager(base); err == nil {
		t.Fatal("invalid registry identity accepted")
	}
}

func TestRegistryOnlyRuntimeConstructsOnlyServiceWildcardManager(t *testing.T) {
	directory := t.TempDir()
	token := filepath.Join(directory, "token")
	if err := os.WriteFile(token, []byte("zone-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf(`{"zoneId":"0123456789abcdef0123456789abcdef","tokenFile":%q,"acmeEmail":"owner@example.com","directoryUrl":%q,"acceptTerms":true,"appRegistry":{"identity":%q,"tailscaleName":"registry.example.ts.net","controlPort":7443,"websocketPath":"/mesh"}}`, token, LetsEncryptProductionURL, testIdentityID(t))
	path := writePrivateNamesConfig(t, directory, config)
	runtime, err := NewPrivateNamesRuntime(path, PrivateNamesRuntimeOptions{StateDir: filepath.Join(directory, "state")})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Manager != nil || runtime.ServiceManager == nil || runtime.ServiceManager.renewer.(*Issuer).config.Name != ServiceWildcardName() {
		t.Fatalf("recipient-only runtime = %#v", runtime)
	}
}

func TestServiceWildcardCoalescesRegistryAndOriginRecipient(t *testing.T) {
	now := time.Now().UTC()
	signerID, signer := testEd25519Identity(t)
	originID := testIdentityID(t)
	cert, key := testCertificate(t, 882, ServiceWildcardName(), now.Add(-time.Hour), now.Add(90*24*time.Hour))
	bundle, err := ValidateBundle(cert, key, ServiceWildcardName(), now)
	if err != nil {
		t.Fatal(err)
	}
	dials := 0
	distributor := mustTestDistributor(t, DistributorConfig{
		Profile: ProfilePrivateService, Signer: signer, Environment: EnvironmentLive,
		Now: func() time.Time { return now },
		Dial: func(_ context.Context, endpoint string) (transport.Conn, error) {
			if endpoint != "ws://100.64.0.9:7337/mesh" {
				t.Errorf("recipient endpoint = %s", endpoint)
			}
			dials++
			return newDistributionTestConn(t, originID, signerID, bundle.Fingerprint, now), nil
		},
	})
	provider := &memoryProvider{}
	renewer := &managerTestRenewer{bundle: bundle}
	manager, err := NewPrivateNamesManager(PrivateNamesManagerConfig{
		ServiceHosts: true, Provider: provider, Renewer: renewer, Distributor: distributor,
		Origins:     []PrivateOrigin{{Name: "pc", ServiceNames: []string{"fregat"}, Identity: originID, TailscaleName: "pc.example.ts.net", ControlPort: 7337, WebSocketPath: "/mesh"}},
		AppRegistry: &CertificateRecipient{Identity: originID, TailscaleName: "pc.example.ts.net", ControlPort: 7337, WebSocketPath: "/mesh"},
		DiscoverSelf: func(context.Context) (tailnet.Peer, error) {
			return tailnet.Peer{Name: "pc.example.ts.net", Addrs: []string{"100.64.0.9"}, Online: true}, nil
		},
		DiscoverPeers: func(context.Context) ([]tailnet.Peer, error) { return nil, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.RunOnce(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if dials != 1 || renewer.calls != 1 || provider.creates != 1 || len(provider.records) != 1 || provider.records[0].Name != "fregat.mesh.test" {
		t.Fatalf("signed installs = %d, renewals = %d, provider = %#v", dials, renewer.calls, provider)
	}
}

func TestServiceWildcardRejectsConflictingRegistryOriginRecipient(t *testing.T) {
	origin := PrivateOrigin{Name: "pc", ServiceNames: []string{"fregat"}, Identity: testIdentityID(t), TailscaleName: "pc.example.ts.net", ControlPort: 7337, WebSocketPath: "/mesh"}
	registry := CertificateRecipient{Identity: origin.Identity, TailscaleName: origin.TailscaleName, ControlPort: origin.ControlPort, WebSocketPath: origin.WebSocketPath}
	for name, mutate := range map[string]func(*CertificateRecipient){
		"identity":     func(r *CertificateRecipient) { r.Identity = testIdentityID(t) },
		"peer name":    func(r *CertificateRecipient) { r.TailscaleName = "different.example.ts.net" },
		"control port": func(r *CertificateRecipient) { r.ControlPort = 7443 },
		"control path": func(r *CertificateRecipient) { r.WebSocketPath = "/control/ws" },
	} {
		t.Run(name, func(t *testing.T) {
			conflicting := registry
			mutate(&conflicting)
			_, err := NewPrivateNamesManager(PrivateNamesManagerConfig{
				ServiceHosts: true, Provider: &memoryProvider{}, Renewer: &managerTestRenewer{}, Origins: []PrivateOrigin{origin}, AppRegistry: &conflicting,
				DiscoverSelf: func(context.Context) (tailnet.Peer, error) {
					t.Fatal("invalid config reached discovery")
					return tailnet.Peer{}, nil
				},
				DiscoverPeers: func(context.Context) ([]tailnet.Peer, error) {
					t.Fatal("invalid config reached discovery")
					return nil, nil
				},
			})
			if err == nil || !strings.Contains(err.Error(), "conflicting") {
				t.Fatalf("recipient conflict error = %v", err)
			}
		})
	}
}
