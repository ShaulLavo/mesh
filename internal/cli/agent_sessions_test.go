package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/transport"
)

func TestAgentSessionUsage(t *testing.T) {
	for _, flags := range [][]string{{"--limit", "0"}, {"--limit", "501"}, {"--timeout", "0"}, {"--timeout", "61s"}, {"--page", "bad"}, {"--bogus"}, {"--privacy"}, {"--limit", "--json=false"}} {
		stdout, stderr, err := executeCommand(t, Dependencies{}, append([]string{"session", "list", "--host", "pc", "--json"}, flags...)...)
		assertAgentSessionFailure(t, stdout, stderr, err, agentSessionsList, "usage.invalid", 2)
	}
}

func TestAgentSessionListUsesOnlyPassiveDial(t *testing.T) {
	state := t.TempDir()
	t.Setenv("MESH_STATE_DIR", state)
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	if _, _, err := identity.LoadOrCreate(state); err != nil {
		t.Fatal(err)
	}
	id := argumentIdentities(t)[1]
	host := HostRecord{ID: id, MeshIdentity: id, Endpoint: "ws://127.0.0.1:1/control/ws"}
	if err := SaveHost(host); err != nil {
		t.Fatal(err)
	}
	active := func(context.Context, HostRecord) (transport.Conn, error) {
		t.Error("active dial called")
		return nil, context.Canceled
	}
	passive := serviceRemoteDial(host, func(request protocol.Control) protocol.Control {
		if request.Type != protocol.TypeList || !request.Lean {
			t.Errorf("not lean passive list: %+v", request)
		}
		return protocol.Control{Type: protocol.TypeListed, Sessions: []protocol.SessionInfo{{ID: "7K3D", HostID: id, State: "running", Command: []string{"private"}, Cwd: "/private", CreatedAt: time.Now()}}}
	})
	stdout, stderr, err := executeCommand(t, Dependencies{DialHost: active, DialControl: active, DialPassive: passive}, "session", "list", "--host", id, "--json")
	if err != nil || stderr != "" {
		t.Fatalf("%v %s %s", err, stderr, stdout)
	}
	var value struct{ Data agentapi.SessionPage }
	if err := json.Unmarshal([]byte(stdout), &value); err != nil {
		t.Fatal(err)
	}
	if len(value.Data.Sessions) != 1 || value.Data.Sessions[0].Command != nil || value.Data.Sessions[0].Cwd != "" {
		t.Fatal(stdout)
	}
	assertAgentAbsent(t, filepath.Join(state, "s"), filepath.Join(state, "mesh.db"))
}

func TestAgentSessionAbsentLocalDoesNotCreateSessionDirectory(t *testing.T) {
	state := t.TempDir()
	t.Setenv("MESH_STATE_DIR", state)
	t.Setenv("MESH_CONFIG_DIR", filepath.Join(t.TempDir(), "absent"))
	owner, _, err := identity.LoadOrCreate(state)
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := executeCommand(t, Dependencies{}, "session", "inspect", "7K3D", "--host", owner.ID, "--json")
	assertAgentSessionFailure(t, stdout, stderr, err, agentSessionsInspect, "session.not_found", 1)
	assertAgentAbsent(t, filepath.Join(state, "s"), filepath.Join(state, "mesh.db"))
	entries, err := os.ReadDir(state)
	if err != nil || len(entries) != 1 {
		t.Fatal(entries, err)
	}
}

