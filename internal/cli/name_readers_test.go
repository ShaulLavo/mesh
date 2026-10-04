package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/worker"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/machinename"
)

func TestOwnerDeclarationReplacesDashboardViewerLabel(t *testing.T) {
	f := namedDestination(t)
	state := StateView{Name: f.names.Current(), NameVerified: true, Connection: StateReachable}
	view := projectDashboardState(DashboardHost{ID: f.host.ID, MachineName: "viewer-label", Local: true}, state)
	if view.Host.MachineName != "destination" {
		t.Fatalf("dashboard retained per-viewer/local label %q", view.Host.MachineName)
	}
}

func TestOwnerNameStorageDiscardsOnlyObsoleteViewerLabel(t *testing.T) {
	f := namedDestination(t)
	path, err := ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	// The address book owns connection records; authenticated claims own names.
	record, err := json.Marshal(f.host)
	if err != nil {
		t.Fatal(err)
	}
	var legacy map[string]any
	if err := json.Unmarshal(record, &legacy); err != nil {
		t.Fatal(err)
	}
	legacy["alias"] = "obsolete-viewer-name"
	raw, err := json.Marshal(map[string]any{"version": 1, "hosts": []any{legacy}, "dashboard": map[string]any{"usageFeedURL": "https://usage.example.test/feed"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := machinename.RememberClaim(t.Context(), filepath.Dir(path), f.host.ID, f.names.Current()); err != nil {
		t.Fatal(err)
	}
	hosts, err := LoadHosts()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveArgument("obsolete-viewer-name", hosts); err == nil {
		t.Fatal("obsolete viewer name still selects a destination")
	}
	if len(hosts) != 1 || hosts[0].Endpoint != f.host.Endpoint || hosts[0].MeshIdentity != f.host.MeshIdentity {
		t.Fatal("unrelated connection state changed")
	}
	if err := SaveHost(hosts[0]); err != nil {
		t.Fatal(err)
	}
	after := readNamingFixtureFile(t, path)
	if !bytes.Contains(after, []byte("https://usage.example.test/feed")) {
		t.Fatal("name cutover discarded dashboard settings")
	}
	if bytes.Contains(after, []byte(`"alias"`)) {
		t.Fatal("product viewer Alias remains persisted")
	}
}

func TestOwnerNameSessionTableReadsDeclaration(t *testing.T) {
	f := namedDestination(t)
	f.host.MachineName, f.host.NameRevision = "destination", 1
	var out bytes.Buffer
	if err := writeProtocolSessions(&out, commandTestTime, []HostSessions{{Host: f.host, Sessions: []protocol.SessionInfo{{ID: "7K3D", HostID: f.host.ID, State: "running", CreatedAt: commandTestTime, Command: []string{"shell"}}}}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "destination") || !strings.Contains(out.String(), "7K3D") || strings.Contains(out.String(), "viewer-label") {
		t.Fatal("session table uses per-viewer label")
	}
}

func TestUpdateBareNameRevalidatesOwnerBeforeEffects(t *testing.T) {
	f := namedDestination(t)
	f.host.MachineName, f.host.NameRevision = "destination", 1
	if err := saveNamedTestHost(t, f.host); err != nil {
		t.Fatal(err)
	}
	app := application{dependencies: Dependencies{DialControl: dialControlHost}}
	resolved, intents, err := app.resolveUpdateNameIntents(t.Context(), updateOptions{hosts: []string{"destination"}})
	if err != nil || len(intents) != 1 || len(resolved.hosts) != 1 || resolved.hosts[0] != f.host.ID {
		t.Fatalf("resolved update intent: %+v, %v", resolved, err)
	}
	if _, _, err := f.names.Rename(t.Context(), f.host.ID, "renamed", 1); err != nil {
		t.Fatal(err)
	}
	if err := verifyUpdateNameIntents(t.Context(), intents, dialControlHost); err == nil {
		t.Fatal("stale update name intent reached effects")
	}
	if _, _, err := app.resolveUpdateNameIntents(t.Context(), updateOptions{hosts: []string{f.host.ID}}); err != nil {
		t.Fatalf("exact ID after owner rename: %v", err)
	}
}

func TestLocalDeclarationRefusalCannotBecomeIDOnlySuccess(t *testing.T) {
	for _, mode := range []string{"malformed", "refused", "mixed"} {
		t.Run(mode, func(t *testing.T) {
			var owner identity.Host
			socket, done := startDaemonCreateServer(t, func(conn transport.Conn, request protocol.Control) error {
				if request.Type != protocol.TypeHostInfo {
					return fmt.Errorf("unexpected local effect %s", request.Type)
				}
				response := protocol.Control{Type: protocol.TypeHostInfoResult, RequestID: request.RequestID, Host: &protocol.HostInfo{ID: owner.ID, MeshIdentity: owner.ID, MachineName: "local-own", NameRevision: 1}}
				switch mode {
				case "malformed":
					response.Host.NameRevision = 0
				case "refused":
					response = protocol.Control{Type: protocol.TypeError, RequestID: request.RequestID, Message: "fixture refused"}
				case "mixed":
					response.Sessions = []protocol.SessionInfo{{ID: "7K3D"}}
				}
				return writeDaemonControl(conn, response)
			})
			stateDir := filepath.Dir(socket)
			var err error
			owner, _, err = identity.LoadOrCreate(stateDir)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := localNameRecord(t.Context(), stateDir); err == nil {
				t.Fatal("invalid owner declaration was hidden as ID-only success")
			}
			awaitDaemonServer(t, done)
		})
	}
}

func TestLocalUpdaterLabelReadsOwnerWithoutSelfAdoption(t *testing.T) {
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	var owner identity.Host
	socket, done := startDaemonCreateServer(t, func(conn transport.Conn, request protocol.Control) error {
		if request.Type != protocol.TypeHostInfo {
			return fmt.Errorf("unexpected local effect %s", request.Type)
		}
		return writeDaemonControl(conn, protocol.Control{Type: protocol.TypeHostInfoResult, RequestID: request.RequestID, Host: &protocol.HostInfo{ID: owner.ID, MeshIdentity: owner.ID, MachineName: "local-own", NameRevision: 2}})
	})
	stateDir := filepath.Dir(socket)
	t.Setenv("MESH_STATE_DIR", stateDir)
	var err error
	owner, _, err = identity.LoadOrCreate(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	shown, err := declaredUpdateTargets(t.Context(), []update.Target{{Host: update.Host{ID: owner.ID, MachineName: "os-hostname"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(shown) != 1 || shown[0].Host.Label() != "local-own" {
		t.Fatalf("local owner declaration: %+v", shown)
	}
	hosts, err := LoadHosts()
	if err != nil || len(hosts) != 0 {
		t.Fatalf("local display created self adoption: %+v, %v", hosts, err)
	}
	awaitDaemonServer(t, done)
}

func TestRetainedSessionTableNameIsExplicit(t *testing.T) {
	var out bytes.Buffer
	host := HostRecord{ID: "exact-owner", MachineName: "garden", NameRevision: 1}
	if _, err := writeSessionList(&out, commandTestTime, []HostSessions{{Host: host, Sessions: []protocol.SessionInfo{{ID: "7K3D", HostID: host.ID, State: "running", CreatedAt: commandTestTime}}}}, fullListView); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "garden · last known name") {
		t.Fatalf("retained list name has no provenance: %s", &out)
	}
}

func TestBareRemoteNameRefusesKnownOwnMachineCollisionBeforeEffects(t *testing.T) {
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	var owner identity.Host
	socket, done := startDaemonCreateServer(t, func(conn transport.Conn, request protocol.Control) error {
		if request.Type != protocol.TypeHostInfo {
			return fmt.Errorf("unexpected own effect %s", request.Type)
		}
		return writeDaemonControl(conn, protocol.Control{Type: protocol.TypeHostInfoResult, RequestID: request.RequestID, Host: &protocol.HostInfo{ID: owner.ID, MeshIdentity: owner.ID, MachineName: "garden", NameRevision: 1}})
	})
	stateDir := filepath.Dir(socket)
	t.Setenv("MESH_STATE_DIR", stateDir)
	var err error
	owner, _, err = identity.LoadOrCreate(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	remote := namedDestinationPeer(t)
	if _, _, err := remote.names.Rename(t.Context(), remote.host.ID, "garden", 1); err != nil {
		t.Fatal(err)
	}
	remote.host.MachineName, remote.host.NameRevision = "garden", 2
	if err := saveNamedTestHost(t, remote.host); err != nil {
		t.Fatal(err)
	}
	hosts, err := LoadHosts()
	if err != nil {
		t.Fatal(err)
	}
	target, err := ResolveArgument("garden", hosts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := listRemoteHost(t.Context(), *target.Host, dialControlHost); err == nil || !strings.Contains(err.Error(), "shared by hosts") {
		t.Fatalf("own collision not refused: %v", err)
	}
	if remote.operations.Load() != 0 {
		t.Fatal("ambiguous name reached a session operation")
	}
	if _, err := listRemoteHost(t.Context(), remote.host, dialControlHost); err != nil {
		t.Fatalf("exact owner after conflict: %v", err)
	}
	if remote.operations.Load() != 1 {
		t.Fatal("exact-ID control did not reach its owner")
	}
	awaitDaemonServer(t, done)
}

func TestDaemonSessionListReadsOwnDeclarationWithoutSelfAdoption(t *testing.T) {
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	var owner identity.Host
	socket, done := startDaemonControlServer(t, 2, func(conn transport.Conn, request protocol.Control) error {
		response := protocol.Control{RequestID: request.RequestID}
		switch request.Type {
		case protocol.TypeList:
			response.Type, response.Sessions = protocol.TypeListed, []protocol.SessionInfo{{ID: "7K3D", HostID: owner.ID, State: "running", Command: []string{"shell"}, CreatedAt: commandTestTime}}
		case protocol.TypeHostInfo:
			response.Type, response.Host = protocol.TypeHostInfoResult, &protocol.HostInfo{ID: owner.ID, MeshIdentity: owner.ID, MachineName: "local-own", NameRevision: 2}
		default:
			return fmt.Errorf("unexpected list control %s", request.Type)
		}
		return writeDaemonControl(conn, response)
	})
	stateDir := filepath.Dir(socket)
	t.Setenv("MESH_STATE_DIR", stateDir)
	var err error
	owner, _, err = identity.LoadOrCreate(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := executeCommand(t, Dependencies{}, "ls", "--daemon")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "local-own") || strings.Contains(out, "last known name") {
		t.Fatalf("own list declaration: %s", out)
	}
	hosts, err := LoadHosts()
	if err != nil || len(hosts) != 0 {
		t.Fatalf("list invented self adoption: %+v %v", hosts, err)
	}
	awaitDaemonServer(t, done)
}

func readNamingFixtureFile(t *testing.T, path string) []byte {
	t.Helper()
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close() //nolint:errcheck // owned fixture root cleanup
	contents, err := root.ReadFile(filepath.Base(path))
	if err != nil {
		t.Fatal(err)
	}
	return contents
}

func TestMixedSessionListKeepsOwnDeclarationProvenance(t *testing.T) {
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	var owner identity.Host
	socket, done := startDaemonControlServer(t, 2, func(conn transport.Conn, request protocol.Control) error {
		response := protocol.Control{RequestID: request.RequestID}
		switch request.Type {
		case protocol.TypeList:
			response.Type = protocol.TypeListed
		case protocol.TypeHostInfo:
			response.Type, response.Host = protocol.TypeHostInfoResult, &protocol.HostInfo{ID: owner.ID, MeshIdentity: owner.ID, MachineName: "destination", NameRevision: 2}
		default:
			return fmt.Errorf("unexpected mixed-list control %s", request.Type)
		}
		return writeDaemonControl(conn, response)
	})
	stateDir := filepath.Dir(socket)
	t.Setenv("MESH_STATE_DIR", stateDir)
	var err error
	owner, _, err = identity.LoadOrCreate(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	writeLocalSessionDir(t, "70C7", worker.StateExited)
	remote := namedDestinationPeer(t)
	remote.host.MachineName, remote.host.NameRevision = "destination", 1
	if err := saveNamedTestHost(t, remote.host); err != nil {
		t.Fatal(err)
	}
	out, _, err := executeCommand(t, Dependencies{}, "ls", "--all")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "70C7") {
			continue
		}
		if !strings.Contains(line, "conflict") || strings.Contains(line, "last known name") {
			t.Fatalf("mixed list lost actual own conflict/provenance: %s", line)
		}
	}
	if !strings.Contains(out, "70C7") {
		t.Fatal("mixed list lost own session")
	}
	hosts, err := LoadHosts()
	if err != nil || len(hosts) != 1 || hosts[0].ID != remote.host.ID {
		t.Fatal("mixed list created self adoption")
	}
	awaitDaemonServer(t, done)
}
