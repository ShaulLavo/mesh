package dnsname

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/tailnet"
)

func TestPublicCertificateManagerSelectsOnePinnedNumericEndpoint(t *testing.T) {
	target := PublicEdgeTarget{
		TailscaleName: "edge.example.ts.net", Identity: testIdentityID(t), ControlPort: 7337, WebSocketPath: "/control/ws",
	}
	for name, addresses := range map[string][]string{
		"prefer IPv4": {"fd7a:115c:a1e0::8", "100.70.0.8"},
		"IPv6 only":   {"fd7a:115c:a1e0::8"},
	} {
		t.Run(name, func(t *testing.T) {
			renewer := &managerTestRenewer{bundle: Bundle{Fingerprint: "current"}}
			distributor := &managerTestDistributor{}
			manager := newPublicManagerForTest(t, target, renewer, distributor,
				func(context.Context) (tailnet.Peer, error) {
					return tailnet.Peer{Name: target.TailscaleName, Online: true, Addrs: addresses}, nil
				},
				func(context.Context) ([]tailnet.Peer, error) { return nil, nil },
			)
			if err := manager.RunOnce(context.Background(), true); err != nil {
				t.Fatal(err)
			}
			address := "100.70.0.8:7337"
			if name == "IPv6 only" {
				address = "[fd7a:115c:a1e0::8]:7337"
			}
			want := [][]OriginTarget{{{
				Name: "public-edge", Identity: target.Identity, Endpoint: "ws://" + address + "/control/ws",
			}}}
			if !reflect.DeepEqual(distributor.calls, want) {
				t.Fatalf("distribution calls = %#v, want %#v", distributor.calls, want)
			}
			if renewer.calls != 1 || !renewer.forces[0] {
				t.Fatalf("renew calls = %d, forces = %v", renewer.calls, renewer.forces)
			}
		})
	}
}

func TestPublicCertificateManagerRenewsButRefusesUnsafeDiscovery(t *testing.T) {
	target := PublicEdgeTarget{
		TailscaleName: "edge.example.ts.net", Identity: testIdentityID(t), ControlPort: 7337, WebSocketPath: "/mesh",
	}
	cases := map[string]struct {
		self  tailnet.Peer
		peers []tailnet.Peer
		err   error
	}{
		"absent": {},
		"offline": {
			self: tailnet.Peer{Name: target.TailscaleName, Online: false, Addrs: []string{"100.70.0.8"}},
		},
		"duplicate": {
			self:  tailnet.Peer{Name: target.TailscaleName, Online: true, Addrs: []string{"100.70.0.8"}},
			peers: []tailnet.Peer{{Name: target.TailscaleName, Online: false, Addrs: []string{"100.70.0.9"}}},
		},
		"malformed": {
			self: tailnet.Peer{Name: target.TailscaleName, Online: true, Addrs: []string{"not-an-address"}},
		},
		"outside Tailscale": {
			self: tailnet.Peer{Name: target.TailscaleName, Online: true, Addrs: []string{"203.0.113.8"}},
		},
		"discovery failed": {err: errors.New("tailscale unavailable")},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			renewer := &managerTestRenewer{bundle: Bundle{Fingerprint: "current"}}
			distributor := &managerTestDistributor{}
			manager := newPublicManagerForTest(t, target, renewer, distributor,
				func(context.Context) (tailnet.Peer, error) { return testCase.self, testCase.err },
				func(context.Context) ([]tailnet.Peer, error) { return testCase.peers, nil },
			)
			if err := manager.RunOnce(context.Background(), false); err == nil {
				t.Fatal("unsafe discovery succeeded")
			}
			if renewer.calls != 1 {
				t.Fatalf("renew calls = %d, want 1", renewer.calls)
			}
			if len(distributor.calls) != 0 {
				t.Fatalf("unsafe target was distributed: %#v", distributor.calls)
			}
		})
	}
}

func TestPublicCertificateManagerReturnsDistributionFailure(t *testing.T) {
	target := PublicEdgeTarget{
		TailscaleName: "edge.example.ts.net", Identity: testIdentityID(t), ControlPort: 7337, WebSocketPath: "/mesh",
	}
	distributeErr := errors.New("install failed")
	distributor := &managerTestDistributor{err: distributeErr}
	manager := newPublicManagerForTest(t, target, &managerTestRenewer{bundle: Bundle{Fingerprint: "current"}}, distributor,
		func(context.Context) (tailnet.Peer, error) {
			return tailnet.Peer{Name: target.TailscaleName, Online: true, Addrs: []string{"100.70.0.8"}}, nil
		},
		func(context.Context) ([]tailnet.Peer, error) { return nil, nil },
	)
	err := manager.RunOnce(context.Background(), false)
	if !errors.Is(err, distributeErr) || !strings.Contains(err.Error(), "distribute public wildcard") {
		t.Fatalf("RunOnce error = %v", err)
	}
}

func newPublicManagerForTest(
	t *testing.T,
	target PublicEdgeTarget,
	renewer CertificateRenewer,
	distributor CertificateDistributor,
	discoverSelf func(context.Context) (tailnet.Peer, error),
	discoverPeers func(context.Context) ([]tailnet.Peer, error),
) *PublicCertificateManager {
	t.Helper()
	manager, err := NewPublicCertificateManager(PublicCertificateManagerConfig{
		Renewer: renewer, Distributor: distributor, Target: target,
		DiscoverSelf: discoverSelf, DiscoverPeers: discoverPeers,
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}