func TestAgentSessionWrongIdentityNeverFallsBackToCache(t *testing.T) {
	state := t.TempDir()
	t.Setenv("MESH_STATE_DIR", state)
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	if _, _, err := identity.LoadOrCreate(state); err != nil {
		t.Fatal(err)
	}
	id := argumentIdentities(t)[1]
	host := HostRecord{ID: id, MeshIdentity: id, Endpoint: "ws://127.0.0.1:1/control/ws"}
	if err := SaveHost(host); err != nil {
		t.Fatal(err)
	}
	dial := func(context.Context, HostRecord) (transport.Conn, error) { return nil, &hostIdentityError{label: id} }
	stdout, stderr, err := executeCommand(t, Dependencies{DialPassive: dial}, "session", "list", "--host", id, "--json")
	assertAgentSessionFailure(t, stdout, stderr, err, agentSessionsList, "host.identity_mismatch", 1)
	if errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestAgentSessionPagingAndBounds(t *testing.T) {
	at := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	rows := []protocol.SessionInfo{{ID: "9K3D", HostID: "a", State: "running", CreatedAt: at}, {ID: "7K3D", HostID: "a", State: "running", CreatedAt: at}, {ID: "8K3D", HostID: "a", State: "running", CreatedAt: at}}
	snapshot := agentSessionSnapshot{rows: rows, freshness: agentapi.FreshnessLive}
	first := agentSessionPage("a", snapshot, agentSessionOptions{limit: 1}, nil)
	if first.Sessions[0].ID != "7K3D" || first.NextPage == nil {
		t.Fatalf("%+v", first)
	}
	cursor, err := parseAgentSessionCursor(*first.NextPage)
	if err != nil {
		t.Fatal(err)
	}
	second := agentSessionPage("a", snapshot, agentSessionOptions{limit: 1}, cursor)
	if second.Sessions[0].ID != "8K3D" {
		t.Fatalf("%+v", second)
	}
	for _, token := range []string{"!", strings.Repeat("x", 1025), base64.RawURLEncoding.EncodeToString([]byte(`{"hostId":"a","createdAt":"2026-11-06T10:00:00Z","id":"../x"}`)), base64.RawURLEncoding.EncodeToString([]byte(`{"hostId":"a","createdAt":"2026-11-06T10:00:00Z","id":"7K3D","extra":1}`))} {
		if _, err := parseAgentSessionCursor(token); err == nil {
			t.Error("invalid cursor accepted")
		}
	}
}

func TestAgentSessionMetadataAndInspectionBoundaries(t *testing.T) {
	host := HostRecord{ID: argumentIdentities(t)[1], MeshIdentity: argumentIdentities(t)[1]}
	for _, row := range []protocol.SessionInfo{
		{ID: "7K3D", HostID: "wrong", Command: []string{"sh"}, State: "running", CreatedAt: time.Now()},
		{ID: "7K3D", HostID: host.ID, Command: []string{strings.Repeat("x", recovery.MaxCommandBytes+1)}, State: "running", CreatedAt: time.Now()},
	} {
		if err := validateAgentSessionRow(host.ID, row); err == nil {
			t.Error("invalid metadata accepted")
		}
	}
	record := recovery.Record{Version: 1, HostID: host.ID, SessionID: "7K3D", Shell: "/bin/sh", ShellDirectory: "/work", DirectorySource: recovery.DirectoryLaunch, Command: []string{"sh"}, Lines: []string{strings.Repeat("x", 120)}}
	inspected, err := savedInspection(host, "7K3D", record)
	if err != nil {
		t.Fatal(err)
	}
	var result agentapi.SessionDetail
	applyAgentInspection(&result, inspected, false)
	if result.Live != nil || result.Saved == nil || result.Saved.CheckpointAt != nil || len(result.Saved.Preview) != 0 {
		t.Fatalf("%+v", result)
	}
	applyAgentInspection(&result, inspected, true)
	if len(result.Saved.Preview[0]) != 80 {
		t.Fatal(result.Saved.Preview)
	}
}

func TestAgentSessionCachedOfflineAndCacheFailure(t *testing.T) {
	state := t.TempDir()
	t.Setenv("MESH_STATE_DIR", state)
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	if _, _, err := identity.LoadOrCreate(state); err != nil {
		t.Fatal(err)
	}
	id := argumentIdentities(t)[1]
	host := HostRecord{ID: id, MeshIdentity: id, Endpoint: "ws://127.0.0.1:1/control/ws"}
	if err := SaveHost(host); err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(t.Context(), filepath.Join(state, catalogDatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	cache := &SQLiteCatalogCache{store: store, now: time.Now}
	row := protocol.SessionInfo{ID: "7K3D", HostID: id, State: "running", Command: []string{"sh"}, Cwd: "/private", CreatedAt: time.Now()}
	if err := cache.Save(t.Context(), host, []protocol.SessionInfo{row}); err != nil {
		t.Fatal(err)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	mismatch := func(context.Context, HostRecord) (transport.Conn, error) { return nil, &hostIdentityError{label: id} }
	deniedOut, deniedErr, denied := executeCommand(t, Dependencies{DialPassive: mismatch}, "session", "list", "--host", id, "--json")
	assertAgentSessionFailure(t, deniedOut, deniedErr, denied, agentSessionsList, "host.identity_mismatch", 1)
	offline := func(context.Context, HostRecord) (transport.Conn, error) { return nil, errors.New("offline") }
	stdout, stderr, err := executeCommand(t, Dependencies{DialPassive: offline}, "session", "list", "--host", id, "--json")
	var result struct {
		Status string
		Data   agentapi.SessionPage
		Errors []agentapi.Error
	}
	if decodeErr := json.Unmarshal([]byte(stdout), &result); decodeErr != nil {
		t.Fatal(stdout, decodeErr)
	}
	code, ok := StatusCode(err)
	if !ok || code != 3 || stderr != "" || result.Status != "partial" || result.Data.Freshness != agentapi.FreshnessCached || len(result.Data.Sessions) != 1 || result.Data.Sessions[0].Cwd != "" {
		t.Fatalf("%s %s %v", stdout, stderr, err)
	}
	stdout, stderr, err = executeCommand(t, Dependencies{DialPassive: offline}, "session", "inspect", "8K3D", "--host", id, "--json")
	assertAgentSessionFailure(t, stdout, stderr, err, agentSessionsInspect, "host.unreachable", 1)
	if err := os.Remove(filepath.Join(state, catalogDatabaseName)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(state, catalogDatabaseName), 0o700); err != nil {
		t.Fatal(err)
	}
	live := serviceRemoteDial(host, func(protocol.Control) protocol.Control {
		return protocol.Control{Type: protocol.TypeListed, Sessions: []protocol.SessionInfo{row}}
	})
	stdout, stderr, err = executeCommand(t, Dependencies{DialPassive: live}, "session", "list", "--host", id, "--json")
	if decodeErr := json.Unmarshal([]byte(stdout), &result); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	code, ok = StatusCode(err)
	if !ok || code != 3 || stderr != "" || result.Data.Freshness != agentapi.FreshnessLive || result.Errors[0].Code != agentapi.StateUnavailable {
		t.Fatalf("%s %s %v", stdout, stderr, err)
	}
}

func TestAgentSessionDecodedGoldens(t *testing.T) {
	host := HostRecord{ID: argumentIdentities(t)[1], MeshIdentity: argumentIdentities(t)[1]}
	at := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	row := protocol.SessionInfo{ID: "7K3D", HostID: host.ID, Command: []string{"sh"}, Cwd: "/work", State: "running", CreatedAt: at}
	rows, err := listAgentSessions(t.Context(), host, serviceRemoteDial(host, func(protocol.Control) protocol.Control {
		return protocol.Control{Type: protocol.TypeListed, Sessions: []protocol.SessionInfo{row}}
	}))
	if err != nil {
		t.Fatal(err)
	}
	page := agentSessionPage(host.ID, agentSessionSnapshot{rows: rows, freshness: agentapi.FreshnessLive}, agentSessionOptions{limit: 100}, nil)
	checkAgentSessionGolden[agentapi.SessionPage](t, "sessions.list.ok", agentSessionsList, agentSessionEnvelope(page, nil))
	a := &application{dependencies: Dependencies{DialPassive: serviceRemoteDial(host, func(protocol.Control) protocol.Control {
		return protocol.Control{Type: protocol.TypeInspected, SessionID: row.ID, Inspection: &protocol.SessionInspection{ObservedAt: at, Attached: true, Preview: []string{"x"}}}
	})}}
	inspection, err := a.inspectAgentSession(t.Context(), host, row.ID, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	detail := agentapi.SessionDetail{Session: agentSessionSummary(row, true), Freshness: agentapi.FreshnessLive}
	applyAgentInspection(&detail, inspection, false)
	checkAgentSessionGolden[agentapi.SessionDetail](t, "sessions.inspect.live", agentSessionsInspect, agentSessionEnvelope(detail, nil))
	a.dependencies.DialPassive = serviceRemoteDial(host, func(protocol.Control) protocol.Control {
		return protocol.Control{Type: protocol.TypeInspected, SessionID: row.ID, Recovery: &recovery.Record{Version: 1, HostID: host.ID, SessionID: row.ID, Shell: "/bin/sh", ShellDirectory: "/work", DirectorySource: recovery.DirectoryLaunch, Command: []string{"sh"}}}
	})
	inspection, err = a.inspectAgentSession(t.Context(), host, row.ID, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	detail.Live = nil
	exit := 37
	detail.Session.State, detail.Session.ExitCode = "exited", &exit
	applyAgentInspection(&detail, inspection, false)
	checkAgentSessionGolden[agentapi.SessionDetail](t, "sessions.inspect.saved", agentSessionsInspect, agentSessionEnvelope(detail, nil))
}

func checkAgentSessionGolden[T any](t *testing.T, name, operation string, envelope agentapi.Envelope) {
	t.Helper()
	envelope.SchemaVersion, envelope.Operation, envelope.RequestID = 1, operation, "01JA0000000000000000000000"
	var output bytes.Buffer
	if err := agentapi.Write(&output, envelope); err != nil {
		t.Fatal(err)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, output.Bytes(), "", "  "); err != nil {
		t.Fatal(err)
	}
	directory, err := os.OpenRoot(filepath.Join("..", "agentapi", "testdata"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = directory.Close() })
	if os.Getenv("MESH_UPDATE_AGENT_GOLDENS") == "1" {
		if err := directory.WriteFile(name+".json", pretty.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := directory.ReadFile(name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(want, pretty.Bytes()) {
		t.Fatalf("golden %s differs\n%s", name, pretty.String())
	}
	decoder := json.NewDecoder(bytes.NewReader(want))
	decoder.DisallowUnknownFields()
	var checked struct {
		SchemaVersion int              `json:"schemaVersion"`
		Operation     string           `json:"operation"`
		Status        agentapi.Status  `json:"status"`
		RequestID     string           `json:"requestId"`
		Data          T                `json:"data"`
		Errors        []agentapi.Error `json:"errors"`
	}
	if err := decoder.Decode(&checked); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&checked); err != io.EOF {
		t.Fatal("multiple golden results")
	}
}

func assertAgentSessionFailure(t *testing.T, stdout, stderr string, err error, operation, code string, exit int) {
	t.Helper()
	var result agentapi.Envelope
	decoder := json.NewDecoder(strings.NewReader(stdout))
	decoder.DisallowUnknownFields()
	if decodeErr := decoder.Decode(&result); decodeErr != nil {
		t.Fatalf("expected one envelope, got %q: %v", stdout, decodeErr)
	}
	if result.Operation != operation || result.Status != agentapi.StatusError || result.Data != nil || len(result.Errors) != 1 || string(result.Errors[0].Code) != code {
		t.Fatalf("unexpected envelope: %s", stdout)
	}
	if decodeErr := decoder.Decode(&result); decodeErr != io.EOF {
		t.Fatal("multiple results")
	}
	if status, ok := StatusCode(err); !ok || status != exit || stderr != "" {
		t.Fatalf("exit=%v stderr=%q", err, stderr)
	}
}
