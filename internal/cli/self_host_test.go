package cli

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/transport"
)

func TestListDoesNotCreateIdentity(t *testing.T) {
	host := setupCommandTestHost(t)
	stateDir := os.Getenv("MESH_STATE_DIR")
	if _, _, err := executeCommand(t, Dependencies{DialHost: host.dial, DialControl: host.dial}, "ls"); err != nil {
		t.Fatal(err)
	}
	if _, err := identity.Load(stateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("listing created an identity; load error = %v", err)
	}
}

func TestPickerCatalogOmitsSelfAdoptedHost(t *testing.T) {
	fixture := setupCommandTestHost(t)
	local, _, err := identity.LoadOrCreate(os.Getenv("MESH_STATE_DIR"))
	if err != nil {
		t.Fatal(err)
	}
	self := fixture.host
	self.ID, self.MeshIdentity, self.Alias = local.ID, local.ID, "home"
	if err := SaveHost(self); err != nil {
		t.Fatal(err)
	}
	called := false
	_, _, err = executeCommand(t, Dependencies{
		DialHost: func(ctx context.Context, host HostRecord) (transport.Conn, error) {
			if host.ID == local.ID {
				t.Error("picker queried this host over the network")
				return nil, errors.New("unexpected self query")
			}
			return fixture.dial(ctx, host)
		},
		Picker: func(ctx context.Context, input PickerInput) (PickerSelection, error) {
			called = true
			if len(input.Hosts) != 2 || !input.Hosts[0].Local || input.Hosts[0].Host.ID != local.ID || input.Hosts[1].Host.Alias != "pc" {
				t.Errorf("picker hosts = %+v, want this host and pc only", input.Hosts)
			}
			remote, err := input.LoadHosts(ctx)
			if err != nil || len(remote) != 1 || remote[0].Host.Alias != "pc" {
				t.Errorf("picker remote hosts = %+v, %v, want pc only", remote, err)
			}
			return PickerSelection{}, nil
		},

		DialControl: func(ctx context.Context, host HostRecord) (transport.Conn, error) {
			if host.ID == local.ID {
				t.Error("picker queried this host over the network")
				return nil, errors.New("unexpected self query")
			}
			return fixture.dial(ctx, host)
		},
	})
	if err != nil || !called {
		t.Fatalf("picker returned %v, called = %t", err, called)
	}
}

func TestResolutionOmitsSelfWhenSessionIsNotLocal(t *testing.T) {
	fixture := setupCommandTestHost(t)
	local, _, err := identity.LoadOrCreate(os.Getenv("MESH_STATE_DIR"))
	if err != nil {
		t.Fatal(err)
	}
	self := fixture.host
	self.ID, self.MeshIdentity, self.Alias = local.ID, local.ID, "home"
	app := &application{dependencies: Dependencies{DialControl: func(ctx context.Context, host HostRecord) (transport.Conn, error) {
		if host.ID == local.ID {
			t.Error("resolution queried self for a session missing locally")
			return nil, errors.New("unexpected self query")
		}
		return fixture.dial(ctx, host)
	}}}
	resolved, err := app.resolveSession(t.Context(), []HostRecord{self, fixture.host}, "7K3D")
	if err != nil || resolved.host == nil || resolved.host.Alias != "pc" {
		t.Fatalf("resolved = %+v, %v, want remote pc", resolved, err)
	}
}

func TestSelfHostFilterRequiresBothIdentityFields(t *testing.T) {
	stateDir := t.TempDir()
	self, _, err := identity.LoadOrCreate(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	hosts := []HostRecord{
		{Alias: "self", ID: self.ID, MeshIdentity: self.ID},
		{Alias: "different-key", ID: self.ID, MeshIdentity: "other-key"},
		{Alias: "different-id", ID: "other-id", MeshIdentity: self.ID},
	}
	remote, alias := withoutThisHost(stateDir, hosts)
	if alias != "self" || len(remote) != 2 || remote[0].Alias != hosts[1].Alias || remote[1].Alias != hosts[2].Alias {
		t.Fatalf("filtered = %+v, alias %q, want both mismatched identities retained", remote, alias)
	}
}
