package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
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

func sessionCacheHost(t *testing.T) HostRecord {
	t.Helper()
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
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return host
}

func sessionCacheRows(host HostRecord, label string) []protocol.SessionInfo {
	at := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	exit := 37
	return []protocol.SessionInfo{
		{ID: "7K3D", HostID: host.ID, Command: []string{"sh"}, Cwd: "/private", State: "exited", ExitCode: &exit, CreatedAt: at, DetachedAt: &at, Label: label, RecoveryPending: true, Hibernated: &recovery.Hibernation{Version: 1, At: at, Reason: "request", Provider: "codex", ConversationID: "not-a-cached-recipe"}},
		{ID: "8K3D", HostID: host.ID, Command: []string{"bash"}, Cwd: "/detached", State: "detached", CreatedAt: at.Add(time.Second), DetachedAt: &at, Label: label},
		{ID: "9K3D", HostID: host.ID, Command: []string{"zsh"}, Cwd: "/running", State: "running", CreatedAt: at.Add(2 * time.Second)},
	}
}

func cacheOffline(context.Context, HostRecord) (transport.Conn, error) {
	return nil, errors.New("fixture offline")
}

func sessionCacheList(t *testing.T, host HostRecord, dial HostDialer, status int, flags ...string) (agentapi.SessionPage, []agentapi.Error) {
	t.Helper()
	args := append([]string{"session", "list", "--host", host.ID, "--json", "--all"}, flags...)
	stdout, stderr, err := executeCommand(t, Dependencies{DialPassive: dial}, args...)
	assertCacheCommandStatus(t, stdout, stderr, err, status)
	var envelope struct {
		Status string
		Data   agentapi.SessionPage
		Errors []agentapi.Error
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatal(err)
	}
	if status == 3 && envelope.Status != "partial" {
		t.Fatal(stdout)
	}
	return envelope.Data, envelope.Errors
}

func assertCacheCommandStatus(t *testing.T, stdout, stderr string, err error, status int) {
	t.Helper()
	if status == 0 && err == nil && stderr == "" {
		return
	}
	code, ok := StatusCode(err)
	if !ok || code != status || stderr != "" {
		t.Fatalf("status want %d: %s %s %v", status, stdout, stderr, err)
	}
}

func cacheLiveDial(host HostRecord, rows []protocol.SessionInfo) HostDialer {
	return serviceRemoteDial(host, func(protocol.Control) protocol.Control {
		return protocol.Control{Type: protocol.TypeListed, Sessions: rows}
	})
}

// This is the review's behavioral regression: separate real commands open,
// save, close and reopen one SQLite database before offline list and inspect.
func TestAgentSessionCacheFidelity(t *testing.T) {
	for i, label := range []string{"review-label", strings.Repeat("a", 2048), strings.Repeat("a", 4096), strings.Repeat("é", 2048), "safe\x1b[31m\nlabel\u202e"} {
		t.Run(strconv.Itoa(len(label))+"-bytes-"+strconv.Itoa(i), func(t *testing.T) { checkSessionCacheFidelity(t, label) })
	}
}

func checkSessionCacheFidelity(t *testing.T, label string) {
	host := sessionCacheHost(t)
	rows := sessionCacheRows(host, label)
	dial := cacheLiveDial(host, rows)
	for _, detail := range []bool{false, true} {
		checkSessionCacheListEquality(t, host, dial, detail)
	}
	before, _ := sessionCacheList(t, host, dial, 0, "--detail")
	for _, summary := range before.Sessions {
		assertCachedInspection(t, host, summary)
	}
	cache, err := OpenCatalogCache(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cache.Close() }()
	observed, err := cache.LoadObserved(t.Context(), host)
	if err != nil || len(observed) != len(rows) {
		t.Fatal(observed, err)
	}
	for _, row := range observed {
		if row.Info.Hibernated != nil || row.Info.Recovery != nil {
			t.Fatal("cache reconstructed recovery instructions")
		}
	}
}

