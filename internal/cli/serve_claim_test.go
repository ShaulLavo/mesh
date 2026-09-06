package cli

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/tunnel"
)

func setupTunnelCLI(t *testing.T) (HostRecord, ed25519.PrivateKey, string) {
	t.Helper()
	stateDir := t.TempDir()
	t.Setenv("MESH_STATE_DIR", stateDir)
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	_, key, err := identity.LoadOrCreate(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	edge, _, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	host := HostRecord{Alias: "vps", ID: edge.ID, MeshIdentity: edge.ID,
		TailscaleName: "vps.example.ts.net", Endpoint: "ws://100.64.0.2:7337/mesh"}
	if err := SaveHost(host); err != nil {
		t.Fatal(err)
	}
	return host, key, stateDir
}

func TestTunnelClaimCancellationDoesNotAllocateOrSend(t *testing.T) {
	host, _, stateDir := setupTunnelCLI(t)
	confirmations := 0
	stdout, _, err := executeCommand(t, Dependencies{
		ConfirmPublic: func(_ context.Context, confirmation PublicConfirmation) (bool, error) {
			confirmations++
			if !confirmation.TunnelClaim || confirmation.URL != "https://blog.shaulavo.dev" || confirmation.Host.ID != host.ID || confirmation.Service.Name != "" {
				t.Errorf("confirmation = %#v", confirmation)
			}
			return false, nil
		},
		DialControl: func(context.Context, HostRecord) (transport.Conn, error) {
			t.Error("cancelled claim dialed the edge")
			return nil, errors.New("unexpected dial")
		},
	}, "serve", "claim", "vps", "blog.shaulavo.dev")
	if err != nil || confirmations != 1 || !strings.Contains(stdout, "cancelled") {
		t.Fatalf("output=%q confirmations=%d error=%v", stdout, confirmations, err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, catalogDatabaseName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled claim touched SQLite: %v", err)
	}
}

func TestTunnelClaimYesAndOwnerReleaseUseLocalIdentity(t *testing.T) {
	host, key, stateDir := setupTunnelCLI(t)
	var mutations []tunnel.Mutation
	dependencies := Dependencies{
		ConfirmPublic: func(context.Context, PublicConfirmation) (bool, error) {
			t.Error("--yes prompted for confirmation")
			return false, nil
		},
		DialControl: serviceRemoteDial(host, func(request protocol.Control) protocol.Control {
			if request.Type != protocol.TypeTunnelClaim || request.TunnelMutation == nil {
				t.Errorf("unexpected request %#v", request)
				return protocol.Control{Type: protocol.TypeError, Message: "unexpected request"}
			}
			mutation := *request.TunnelMutation
			digest, err := tunnel.Verify(mutation, host.ID)
			if err != nil || mutation.ClaimantID != tunnel.KeyID(key.Public().(ed25519.PublicKey)) {
				t.Errorf("invalid local signature: mutation=%#v err=%v", mutation, err)
			}
			mutations = append(mutations, mutation)
			return protocol.Control{Type: protocol.TypeTunnelClaimed, TunnelAck: &tunnel.Ack{Sequence: mutation.Sequence, Digest: digest}}
		}),
	}
	stdout, _, err := executeCommand(t, dependencies, "serve", "claim", "vps", "blog.shaulavo.dev", "--yes")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"blog.shaulavo.dev", "ssh -N", "ExitOnForwardFailure=yes", "IdentitiesOnly=yes", "-p 2222", filepath.Join(stateDir, "identity.key"), "blog.shaulavo.dev:80:localhost:3000", host.TailscaleName} {
		if !strings.Contains(stdout, required) {
			t.Errorf("output %q omits %q", stdout, required)
		}
	}
	if _, _, err := executeCommand(t, dependencies, "unserve", "blog.shaulavo.dev", "--host", "vps"); err != nil {
		t.Fatal(err)
	}
	if len(mutations) != 2 || mutations[0].Action != tunnel.Create || mutations[1].Action != tunnel.Release || mutations[1].Sequence != mutations[0].Sequence+1 {
		t.Fatalf("mutations = %#v", mutations)
	}
}

func TestTunnelRemoteAcknowledgementAndEdgePin(t *testing.T) {
	host, key, _ := setupTunnelCLI(t)
	mutation, err := tunnel.Sign(key, host.ID, tunnel.Create, "blog.shaulavo.dev", 1)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := tunnel.Verify(mutation, host.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, ack := range []tunnel.Ack{{Sequence: 2, Digest: digest}, {Sequence: 1, Digest: "wrong"}} {
		_, err := sendTunnelMutation(context.Background(), host, serviceRemoteDial(host, func(protocol.Control) protocol.Control {
			return protocol.Control{Type: protocol.TypeTunnelClaimed, TunnelAck: &ack}
		}), mutation)
		if err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Fatalf("ack=%#v error=%v", ack, err)
		}
	}
	wrongHost := host
	wrongHost.ID = "changed"
	_, err = sendTunnelMutation(context.Background(), host, serviceRemoteDial(wrongHost, func(protocol.Control) protocol.Control {
		t.Error("mutation sent despite changed edge identity")
		return protocol.Control{}
	}), mutation)
	if err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("changed identity error = %v", err)
	}
	refusal := tunnel.Ack{Sequence: mutation.Sequence, Digest: digest, Error: "claim held by another owner"}
	got, err := sendTunnelMutation(context.Background(), host, serviceRemoteDial(host, func(protocol.Control) protocol.Control {
		return protocol.Control{Type: protocol.TypeTunnelClaimed, TunnelAck: &refusal}
	}), mutation)
	if err != nil || got != refusal {
		t.Fatalf("definitive refusal was lost: ack=%#v error=%v", got, err)
	}
}

