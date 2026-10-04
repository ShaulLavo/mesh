package cli

import (
	"bytes"
	"crypto/ed25519"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
	"golang.org/x/crypto/ssh"
)

func TestRenameHostChangesAuthenticatedDestinationOnly(t *testing.T) {
	f := namedDestination(t)
	if err := SaveHost(f.host); err != nil {
		t.Fatal(err)
	}
	path, err := ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	before := readNamingFixtureFile(t, path)
	claim, err := RenameHost(t.Context(), f.host, "work-pc", dialControlHost)
	if err != nil {
		t.Fatal(err)
	}
	if claim.ID != f.host.ID || claim.MachineName != "work-pc" || claim.Revision != 2 || f.names.Current() != claim {
		t.Fatal("rename did not change destination-owned declaration")
	}
	after := readNamingFixtureFile(t, path)
	if !bytes.Equal(before, after) {
		t.Fatal("rename rewrote addresses or dashboard settings")
	}
	cached, err := readNameCacheFixtureClaim(filepath.Dir(path), f.host.ID)
	if err != nil || cached != claim {
		t.Fatalf("authenticated owner cache: %+v, %v", cached, err)
	}
	retry, err := RenameHost(t.Context(), f.host, "work-pc", dialControlHost)
	if err != nil || retry != claim {
		t.Fatalf("same-name retry: %+v, %v", retry, err)
	}
}

func TestOwnMachineRenameNeedsNoSelfAdoption(t *testing.T) {
	for _, fixture := range []struct {
		name    string
		seed    byte
		useName bool
	}{
		{name: "nondash-id", seed: 0},
		{name: "leading-dash-id", seed: 41},
		{name: "nondash-name", seed: 0, useName: true},
		{name: "leading-dash-name", seed: 41, useName: true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			t.Setenv("MESH_CONFIG_DIR", t.TempDir())
			var owner identity.Host
			calls := 0
			socket, done := startDaemonControlServer(t, 2, func(conn transport.Conn, request protocol.Control) error {
				calls++
				if request.Type != protocol.TypeHostInfo {
					return fmt.Errorf("unexpected own control %s", request.Type)
				}
				info := protocol.HostInfo{ID: owner.ID, MeshIdentity: owner.ID, MachineName: "local-own", NameRevision: 1}
				if err := writeDaemonControl(conn, protocol.Control{Type: protocol.TypeHostInfoResult, RequestID: request.RequestID, Host: &info}); err != nil {
					return err
				}
				if calls == 1 {
					return nil
				}
				frame, err := conn.ReadFrame()
				if err != nil {
					return fmt.Errorf("read own rename fixture: %w", err)
				}
				rename, err := protocol.DecodeControl(frame.Payload)
				if err != nil {
					return fmt.Errorf("decode own rename fixture: %w", err)
				}
				if rename.Type != protocol.TypeHostRename || rename.Rename == nil || rename.Rename.TargetID != owner.ID || rename.Rename.MachineName != "local-new" || rename.Rename.ExpectedRevision != 1 {
					return fmt.Errorf("rename did not preserve exact owner and revision")
				}
				info.MachineName, info.NameRevision = "local-new", 2
				return writeDaemonControl(conn, protocol.Control{Type: protocol.TypeHostRenamed, RequestID: rename.RequestID, Host: &info})
			})
			stateDir := filepath.Dir(socket)
			t.Setenv("MESH_STATE_DIR", stateDir)
			// These seeds produce real public-key IDs on both sides of the option boundary.
			private := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{fixture.seed}, ed25519.SeedSize))
			block, err := ssh.MarshalPrivateKey(private, "")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(stateDir, "identity.key"), pem.EncodeToMemory(block), 0o600); err != nil {
				t.Fatal(err)
			}
			owner, err = identity.Load(stateDir)
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(owner.ID, "-") != (fixture.seed == 41) || !bytes.Equal(owner.PublicKey, private.Public().(ed25519.PublicKey)) {
				t.Fatal("fixture did not load the exact cryptographic identity and ID shape")
			}
			target := owner.ID
			if fixture.useName {
				target = "local-own"
			}
			out, _, err := executeCommand(t, Dependencies{}, "rename", target, "local-new")
			if err != nil {
				t.Fatalf("own rename without address-book entry: %v", err)
			}
			if !strings.Contains(out, "local-new (revision 2)") {
				t.Fatalf("own rename result: %s", out)
			}
			hosts, err := LoadHosts()
			if err != nil || len(hosts) != 0 {
				t.Fatalf("rename invented self adoption: %+v, %v", hosts, err)
			}
			awaitDaemonServer(t, done)
		})
	}
}

func TestRemoteRenameRefusesActualOwnNameBeforeRenameEffect(t *testing.T) {
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	var owner identity.Host
	socket, done := startDaemonCreateServer(t, func(conn transport.Conn, request protocol.Control) error {
		if request.Type != protocol.TypeHostInfo {
			return fmt.Errorf("unexpected own effect %s", request.Type)
		}
		return writeDaemonControl(conn, protocol.Control{Type: protocol.TypeHostInfoResult, RequestID: request.RequestID, Host: &protocol.HostInfo{ID: owner.ID, MeshIdentity: owner.ID, MachineName: "local-own", NameRevision: 1}})
	})
	stateDir := filepath.Dir(socket)
	t.Setenv("MESH_STATE_DIR", stateDir)
	var err error
	owner, _, err = identity.LoadOrCreate(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	remote := namedDestinationPeer(t)
	before := remote.names.Current()
	if _, err := RenameHost(t.Context(), remote.host, "local-own", dialControlHost); err == nil || !strings.Contains(err.Error(), "already claimed") {
		t.Fatalf("remote rename ignored actual own name: %v", err)
	}
	if remote.names.Current() != before {
		t.Fatal("refused collision changed destination declaration")
	}
	hosts, err := LoadHosts()
	if err != nil || len(hosts) != 0 {
		t.Fatalf("collision query invented self adoption: %+v %v", hosts, err)
	}
	awaitDaemonServer(t, done)
}
