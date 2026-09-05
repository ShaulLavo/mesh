package wake

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testNIC() NIC {
	return NIC{MAC: "02:11:22:33:44:55", Address: "192.168.1.20", Prefix: "192.168.1.0/24", GatewayMAC: "02:aa:bb:cc:dd:ee"}
}

func testAuthority(t *testing.T, dir string) *Authority {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := NewAuthorityWithOptions(dir, key, AuthorityOptions{Discover: func(context.Context) (NIC, error) { return testNIC(), nil }})
	if err != nil {
		t.Fatal(err)
	}
	return authority
}

func testGrant(t *testing.T) Grant {
	t.Helper()
	grant, err := testAuthority(t, t.TempDir()).SetAllowed(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	return grant
}

func TestAuthorityConsentSurvivesRestartAndRenews(t *testing.T) {
	dir := t.TempDir()
	authority := testAuthority(t, dir)
	initial, err := authority.Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if initial.Enabled || initial.NIC != nil || initial.Revision != 1 {
		t.Fatalf("default policy = %+v", initial)
	}
	grant, err := authority.SetAllowed(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if !grant.Enabled || grant.Revision != 2 {
		t.Fatalf("allowed grant = %+v", grant)
	}
	restored, err := NewAuthorityWithOptions(dir, authority.key, authority.options)
	if err != nil {
		t.Fatal(err)
	}
	current, err := restored.Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(current.Signature, grant.Signature) {
		t.Fatal("unchanged policy changed signature")
	}
	restored.options.Now = func() time.Time { return grant.IssuedAt.Add(GrantLifetime * 3 / 4) }
	renewed, err := restored.Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if renewed.Revision != grant.Revision || !renewed.ExpiresAt.After(grant.ExpiresAt) {
		t.Fatalf("renewal = %+v", renewed)
	}
	if err := ValidateGrant(renewed, restored.options.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestAuthorityTransientDiscoveryFailurePreservesGrantAndExpiry(t *testing.T) {
	authority := testAuthority(t, t.TempDir())
	allowed, err := authority.SetAllowed(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	authority.options.Discover = func(context.Context) (NIC, error) { return NIC{}, ErrNoLAN }
	authority.options.Now = func() time.Time { return allowed.IssuedAt.Add(GrantLifetime * 3 / 4) }
	unavailable, err := authority.Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(unavailable.Signature, allowed.Signature) || !unavailable.ExpiresAt.Equal(allowed.ExpiresAt) {
		t.Fatalf("unavailable = %+v", unavailable)
	}
	denied, err := authority.SetAllowed(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if denied.Enabled || denied.Revision <= allowed.Revision {
		t.Fatalf("explicit deny = %+v", denied)
	}
}

func TestAuthorityFirstAllowWithoutMetadataPersistsConsent(t *testing.T) {
	authority := testAuthority(t, t.TempDir())
	authority.options.Discover = func(context.Context) (NIC, error) { return NIC{}, ErrNoLAN }
	unavailable, err := authority.SetAllowed(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if unavailable.Enabled || unavailable.NIC != nil {
		t.Fatalf("first allow without NIC = %+v", unavailable)
	}
	saved, err := authority.load()
	if err != nil {
		t.Fatal(err)
	}
	if !saved.Allowed {
		t.Fatal("consent was lost with the NIC discovery error")
	}
	authority.options.Discover = func(context.Context) (NIC, error) { return testNIC(), nil }
	recovered, err := authority.Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !recovered.Enabled || recovered.Revision <= unavailable.Revision {
		t.Fatalf("recovered = %+v", recovered)
	}
}

func TestAuthorityNetworkChangeAndUnsupportedNICReplacePriorGrant(t *testing.T) {
	authority := testAuthority(t, t.TempDir())
	allowed, err := authority.SetAllowed(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	authority.options.Discover = func(context.Context) (NIC, error) {
		nic := testNIC()
		nic.GatewayMAC = "02:aa:bb:cc:dd:ff"
		return nic, nil
	}
	moved, err := authority.Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !moved.Enabled || moved.Revision <= allowed.Revision || moved.NIC.GatewayMAC == allowed.NIC.GatewayMAC {
		t.Fatalf("moved grant = %+v", moved)
	}
	authority.options.Discover = func(context.Context) (NIC, error) { return NIC{}, ErrUnsupportedNIC }
	unsupported, err := authority.Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if unsupported.Enabled || unsupported.Revision <= moved.Revision {
		t.Fatalf("unsupported interface = %+v", unsupported)
	}
}

func TestAuthorityDoesNotRenewExpiredNICAfterDiscoveryFailure(t *testing.T) {
	authority := testAuthority(t, t.TempDir())
	allowed, err := authority.SetAllowed(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	authority.options.Discover = func(context.Context) (NIC, error) { return NIC{}, ErrNoLAN }
	authority.options.Now = func() time.Time { return allowed.ExpiresAt }
	unavailable, err := authority.Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if unavailable.Enabled || unavailable.NIC != nil {
		t.Fatalf("renewed stale network metadata = %+v", unavailable)
	}
}

func TestGrantSignatureAndValidity(t *testing.T) {
	grant := testGrant(t)
	if err := ValidateGrant(grant, time.Now()); err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(*Grant){
		"target":     func(g *Grant) { g.TargetID = "../peer" },
		"revision":   func(g *Grant) { g.Revision++ },
		"enablement": func(g *Grant) { g.Enabled = false },
		"MAC":        func(g *Grant) { g.NIC.MAC = "02:11:22:33:44:66" },
		"LAN":        func(g *Grant) { g.NIC.GatewayMAC = "02:aa:bb:cc:dd:ff" },
		"expired":    func(g *Grant) { g.ExpiresAt = time.Now().Add(-time.Hour) },
		"far future": func(g *Grant) { g.ExpiresAt = g.IssuedAt.Add(2 * GrantLifetime) },
		"year":       func(g *Grant) { g.IssuedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) { requireInvalidMutation(t, grant, mutate) })
	}
	if err := ValidateGrant(grant, grant.ExpiresAt); !errors.Is(err, ErrExpired) {
		t.Fatalf("expiry boundary: %v", err)
	}
	if err := ValidateGrant(grant, grant.IssuedAt.Add(-6*time.Minute)); err == nil {
		t.Fatal("future grant accepted")
	}
}

func requireInvalidMutation(t *testing.T, grant Grant, mutate func(*Grant)) {
	t.Helper()
	nic := *grant.NIC
	grant.NIC = &nic
	mutate(&grant)
	if err := ValidateGrant(grant, time.Now()); err == nil {
		t.Fatal("mutated grant accepted")
	}
}

func TestNICValidation(t *testing.T) {
	mutations := map[string]func(*NIC){
		"multicast MAC":   func(n *NIC) { n.MAC = "01:11:22:33:44:55" },
		"zero MAC":        func(n *NIC) { n.MAC = "00:00:00:00:00:00" },
		"EUI64":           func(n *NIC) { n.MAC = "02:11:22:33:44:55:66:77" },
		"unmasked subnet": func(n *NIC) { n.Prefix = "192.168.1.20/24" },
		"default subnet":  func(n *NIC) { n.Prefix = "0.0.0.0/0" },
		"host subnet":     func(n *NIC) { n.Prefix = "192.168.1.20/32" },
		"IPv6":            func(n *NIC) { n.Address = "::1" },
		"loopback":        func(n *NIC) { n.Address = "127.0.0.1" },
		"broadcast":       func(n *NIC) { n.Address = "192.168.1.255" },
		"network":         func(n *NIC) { n.Address = "192.168.1.0" },
		"outside subnet":  func(n *NIC) { n.Address = "192.168.2.20" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) { requireInvalidNIC(t, mutate) })
	}
}

func requireInvalidNIC(t *testing.T, mutate func(*NIC)) {
	t.Helper()
	nic := testNIC()
	mutate(&nic)
	if err := validateNIC(nic); err == nil {
		t.Fatal("invalid NIC accepted")
	}
}

func TestCachePreservesRevocationsAcrossInstances(t *testing.T) {
	authority := testAuthority(t, t.TempDir())
	allowed, err := authority.SetAllowed(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	first, err := NewCache(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Put(allowed); err != nil {
		t.Fatal(err)
	}
	denied, err := authority.SetAllowed(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewCache(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Put(denied); err != nil {
		t.Fatal(err)
	}
	if err := first.Put(allowed); !errors.Is(err, ErrStaleGrant) {
		t.Fatalf("old grant replaced revocation: %v", err)
	}
	actual, err := first.Get(allowed.TargetID)
	if err != nil {
		t.Fatal(err)
	}
	if actual.Enabled || actual.Revision != denied.Revision {
		t.Fatalf("cached revocation = %+v", actual)
	}
}

func TestSenderConcurrentInstancesReserveOnePacketPair(t *testing.T) {
	grant := testGrant(t)
	dir := t.TempDir()
	var transmitted atomic.Int32
	options := SenderOptions{Discover: func(context.Context) (NIC, error) { return testNIC(), nil }, Transmit: func(context.Context, NIC, string) error { transmitted.Add(1); return nil }}
	first, err := NewSenderWithOptions(dir, options)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewSenderWithOptions(dir, options)
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	errors := make(chan error, 20)
	for index := range 20 {
		workers.Go(func() { _, err := []*Sender{first, second}[index%2].Send(context.Background(), grant); errors <- err })
	}
	workers.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if transmitted.Load() != 1 {
		t.Fatalf("transmitted %d packet pairs", transmitted.Load())
	}
}

func TestSenderRefusesAnotherLANAndRevokedPermission(t *testing.T) {
	authority := testAuthority(t, t.TempDir())
	grant, err := authority.SetAllowed(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	sender, err := NewSenderWithOptions(t.TempDir(), SenderOptions{Discover: func(context.Context) (NIC, error) {
		nic := testNIC()
		nic.GatewayMAC = "02:bb:cc:dd:ee:ff"
		return nic, nil
	}, Transmit: func(context.Context, NIC, string) error { t.Fatal("unexpected packet"); return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sender.Send(context.Background(), grant); !errors.Is(err, ErrNoLAN) {
		t.Fatalf("overlapping private subnet accepted: %v", err)
	}
	denied, err := authority.SetAllowed(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sender.Probe(context.Background(), denied); !errors.Is(err, ErrDisabled) {
		t.Fatalf("deny probe: %v", err)
	}
	if _, err := sender.Send(context.Background(), grant); !errors.Is(err, ErrStaleGrant) {
		t.Fatalf("revoked grant accepted: %v", err)
	}
}

func TestSenderAcceptsPriorRenewalAndPreservesCurrentGrant(t *testing.T) {
	now := time.Now().UTC().Add(-20 * 24 * time.Hour)
	authority := testAuthority(t, t.TempDir())
	authority.options.Now = func() time.Time { return now }
	initial, err := authority.SetAllowed(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(16 * 24 * time.Hour)
	renewed, err := authority.Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if renewed.Revision != initial.Revision || !samePolicy(initial, renewed) || !renewed.ExpiresAt.After(initial.ExpiresAt) {
		t.Fatal("expected a renewal with unchanged policy")
	}
	var transmitted int
	sender, err := NewSenderWithOptions(t.TempDir(), SenderOptions{
		Discover: func(context.Context) (NIC, error) { return testNIC(), nil },
		Observe:  func(context.Context, NIC) (State, error) { return Down, nil },
		Transmit: func(context.Context, NIC, string) error { transmitted++; return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sender.cache.Put(renewed); err != nil {
		t.Fatal(err)
	}
	if state, err := sender.Probe(context.Background(), initial); err != nil || state != Down {
		t.Fatalf("probe with valid prior renewal = %q, %v", state, err)
	}
	if sent, err := sender.Send(context.Background(), initial); err != nil || !sent {
		t.Fatalf("send with valid prior renewal = %v, %v", sent, err)
	}
	if transmitted != 1 {
		t.Fatalf("transmitted = %d", transmitted)
	}
	cached, err := sender.cache.Get(initial.TargetID)
	if err != nil {
		t.Fatal(err)
	}
	if !cached.IssuedAt.Equal(renewed.IssuedAt) || !cached.ExpiresAt.Equal(renewed.ExpiresAt) || !bytes.Equal(cached.Signature, renewed.Signature) {
		t.Fatal("older grant replaced the cached renewal")
	}
	denied, err := authority.SetAllowed(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := sender.cache.Put(denied); err != nil {
		t.Fatal(err)
	}
	if _, err := sender.Probe(context.Background(), initial); !errors.Is(err, ErrStaleGrant) {
		t.Fatalf("probe with revoked permission = %v", err)
	}
	if _, err := sender.Send(context.Background(), initial); !errors.Is(err, ErrStaleGrant) {
		t.Fatalf("send with revoked permission = %v", err)
	}
}

func TestSenderUnknownProbeStillAllowsExplicitWake(t *testing.T) {
	sender, err := NewSenderWithOptions(t.TempDir(), SenderOptions{Discover: func(context.Context) (NIC, error) { return testNIC(), nil }, Observe: func(context.Context, NIC) (State, error) { return Unknown, os.ErrNotExist }, Transmit: func(context.Context, NIC, string) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	grant := testGrant(t)
	state, err := sender.Probe(context.Background(), grant)
	if err != nil || state != Unknown {
		t.Fatalf("probe = %q, %v", state, err)
	}
	sent, err := sender.Send(context.Background(), grant)
	if err != nil || !sent {
		t.Fatalf("explicit wake = %v, %v", sent, err)
	}
}

func TestSenderAmbiguousFailureKeepsCooldown(t *testing.T) {
	var attempts int
	sender, err := NewSenderWithOptions(t.TempDir(), SenderOptions{Discover: func(context.Context) (NIC, error) { return testNIC(), nil }, Transmit: func(context.Context, NIC, string) error { attempts++; return net.ErrClosed }})
	if err != nil {
		t.Fatal(err)
	}
	grant := testGrant(t)
	if _, err := sender.Send(context.Background(), grant); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("send error: %v", err)
	}
	if sent, err := sender.Send(context.Background(), grant); err != nil || sent {
		t.Fatalf("retry = %v, %v", sent, err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d", attempts)
	}
}

func TestMagicPacketReachesUDPReceiver(t *testing.T) {
	receiver, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = receiver.Close() }()
	sender, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sender.Close() }()
	packet, err := MagicPacket(testNIC().MAC)
	if err != nil {
		t.Fatal(err)
	}
	if err := writePacket(sender, packet, receiver.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
	if err := receiver.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	actual := make([]byte, 200)
	n, _, err := receiver.ReadFromUDP(actual)
	if err != nil {
		t.Fatal(err)
	}
	mac, _ := net.ParseMAC(testNIC().MAC)
	want := append(bytes.Repeat([]byte{0xff}, 6), bytes.Repeat(mac, 16)...)
	if n != 102 || !bytes.Equal(actual[:n], want) {
		t.Fatalf("received packet %x", actual[:n])
	}
	if got := broadcast(netip.MustParsePrefix("10.32.0.0/12")); got.String() != "10.47.255.255" {
		t.Fatalf("broadcast = %s", got)
	}
}

func TestCacheRejectsOversizedFileAndPathTraversal(t *testing.T) {
	cache, err := NewCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Get("../../identity.key"); err == nil {
		t.Fatal("path traversal accepted")
	}
	grant := testGrant(t)
	if err := os.WriteFile(cache.path(grant.TargetID), bytes.Repeat([]byte{' '}, 65<<10), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Get(grant.TargetID); err == nil {
		t.Fatal("oversized cache accepted")
	}
	if info, err := os.Stat(filepath.Dir(cache.path(grant.TargetID))); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("cache directory permissions: %v, %v", info, err)
	}
}

func TestDiscoverLocalNetwork(t *testing.T) {
	if os.Getenv("MESH_WAKE_DISCOVERY_TEST") != "1" {
		t.Skip("set MESH_WAKE_DISCOVERY_TEST=1 on a machine with a wired default route")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	nic, err := Discover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateNIC(nic); err != nil {
		t.Fatal(err)
	}
	t.Logf("discovered wired IPv4 subnet %s and gateway fingerprint", nic.Prefix)
}
