package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/tailnet"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/update"
)

type evidenceDaemon struct {
	host  update.Host
	state string
	key   ed25519.PrivateKey
	stop  context.CancelFunc
	done  <-chan error
}

func startEvidenceDaemon(t *testing.T, versions []int) *evidenceDaemon {
	t.Helper()
	state := compactSocketTempDir(t)
	host, key, err := identity.LoadOrCreate(state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := update.OpenStore(state); err != nil {
		t.Fatal(err)
	}
	for seq, version := range versions {
		writeEvidenceFixture(t, state, fmt.Sprintf("host-ledger-%020d.json", seq+1), daemonEvidenceEntry(version, uint64(seq+1)))
	}
	if len(versions) > 0 {
		writeEvidenceFixture(t, state, "host-ledger.json", map[string]any{"schema": 1, "first": 1, "last": len(versions)})
	}
	listener, port := newTCPListener(t, "127.0.0.1:0")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	t.Cleanup(func() {
		cancel()
		if err := waitRuntime(t, done); err != nil {
			t.Error(err)
		}
	})
	go func() {
		done <- run(ctx, Config{StateDir: state, ConfigDir: t.TempDir(), TailnetPort: port}, runOptions{
			listen: useTCPListeners(listener), now: time.Now, bootID: func() string { return "evidence-boot" },
			discoverSelf: func(context.Context) (tailnet.Peer, error) {
				return tailnet.Peer{Name: "evidence.example.ts.net", Addrs: []string{"127.0.0.1"}}, nil
			},
			discoverPeers: func(context.Context) ([]tailnet.Peer, error) { return nil, nil }, reconcileInterval: time.Hour,
		})
	}()
	conn := dialUnixRuntime(t, SocketPath(state))
	_ = conn.Close()
	return &evidenceDaemon{host: update.Host{ID: host.ID, Endpoint: fmt.Sprintf("ws://127.0.0.1:%d/mesh", port), Platform: release.Platform{OS: "linux", Arch: "amd64"}}, state: state, key: key, stop: cancel, done: done}
}

func writeEvidenceFixture(t *testing.T, state, name string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "updates", name), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func daemonEvidenceEntry(version int, seq uint64) update.LedgerEntry {
	digest := sha256.Sum256([]byte(fmt.Sprint(version)))
	return update.LedgerEntry{Seq: seq, Request: fmt.Sprintf("%032x", version), Generation: uint64(max(version, 0)), Version: fmt.Sprintf("v0.1.%d", version), Metadata: hex.EncodeToString(digest[:]), Binary: hex.EncodeToString(digest[:]), From: hex.EncodeToString(digest[:]), Origin: "host", CommittedAt: time.Now().UTC().Add(-3 * time.Hour)}
}

func TestUpdateEvidenceThreeDaemonsUseDeviceAccessAndDurableOrder(t *testing.T) {
	a := startEvidenceDaemon(t, []int{2, 4, 6})
	b := startEvidenceDaemon(t, []int{3, 5, 7})
	dependent := startEvidenceDaemon(t, nil)
	for _, peer := range []*evidenceDaemon{a, b} {
		if err := identity.ApproveDevice(t.Context(), peer.state, dependent.host.ID); err != nil {
			t.Fatal(err)
		}
		if enrolled, err := update.AdministratorMembership(peer.state, dependent.host.ID); err != nil || enrolled {
			t.Fatalf("device gained update administrator: %v %v", enrolled, err)
		}
	}
	store, err := update.OpenStore(dependent.state)
	if err != nil {
		t.Fatal(err)
	}
	order := &update.HostOrder{Store: store, Client: update.Client{ID: dependent.host.ID, Key: dependent.key}, SavedFleet: func() (update.Fleet, error) { return update.Fleet{Members: []update.Host{a.host, b.host}}, nil }}
	policy := update.HostPolicy{After: []string{a.host.ID, b.host.ID}, Soak: 2 * time.Hour}
	limit, err := order.Refresh(t.Context(), policy, release.Build{Version: "v0.1.1", Platform: dependent.host.Platform}, time.Now())
	if err != nil || limit.Version != "v0.1.6" {
		t.Fatalf("three-daemon disjoint history = %#v, %v", limit, err)
	}
	client := order.Client
	var info update.Info
	if err := client.Call(t.Context(), a.host, "info", nil, &info); err == nil {
		t.Fatal("evidence device gained administrator controls")
	}
	reply, err := client.Evidence(t.Context(), a.host, update.EvidenceRequest{AfterSeq: 1, Limit: 1})
	if err != nil || !reply.More || len(reply.Entries) != 1 || reply.Entries[0].Seq != 2 {
		t.Fatalf("real bounded page = %#v, %v", reply, err)
	}
	if _, err := client.Evidence(t.Context(), a.host, update.EvidenceRequest{Limit: 65}); err == nil {
		t.Fatal("accepted oversized page")
	}
	target := update.LocalHost(a.state, a.host.ID)
	hostInfo, err := update.Exchange(t.Context(), target, protocol.Control{Type: protocol.TypeHostInfo, RequestID: "evidence-capability"})
	if err != nil || hostInfo.Host == nil || !hostInfo.Host.UpdateEvidenceSupported || hostInfo.Host.HostUpdate == nil || hostInfo.Host.HostUpdate.Mode != "manual" {
		t.Fatalf("host summary = %#v, %v", hostInfo.Host, err)
	}
	unknown, unknownKey, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	unapproved := update.Client{ID: unknown.ID, Key: unknownKey}
	if _, err := unapproved.Evidence(t.Context(), a.host, update.EvidenceRequest{Limit: 1}); err == nil {
		t.Fatal("unenrolled device read evidence")
	}
	if err := update.Trust(t.Context(), a.state, unknown.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := unapproved.Evidence(t.Context(), a.host, update.EvidenceRequest{Limit: 1}); err == nil {
		t.Fatal("administrator-only identity read evidence")
	}
	assertNoAnonymousEvidence(t, a)
}

func assertNoAnonymousEvidence(t *testing.T, peer *evidenceDaemon) {
	t.Helper()
	conn, err := transport.DialOnce(t.Context(), peer.host.Endpoint, transport.DialOptions{})
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	data, _ := (protocol.Control{Type: protocol.TypeUpdateEvidence, RequestID: "anonymous", Update: json.RawMessage(`{"limit":1}`)}).Encode()
	_ = conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: data})
	frame, err := conn.ReadFrame()
	if err != nil {
		return
	}
	response, err := protocol.DecodeControl(frame.Payload)
	if err == nil && response.Type == protocol.TypeUpdateEvidenceResult {
		t.Fatal("anonymous evidence reached daemon")
	}
}
