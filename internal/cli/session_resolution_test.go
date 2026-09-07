package cli

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/worker"
)

func TestLocalSessionWithAdoptedSelfAliasIsNotAmbiguous(t *testing.T) {
	fixture := setupCommandTestHost(t)
	local, _, err := identity.LoadOrCreate(os.Getenv("MESH_STATE_DIR"))
	if err != nil {
		t.Fatal(err)
	}
	fixture.host.ID, fixture.host.MeshIdentity = local.ID, local.ID
	if err := writeHostConfig(hostConfig{Version: hostConfigVersion, Hosts: []HostRecord{fixture.host}}); err != nil {
		t.Fatal(err)
	}
	writeLocalSessionDir(t, "BVMX", worker.StateExited)
	stdout, _, err := executeCommand(t, Dependencies{DialHost: func(context.Context, HostRecord) (transport.Conn, error) {
		t.Error("local host alias was queried as another machine")
		return nil, errors.New("self queried")
	}}, "logs", "BVMX")
	if err != nil || !strings.Contains(stdout, "local output") {
		t.Fatalf("self alias hid local logs: %q, %v", stdout, err)
	}
}

func TestSameSessionIDOnDifferentIdentityRemainsAmbiguous(t *testing.T) {
	fixture := setupCommandTestHost(t)
	if _, _, err := identity.LoadOrCreate(os.Getenv("MESH_STATE_DIR")); err != nil {
		t.Fatal(err)
	}
	writeLocalSessionDir(t, "BVMX", worker.StateExited)
	fixture.sessionID, fixture.sessionState = "BVMX", worker.StateExited
	_, _, err := executeCommand(t, Dependencies{DialHost: fixture.dial}, "logs", "BVMX")
	if err == nil || !strings.Contains(err.Error(), "both on this host and on pc") {
		t.Fatalf("different host collision hidden: %v", err)
	}
}