func checkSessionCacheListEquality(t *testing.T, host HostRecord, dial HostDialer, detail bool) {
	t.Helper()
	var flags []string
	if detail {
		flags = []string{"--detail"}
	}
	before, _ := sessionCacheList(t, host, dial, 0, flags...)
	after, diagnostics := sessionCacheList(t, host, cacheOffline, 3, flags...)
	if after.Freshness != agentapi.FreshnessCached || !reflect.DeepEqual(before.Sessions, after.Sessions) {
		t.Fatalf("live = %+v, cached = %+v", before, after)
	}
	if len(diagnostics) != 1 || diagnostics[0].Code != agentapi.HostUnreachable {
		t.Fatal(diagnostics)
	}
}

func assertCachedInspection(t *testing.T, host HostRecord, want agentapi.SessionSummary) {
	t.Helper()
	stdout, stderr, err := executeCommand(t, Dependencies{DialPassive: cacheOffline}, "session", "inspect", want.ID, "--host", host.ID, "--json")
	assertCacheCommandStatus(t, stdout, stderr, err, 3)
	var envelope struct{ Data agentapi.SessionDetail }
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatal(err)
	}
	got := envelope.Data
	if !reflect.DeepEqual(got.Session, want) || got.LiveState != "unavailable" || got.Live != nil || got.Saved != nil || got.Freshness != agentapi.FreshnessCached {
		t.Fatalf("inspect = %+v, want %+v", got, want)
	}
}