func TestTunnelCommandRetriesUnacknowledgedMutationExactly(t *testing.T) {
	host, _, _ := setupTunnelCLI(t)
	var attempts []tunnel.Mutation
	dependencies := Dependencies{DialControl: serviceRemoteDial(host, func(request protocol.Control) protocol.Control {
		if request.TunnelMutation == nil {
			t.Error("request has no tunnel mutation")
			return protocol.Control{Type: protocol.TypeError}
		}
		mutation := *request.TunnelMutation
		attempts = append(attempts, mutation)
		digest, err := tunnel.Verify(mutation, host.ID)
		if err != nil {
			t.Error(err)
		}
		if len(attempts) == 1 {
			digest = "not-the-mutation-digest"
		}
		return protocol.Control{Type: protocol.TypeTunnelClaimed, TunnelAck: &tunnel.Ack{Sequence: mutation.Sequence, Digest: digest}}
	})}
	_, _, err := executeCommand(t, dependencies, "serve", "claim", "vps", "blog.shaulavo.dev", "--yes")
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched acknowledgement error=%v", err)
	}
	if _, _, err := executeCommand(t, dependencies, "serve", "claim", "vps", "blog.shaulavo.dev", "--yes"); err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 2 || !reflect.DeepEqual(attempts[0], attempts[1]) {
		t.Fatalf("retry changed the stored signed attempt: %#v", attempts)
	}
}

func TestTunnelDefinitiveRefusalDoesNotStrandOwnerRelease(t *testing.T) {
	host, _, _ := setupTunnelCLI(t)
	var actions []tunnel.Action
	dependencies := Dependencies{DialControl: serviceRemoteDial(host, func(request protocol.Control) protocol.Control {
		if request.TunnelMutation == nil {
			t.Error("request has no tunnel mutation")
			return protocol.Control{Type: protocol.TypeError}
		}
		mutation := *request.TunnelMutation
		actions = append(actions, mutation.Action)
		digest, err := tunnel.Verify(mutation, host.ID)
		if err != nil {
			t.Error(err)
		}
		ack := tunnel.Ack{Sequence: mutation.Sequence, Digest: digest}
		if mutation.Action == tunnel.Create {
			ack.Error = "owner no longer authorized\n\x1b[31m"
		}
		return protocol.Control{Type: protocol.TypeTunnelClaimed, TunnelAck: &ack}
	})}
	_, _, err := executeCommand(t, dependencies, "serve", "claim", "vps", "blog.shaulavo.dev", "--yes")
	if err == nil || !strings.Contains(err.Error(), "owner no longer authorized") || strings.ContainsAny(err.Error(), "\n\x1b") {
		t.Fatalf("refusal error=%v", err)
	}
	if _, _, err := executeCommand(t, dependencies, "unserve", "blog.shaulavo.dev", "--host", "vps"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actions, []tunnel.Action{tunnel.Create, tunnel.Release}) {
		t.Fatalf("release retried a definitively refused create: %v", actions)
	}
}

func TestTunnelLocalReleaseUsesOnlyUnixSocket(t *testing.T) {
	socket, done := startDaemonCreateServer(t, func(conn transport.Conn, request protocol.Control) error {
		if request.Type != protocol.TypeTunnelRecover || request.TunnelName != "blog.shaulavo.dev" || request.TunnelMutation != nil {
			return fmt.Errorf("unexpected local recovery request %#v", request)
		}
		return writeDaemonControl(conn, protocol.Control{Type: protocol.TypeTunnelRecovered, RequestID: request.RequestID, TunnelName: request.TunnelName})
	})
	t.Setenv("MESH_STATE_DIR", filepath.Dir(socket))
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	stdout, _, err := executeCommand(t, Dependencies{
		DialControl: func(context.Context, HostRecord) (transport.Conn, error) {
			t.Error("local recovery dialed a remote host")
			return nil, errors.New("unexpected remote dial")
		},
	}, "unserve", "blog.shaulavo.dev", "--local-edge")
	if err != nil || !strings.Contains(stdout, "released blog.shaulavo.dev on the local edge") {
		t.Fatalf("output=%q error=%v", stdout, err)
	}
	awaitDaemonServer(t, done)
	_, _, err = executeCommand(t, Dependencies{}, "unserve", "blog.shaulavo.dev", "--local-edge", "--host", "vps")
	if err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("combined local and remote flags error=%v", err)
	}
}

func TestTunnelClaimRejectsNamesAndMissingIdentityBeforeSend(t *testing.T) {
	_, _, stateDir := setupTunnelCLI(t)
	for _, name := range []string{"blog", "*.shaulavo.dev", "127.0.0.1", "shaulavo.dev", "nested.blog.shaulavo.dev"} {
		_, _, err := executeCommand(t, Dependencies{}, "serve", "claim", "vps", name, "--yes")
		if err == nil {
			t.Errorf("accepted invalid hostname %q", name)
		}
	}
	if err := os.Remove(filepath.Join(stateDir, "identity.key")); err != nil {
		t.Fatal(err)
	}
	_, _, err := executeCommand(t, Dependencies{}, "serve", "claim", "vps", "blog.shaulavo.dev", "--yes")
	if err == nil || !strings.Contains(err.Error(), "load local Mesh identity") {
		t.Fatalf("missing identity error=%v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "identity.key")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("claim recreated missing identity: %v", err)
	}
}
