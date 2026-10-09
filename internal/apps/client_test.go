package apps

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/tailnet"
	"github.com/shaul/mesh/internal/transport"
)

func clientTestKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}
func clientTestPeer(key ed25519.PrivateKey) Peer {
	return Peer{Identity: identityFor(key), TailscaleName: "registry.example.ts.net", ControlPort: 7337, WebSocketPath: "/mesh"}
}
func clientTestPeers(ctx context.Context) ([]tailnet.Peer, error) {
	return []tailnet.Peer{{Name: "registry.example.ts.net", Online: true, Addrs: []string{"100.64.0.2"}}}, nil
}

type registryTestConn struct {
	mu      sync.Mutex
	reply   func(protocol.Control) (protocol.Control, error)
	frame   protocol.Frame
	err     error
	closed  chan struct{}
	once    sync.Once
	stalled bool
}

func (c *registryTestConn) WriteFrame(frame protocol.Frame) error {
	request, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return fmt.Errorf("fixture: decode control request: %w", err)
	}
	reply, err := c.reply(request)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = err
	data, encodeErr := reply.Encode()
	c.frame = protocol.Frame{Kind: protocol.KindControl, Payload: data}
	if encodeErr != nil {
		return fmt.Errorf("fixture: encode control response: %w", encodeErr)
	}
	return nil
}
func (c *registryTestConn) ReadFrame() (protocol.Frame, error) {
	if c.stalled {
		<-c.closed
		return protocol.Frame{}, io.ErrClosedPipe
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.frame, c.err
}
func (c *registryTestConn) Close() error { c.once.Do(func() { close(c.closed) }); return nil }
func TestRegistryClientVerifiesPinRequestIDSignerAndSequence(t *testing.T) {
	for _, failure := range []string{"none", "host identity", "host mesh identity", "request ID", "reply signer", "reply sequence", "reply target", "reply expired"} {
		t.Run(failure, func(t *testing.T) { testRegistryExchangeFailure(t, failure) })
	}
}
func testRegistryExchangeFailure(t *testing.T, failure string) {
	t.Helper()
	owner, registry := clientTestKey(t), clientTestKey(t)
	target := clientTestPeer(registry)
	now := time.Now().UTC()
	operations := 0
	reply := func(request protocol.Control) (protocol.Control, error) {
		if request.Type == protocol.TypeHostInfo {
			host := protocol.HostInfo{ID: target.Identity, MeshIdentity: target.Identity}
			if failure == "host identity" {
				host.ID = identityFor(owner)
			}
			if failure == "host mesh identity" {
				host.MeshIdentity = identityFor(owner)
			}
			return protocol.Control{Type: protocol.TypeHostInfoResult, RequestID: request.RequestID, Host: &host}, nil
		}
		operations++
		key, sequence, to, issued := registry, uint64(1), identityFor(owner), now
		if failure == "reply signer" {
			key = owner
		}
		if failure == "reply sequence" {
			sequence = 2
		}
		if failure == "reply target" {
			to = target.Identity
		}
		if failure == "reply expired" {
			issued = now.Add(-time.Hour)
		}
		signed, err := Sign("mesh-app/response/v1", to, sequence, map[string]string{"requestId": "result"}, key, issued)
		if err != nil {
			return protocol.Control{}, err
		}
		data, err := json.Marshal(signed)
		if err != nil {
			return protocol.Control{}, fmt.Errorf("fixture: encode signed reply: %w", err)
		}
		id := request.RequestID
		if failure == "request ID" {
			id = "wrong"
		}
		return protocol.Control{Type: protocol.TypeAppRegistry, RequestID: id, App: data}, nil
	}
	client, err := NewRegistryClient(RegistryClientConfig{Key: owner, Target: target, Peers: clientTestPeers, Now: func() time.Time { return now }, Dial: func(context.Context, string) (transport.Conn, error) {
		return &registryTestConn{reply: reply, closed: make(chan struct{})}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	signed, err := Sign("mesh-app/request/v1", target.Identity, 1, Request{Action: "list"}, owner, now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Exchange(context.Background(), signed)
	if (err == nil) != (failure == "none") {
		t.Fatalf("exchange error=%v for %s", err, failure)
	}
	if (failure == "host identity" || failure == "host mesh identity") && operations != 0 {
		t.Fatal("operation sent before registry pin matched")
	}
}
func TestRegistryClientRejectsForeignOwnerBeforeDial(t *testing.T) {
	owner, registry := clientTestKey(t), clientTestKey(t)
	target := clientTestPeer(registry)
	dialed := false
	client, err := NewRegistryClient(RegistryClientConfig{Key: owner, Target: target, Peers: clientTestPeers, Dial: func(context.Context, string) (transport.Conn, error) {
		dialed = true
		return nil, errors.New("unexpected dial")
	}})
	if err != nil {
		t.Fatal(err)
	}
	signed, err := Sign("mesh-app/request/v1", target.Identity, 1, Request{Action: "list"}, registry, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Exchange(context.Background(), signed); err == nil || dialed {
		t.Fatalf("foreign operation: error=%v dialed=%v", err, dialed)
	}
}
func TestRegistryClientCancellationClosesStalledConnection(t *testing.T) {
	owner, registry := clientTestKey(t), clientTestKey(t)
	target := clientTestPeer(registry)
	connection := &registryTestConn{closed: make(chan struct{}), stalled: true, reply: func(protocol.Control) (protocol.Control, error) { return protocol.Control{}, nil }}
	client, err := NewRegistryClient(RegistryClientConfig{Key: owner, Target: target, Peers: clientTestPeers, Dial: func(context.Context, string) (transport.Conn, error) { return connection, nil }, RequestTimeout: 25 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	signed, err := Sign("mesh-app/request/v1", target.Identity, 1, Request{Action: "list"}, owner, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Exchange(context.Background(), signed); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stalled exchange=%v", err)
	}
	select {
	case <-connection.closed:
	default:
		t.Fatal("stalled transport was not closed")
	}
}
func TestPrivateAppPeerResolverRejectsAmbiguousOrOffTailnetPeers(t *testing.T) {
	for _, peers := range [][]tailnet.Peer{
		{{Name: "registry.example.ts.net", Online: true, Addrs: []string{"203.0.113.2"}}},
		{{Name: "registry.example.ts.net", Addrs: []string{"100.64.0.2"}}},
		{{Name: "registry.example.ts.net", Online: true, Addrs: []string{"100.64.0.2"}}, {Name: "registry.example.ts.net", Online: true, Addrs: []string{"100.64.0.3"}}},
	} {
		discover := func(context.Context) ([]tailnet.Peer, error) { return peers, nil }
		if _, err := resolveTailscalePeer(context.Background(), discover, "registry.example.ts.net", 7337); err == nil {
			t.Fatal("invalid peer accepted")
		}
	}
	endpoint, err := resolveTailscalePeer(context.Background(), clientTestPeers, "registry.example.ts.net", 7337)
	if err != nil || endpoint != netip.MustParseAddrPort("100.64.0.2:7337") {
		t.Fatalf("endpoint=%v error=%v", endpoint, err)
	}
	if err := ValidateOriginEndpoint(endpoint, 8443); err == nil {
		t.Fatal("wrong origin port accepted")
	}
}
func TestPrivateRegistryConfigRejectsPublicListenerAndUnknownRoles(t *testing.T) {
	key := clientTestKey(t)
	peer := clientTestPeer(key)
	for _, address := range []string{"127.0.0.1:8445", "0.0.0.0:443", "203.0.113.4:443", "127.0.0.1:0"} {
		config := RegistryHostConfig{ListenAddress: address, CertificateRenewerID: peer.Identity, Origins: []Peer{peer}}
		data, err := json.Marshal(config)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "registry.json")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		_, err = LoadRegistryConfig(path)
		if (err == nil) != (address == "127.0.0.1:8445") {
			t.Fatalf("address=%s error=%v", address, err)
		}
	}
	path := filepath.Join(t.TempDir(), "unknown.json")
	if err := os.WriteFile(path, []byte(`{"mode":"proxy"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRegistryConfig(path); err == nil {
		t.Fatal("unknown registry config field accepted")
	}
}