func TestAgentSessionCacheIncompleteRowsAndPaging(t *testing.T) {
	host := sessionCacheHost(t)
	rows := sessionCacheRows(host, "complete")
	sessionCacheList(t, host, cacheLiveDial(host, rows), 0)
	store, err := storage.Open(t.Context(), filepath.Join(os.Getenv("MESH_STATE_DIR"), catalogDatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	missing := storage.Session{ID: "6K3D", HostID: storage.HostID(host.ID), Command: []string{"sh"}, State: storage.StateRunning, CreatedAt: rows[2].CreatedAt.Add(time.Second)}
	if _, err := store.UpsertSession(t.Context(), missing); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	page, diagnostics := sessionCacheList(t, host, cacheOffline, 3, "--limit", "1")
	assertCacheOmission(t, host, diagnostics)
	if len(page.Sessions) != 1 || page.Sessions[0].ID != rows[2].ID || page.NextPage == nil {
		t.Fatal(page)
	}
	next, _ := sessionCacheList(t, host, cacheOffline, 3, "--limit", "1", "--page", *page.NextPage)
	if len(next.Sessions) != 1 || next.Sessions[0].ID != rows[1].ID {
		t.Fatal(next)
	}
	stdout, stderr, err := executeCommand(t, Dependencies{DialPassive: cacheOffline}, "session", "inspect", "6K3D", "--host", host.ID, "--json")
	assertAgentSessionFailure(t, stdout, stderr, err, agentSessionsInspect, "host.unreachable", 1)
	sessionCacheList(t, host, cacheLiveDial(host, nil), 0)
	assertCacheUnavailable(t, host)
}

func assertCacheOmission(t *testing.T, host HostRecord, diagnostics []agentapi.Error) {
	t.Helper()
	if len(diagnostics) != 2 || diagnostics[0].Code != agentapi.HostUnreachable || diagnostics[1].Code != agentapi.StateUnavailable || diagnostics[1].Target != host.ID || !diagnostics[1].Retryable || !strings.HasPrefix(diagnostics[1].Message, "1 cached sessions") {
		t.Fatal(diagnostics)
	}
}

func assertCacheUnavailable(t *testing.T, host HostRecord) {
	t.Helper()
	stdout, stderr, err := executeCommand(t, Dependencies{DialPassive: cacheOffline}, "session", "list", "--host", host.ID, "--json", "--all")
	assertAgentSessionFailure(t, stdout, stderr, err, agentSessionsList, "host.unreachable", 1)
}

func TestAgentSessionCacheDisappearedAndEmpty(t *testing.T) {
	host := sessionCacheHost(t)
	rows := sessionCacheRows(host, "observed")
	sessionCacheList(t, host, cacheLiveDial(host, rows), 0)
	sessionCacheList(t, host, cacheLiveDial(host, rows[:1]), 0)
	page, diagnostics := sessionCacheList(t, host, cacheOffline, 3)
	if len(page.Sessions) != 1 || page.Sessions[0].ID != rows[0].ID || len(diagnostics) != 2 {
		t.Fatal(page, diagnostics)
	}
	empty, _ := sessionCacheList(t, host, cacheLiveDial(host, []protocol.SessionInfo{}), 0)
	if len(empty.Sessions) != 0 || empty.Freshness != agentapi.FreshnessLive {
		t.Fatal(empty)
	}
	assertCacheUnavailable(t, host)
}

func TestAgentSessionCacheHumanAndPendingPickerWrites(t *testing.T) {
	host := sessionCacheHost(t)
	rows := sessionCacheRows(host, "observed")
	sessionCacheList(t, host, cacheLiveDial(host, rows), 0)
	cache, err := OpenCatalogCache(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cache.Close() }()
	before, err := cache.Load(t.Context(), host)
	if err != nil || len(before) != len(rows) {
		t.Fatal(before, err)
	}
	for _, row := range before {
		assertHumanBaseOnly(t, row)
	}
	path := filepath.Join(os.Getenv("MESH_STATE_DIR"), catalogDatabaseName)
	corruptCacheFacts(t, path)
	after, err := cache.Load(t.Context(), host)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("advisory corruption changed human Load", after, err)
	}
	if _, err := cache.LoadObserved(t.Context(), host); err == nil {
		t.Fatal("agent accepted corrupt facts")
	}
	// Human Save never owned validation of these advisory-only fields.
	bad := rows[2]
	negative := time.UnixMilli(-1)
	bad.Label, bad.DetachedAt = strings.Repeat("x", 4097), &negative
	if err := cache.Save(t.Context(), host, []protocol.SessionInfo{bad}); err != nil {
		t.Fatal("human Save acquired advisory validation", err)
	}
	assertCacheUnavailable(t, host)
	if err := cache.SaveObserved(t.Context(), host, rows); err != nil {
		t.Fatal(err)
	}
	pending := rows[0]
	pending.Hibernated, pending.Label, pending.DetachedAt = nil, "", nil
	pending.RecoveryPending = true
	picker := &pickerState{cache: cache}
	view := StateView{initialized: true, Sessions: []protocol.SessionInfo{pending}}
	if _, err := picker.saveCache(t.Context(), host, view, nil, nil); err != nil {
		t.Fatal(err)
	}
	observed, err := cache.LoadObserved(t.Context(), host)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range observed {
		if row.Facts != nil {
			t.Fatal("pending picker write certified unknown details")
		}
	}
	base, err := cache.Load(t.Context(), host)
	if err != nil || len(base) != len(rows) {
		t.Fatal("picker lost base rows", base, err)
	}
	assertCacheUnavailable(t, host)
}

func assertHumanBaseOnly(t *testing.T, row protocol.SessionInfo) {
	t.Helper()
	if row.Hibernated != nil || row.Recovery != nil || row.RecoveryPending || row.Label != "" || row.DetachedAt != nil {
		t.Fatal("human Load acquired advisory fields", row)
	}
}

func corruptCacheFacts(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(t.Context(), `UPDATE cached_session_facts SET hibernated=1 WHERE session_id='9K3D'`); err != nil {
		t.Fatal(err)
	}
}

func TestAgentSessionCacheNeverMasksAuthenticationFailure(t *testing.T) {
	host := sessionCacheHost(t)
	sessionCacheList(t, host, cacheLiveDial(host, sessionCacheRows(host, "complete")), 0)
	denied := func(context.Context, HostRecord) (transport.Conn, error) { return nil, transport.ErrAuthentication }
	stdout, stderr, err := executeCommand(t, Dependencies{DialPassive: denied}, "session", "list", "--host", host.ID, "--json", "--all")
	assertAgentSessionFailure(t, stdout, stderr, err, agentSessionsList, "host.authentication_failed", 1)
}
