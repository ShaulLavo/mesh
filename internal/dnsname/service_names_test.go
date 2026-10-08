package dnsname

import (
	"context"
	"testing"

	"github.com/shaul/mesh/internal/tailnet"
)

func TestServiceNamesUseTailnetDNSAndPinnedOrigin(t *testing.T) {
	originID := testIdentityID(t)
	provider := &memoryProvider{}
	distributor := &managerTestDistributor{}
	config := PrivateNamesManagerConfig{
		ServiceHosts: true, Provider: provider, Renewer: &managerTestRenewer{bundle: Bundle{Fingerprint: "current"}}, Distributor: distributor,
		Origins: []PrivateOrigin{{Name: "pc", ServiceNames: []string{"fregat", "comfy"}, TailscaleName: "pc.example.ts.net", Identity: originID, ControlPort: 7337, WebSocketPath: "/mesh"}},
		DiscoverSelf: func(context.Context) (tailnet.Peer, error) {
			return tailnet.Peer{Name: "pc.example.ts.net", Addrs: []string{"100.64.0.9"}}, nil
		},
		DiscoverPeers: func(context.Context) ([]tailnet.Peer, error) { return nil, nil },
	}
	manager, err := NewPrivateNamesManager(config)
	if err != nil {
		t.Fatal(err)
	}
	config.Origins[0].ServiceNames[0] = "mutated"
	for range 2 {
		if err := manager.RunOnce(context.Background(), false); err != nil {
			t.Fatal(err)
		}
	}
	if provider.creates != 2 || provider.updates != 0 {
		t.Fatalf("creates=%d updates=%d", provider.creates, provider.updates)
	}
	for _, record := range provider.records {
		if record.Name != "fregat.mesh.test" && record.Name != "comfy.mesh.test" {
			t.Fatalf("unexpected record %#v", record)
		}
		if record.Content != "100.64.0.9" || record.Proxied {
			t.Fatalf("service DNS = %#v", record)
		}
	}
	if len(distributor.calls) != 2 || len(distributor.calls[0]) != 1 || distributor.calls[0][0].Identity != originID || distributor.calls[0][0].PrivateName != "" {
		t.Fatalf("targets = %#v", distributor.calls)
	}
	config.Origins[0].ServiceNames[0] = "fregat"
	config.Origins = append(config.Origins, PrivateOrigin{Name: "other", ServiceNames: []string{"fregat"}, TailscaleName: "other.example.ts.net", Identity: testIdentityID(t), ControlPort: 7337, WebSocketPath: "/mesh"})
	if _, err := NewPrivateNamesManager(config); err == nil {
		t.Fatal("duplicate service name accepted")
	}
}
