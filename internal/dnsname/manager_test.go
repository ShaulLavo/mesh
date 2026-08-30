package dnsname

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/tailnet"
)

func TestPrivateNamesManagerReconcilesSelfAndPeersAndAlwaysDistributes(t *testing.T) {
	piID := testIdentityID(t)
	desktopID := testIdentityID(t)
	provider := &memoryProvider{}
	renewer := &managerTestRenewer{bundle: Bundle{Fingerprint: "current"}, renewed: false}
	distributor := &managerTestDistributor{}
	selfCalls := 0
	peerCalls := 0
	manager, err := NewPrivateNamesManager(PrivateNamesManagerConfig{
		Provider: provider, Renewer: renewer, Distributor: distributor,
		Origins: []PrivateOrigin{
			{Name: "pi", TailscaleName: "pi.example.ts.net", Identity: piID, ControlPort: 7337, WebSocketPath: "/mesh"},
			{Name: "desktop", TailscaleName: "desktop.example.ts.net", Identity: desktopID, ControlPort: 7447, WebSocketPath: "/control/ws"},
		},
		DiscoverSelf: func(context.Context) (tailnet.Peer, error) {
			selfCalls++
			return tailnet.Peer{Name: "pi.example.ts.net", Addrs: []string{"fd7a:115c:a1e0::1", "100.64.0.9"}, Online: true}, nil
		},
		DiscoverPeers: func(context.Context) ([]tailnet.Peer, error) {
			peerCalls++
			return []tailnet.Peer{{Name: "desktop.example.ts.net", Addrs: []string{"100.80.0.2"}, Online: false}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.RunOnce(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if provider.creates != 2 || len(provider.records) != 2 {
		t.Fatalf("provider creates = %d, records = %#v", provider.creates, provider.records)
	}
	wantAddresses := map[string]string{
		"pi.mesh.shaulavo.dev":      "100.64.0.9",
		"desktop.mesh.shaulavo.dev": "100.80.0.2",
	}
	for _, record := range provider.records {
		if record.Content != wantAddresses[record.Name] || record.Proxied || record.Comment != ManagedARecordComment {
			t.Fatalf("managed A record = %#v", record)
		}
	}
	wantTargets := []OriginTarget{
		{Name: "pi", Endpoint: "ws://100.64.0.9:7337/mesh", Identity: piID},
		{Name: "desktop", Endpoint: "ws://100.80.0.2:7447/control/ws", Identity: desktopID},
	}
	if len(distributor.calls) != 1 || !reflect.DeepEqual(distributor.calls[0], wantTargets) {
		t.Fatalf("distribution calls = %#v, want %#v", distributor.calls, wantTargets)
	}
	if renewer.calls != 1 || renewer.forces[0] {
		t.Fatalf("renew calls = %d, forces = %#v", renewer.calls, renewer.forces)
	}

	if err := manager.RunOnce(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if provider.creates != 2 || provider.updates != 0 {
		t.Fatalf("idempotent provider creates = %d, updates = %d", provider.creates, provider.updates)
	}
	if len(distributor.calls) != 2 {
		t.Fatalf("current bundle distributions = %d, want 2", len(distributor.calls))
	}
	if selfCalls != 2 || peerCalls != 2 {
		t.Fatalf("discovery calls self = %d, peers = %d", selfCalls, peerCalls)
	}
}

func TestPrivateNamesManagerContinuesAvailableOriginsAndReportsMissingPeer(t *testing.T) {
	availableID := testIdentityID(t)
	missingID := testIdentityID(t)
	provider := &memoryProvider{}
	renewer := &managerTestRenewer{bundle: Bundle{Fingerprint: "current"}}
	distributor := &managerTestDistributor{}
	manager, err := NewPrivateNamesManager(PrivateNamesManagerConfig{
		Provider: provider, Renewer: renewer, Distributor: distributor,
		Origins: []PrivateOrigin{
			{Name: "pi", TailscaleName: "pi.example.ts.net", Identity: availableID, ControlPort: 7337, WebSocketPath: "/mesh"},
			{Name: "missing", TailscaleName: "missing.example.ts.net", Identity: missingID, ControlPort: 7337, WebSocketPath: "/mesh"},
		},
		DiscoverSelf: func(context.Context) (tailnet.Peer, error) {
			return tailnet.Peer{Name: "pi.example.ts.net", Addrs: []string{"100.64.0.9"}}, nil
		},
		DiscoverPeers: func(context.Context) ([]tailnet.Peer, error) { return nil, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	err = manager.RunOnce(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "is absent") {
		t.Fatalf("missing peer error = %v", err)
	}
	if renewer.calls != 1 || !renewer.forces[0] {
		t.Fatalf("renew calls = %d, forces = %#v", renewer.calls, renewer.forces)
	}
	if len(distributor.calls) != 1 || len(distributor.calls[0]) != 1 || distributor.calls[0][0].Name != "pi" {
		t.Fatalf("available distributions = %#v", distributor.calls)
	}
}

func TestPrivateNamesManagerRenewsWhenDiscoveryFails(t *testing.T) {
	provider := &memoryProvider{}
	renewer := &managerTestRenewer{}
	manager, err := NewPrivateNamesManager(PrivateNamesManagerConfig{
		Provider: provider, Renewer: renewer,
		Origins:       []PrivateOrigin{{Name: "pi", TailscaleName: "pi.example.ts.net", Identity: testIdentityID(t), ControlPort: 7337, WebSocketPath: "/mesh"}},
		DiscoverSelf:  func(context.Context) (tailnet.Peer, error) { return tailnet.Peer{}, errors.New("self failed") },
		DiscoverPeers: func(context.Context) ([]tailnet.Peer, error) { return nil, errors.New("peers failed") },
	})
	if err != nil {
		t.Fatal(err)
	}
	err = manager.RunOnce(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "self failed") || !strings.Contains(err.Error(), "peers failed") {
		t.Fatalf("discovery error = %v", err)
	}
	if provider.creates != 0 || renewer.calls != 1 {
		t.Fatalf("failed discovery changed %d DNS records; renew calls = %d", provider.creates, renewer.calls)
	}
}

func TestPrivateNamesManagerUsesPeerTargetsWhenSelfDiscoveryFails(t *testing.T) {
	piID := testIdentityID(t)
	desktopID := testIdentityID(t)
	provider := &memoryProvider{}
	renewer := &managerTestRenewer{bundle: Bundle{Fingerprint: "current"}}
	distributor := &managerTestDistributor{}
	manager, err := NewPrivateNamesManager(PrivateNamesManagerConfig{
		Provider: provider, Renewer: renewer, Distributor: distributor,
		Origins: []PrivateOrigin{
			{Name: "pi", TailscaleName: "pi.example.ts.net", Identity: piID, ControlPort: 7337, WebSocketPath: "/mesh"},
			{Name: "desktop", TailscaleName: "desktop.example.ts.net", Identity: desktopID, ControlPort: 7447, WebSocketPath: "/control/ws"},
		},
		DiscoverSelf: func(context.Context) (tailnet.Peer, error) {
			return tailnet.Peer{}, errors.New("self failed")
		},
		DiscoverPeers: func(context.Context) ([]tailnet.Peer, error) {
			return []tailnet.Peer{{Name: "desktop.example.ts.net", Addrs: []string{"100.80.0.2"}}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = manager.RunOnce(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "self failed") {
		t.Fatalf("partial discovery error = %v", err)
	}
	if renewer.calls != 1 || provider.creates != 1 {
		t.Fatalf("renew calls = %d, provider creates = %d", renewer.calls, provider.creates)
	}
	want := [][]OriginTarget{{{Name: "desktop", Endpoint: "ws://100.80.0.2:7447/control/ws", Identity: desktopID}}}
	if !reflect.DeepEqual(distributor.calls, want) {
		t.Fatalf("partial discovery distributions = %#v, want %#v", distributor.calls, want)
	}
}

func TestPrivateNamesManagerDistributesCurrentBundleWhenRenewalFails(t *testing.T) {
	id := testIdentityID(t)
	renewer := &managerTestRenewer{bundle: Bundle{Fingerprint: "still-valid"}, err: errors.New("ACME unavailable")}
	distributor := &managerTestDistributor{}
	manager, err := NewPrivateNamesManager(PrivateNamesManagerConfig{
		Provider: &memoryProvider{}, Renewer: renewer, Distributor: distributor,
		Origins: []PrivateOrigin{{Name: "pi", TailscaleName: "pi.example.ts.net", Identity: id, ControlPort: 7337, WebSocketPath: "/mesh"}},
		DiscoverSelf: func(context.Context) (tailnet.Peer, error) {
			return tailnet.Peer{Name: "pi.example.ts.net", Addrs: []string{"100.64.0.9"}}, nil
		},
		DiscoverPeers: func(context.Context) ([]tailnet.Peer, error) { return nil, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	err = manager.RunOnce(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "ACME unavailable") {
		t.Fatalf("renewal error = %v", err)
	}
	if len(distributor.calls) != 1 || len(distributor.calls[0]) != 1 {
		t.Fatalf("fallback distributions = %#v", distributor.calls)
	}
}

func TestPrivateNamesManagerUsesBoundedFailureBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	renewer := &managerTestRenewer{errors: []error{
		errors.New("one"), errors.New("two"), errors.New("three"), errors.New("four"),
		errors.New("five"), errors.New("six"), nil, errors.New("seven"),
	}}
	manager, err := NewPrivateNamesManager(PrivateNamesManagerConfig{
		Provider: &memoryProvider{}, Renewer: renewer,
		Origins: []PrivateOrigin{{Name: "pi", TailscaleName: "pi.example.ts.net", Identity: testIdentityID(t), ControlPort: 7337, WebSocketPath: "/mesh"}},
		DiscoverSelf: func(context.Context) (tailnet.Peer, error) {
			return tailnet.Peer{Name: "pi.example.ts.net", Addrs: []string{"100.64.0.9"}}, nil
		},
		DiscoverPeers: func(context.Context) ([]tailnet.Peer, error) { return nil, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	var delays []time.Duration
	manager.wait = func(_ context.Context, delay time.Duration) bool {
		delays = append(delays, delay)
		if len(delays) == 8 {
			cancel()
			return false
		}
		return true
	}
	if err := manager.Run(ctx, 5*time.Minute, func(error) {}); err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute, 30 * time.Second}
	if !reflect.DeepEqual(delays, want) {
		t.Fatalf("reconcile delays = %v, want %v", delays, want)
	}
	if got := failureRetryDelay(100); got != 15*time.Minute {
		t.Fatalf("maximum failure retry = %s", got)
	}
	if got := min(failureRetryDelay(100), 5*time.Minute); got != 5*time.Minute {
		t.Fatalf("failure retry beyond healthy cadence = %s", got)
	}
}

func TestPrivateNamesManagerRunStartsImmediatelyAndRepeatsUnattended(t *testing.T) {
	provider := &memoryProvider{}
	ctx, cancel := context.WithCancel(context.Background())
	renewer := &managerTestRenewer{bundle: Bundle{Fingerprint: "current"}}
	renewer.afterRenew = func(calls int) {
		if calls == 2 {
			cancel()
		}
	}
	manager, err := NewPrivateNamesManager(PrivateNamesManagerConfig{
		Provider: provider, Renewer: renewer,
		Origins: []PrivateOrigin{{Name: "pi", TailscaleName: "pi.example.ts.net", Identity: testIdentityID(t), ControlPort: 7337, WebSocketPath: "/mesh"}},
		DiscoverSelf: func(context.Context) (tailnet.Peer, error) {
			return tailnet.Peer{Name: "pi.example.ts.net", Addrs: []string{"100.64.0.9"}}, nil
		},
		DiscoverPeers: func(context.Context) ([]tailnet.Peer, error) { return nil, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Run(ctx, time.Millisecond, nil); err != nil {
		t.Fatal(err)
	}
	if renewer.calls != 2 {
		t.Fatalf("unattended renew calls = %d, want 2", renewer.calls)
	}
}

type managerTestRenewer struct {
	bundle     Bundle
	renewed    bool
	calls      int
	forces     []bool
	afterRenew func(int)
	err        error
	errors     []error
}

func (r *managerTestRenewer) Renew(_ context.Context, force bool) (Bundle, bool, error) {
	r.calls++
	r.forces = append(r.forces, force)
	if r.afterRenew != nil {
		r.afterRenew(r.calls)
	}
	err := r.err
	if r.calls <= len(r.errors) {
		err = r.errors[r.calls-1]
	}
	return r.bundle, r.renewed, err
}

type managerTestDistributor struct {
	calls [][]OriginTarget
}

func (d *managerTestDistributor) Distribute(_ context.Context, _ Bundle, targets []OriginTarget) error {
	d.calls = append(d.calls, append([]OriginTarget(nil), targets...))
	return nil
}
