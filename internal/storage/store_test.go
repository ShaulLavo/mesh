package storage

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestOpenAppliesMigrationsIdempotently(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "mesh.db")

	first, err := Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	assertSQLiteSettings(t, first)
	assertMigrationVersion(t, first, 1)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if got := first.db.Stats().OpenConnections; got != 0 {
		t.Fatalf("open connections after Close = %d, want 0", got)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	second, err := Open(ctx, databasePath)
	if err != nil {
		t.Fatalf("reopen migrated database: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	assertMigrationVersion(t, second, 1)
	var applied int
	if err := second.db.QueryRowContext(ctx, `
		SELECT count(*)
		FROM goose_db_version
		WHERE version_id = 1 AND is_applied = 1
	`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied != 1 {
		t.Fatalf("applied migration rows = %d, want 1", applied)
	}
}

func TestStoreSessionLifecycle(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	host := testHost("host-a")
	gotHost, err := store.UpsertHost(ctx, host)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotHost, host) {
		t.Fatalf("upserted host = %#v, want %#v", gotHost, host)
	}

	session := testSession(host.ID, "7K3D", StateRunning, 17)
	got, err := store.UpsertSession(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, session) {
		t.Fatalf("upserted session = %#v, want %#v", got, session)
	}

	got, err = store.GetSession(ctx, host.ID, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, session) {
		t.Fatalf("stored session = %#v, want %#v", got, session)
	}

	running, err := store.ListSessionsByState(ctx, StateRunning)
	if err != nil {
		t.Fatal(err)
	}
	if len(running) != 1 || running[0].ID != session.ID {
		t.Fatalf("running sessions = %#v", running)
	}

	exitCode := 7
	exited, err := store.SetSessionState(ctx, host.ID, session.ID, StateExited, &exitCode)
	if err != nil {
		t.Fatal(err)
	}
	if exited.State != StateExited || exited.ExitCode == nil || *exited.ExitCode != exitCode {
		t.Fatalf("exited session = %#v", exited)
	}
	running, err = store.ListSessionsByState(ctx, StateRunning)
	if err != nil {
		t.Fatal(err)
	}
	if running == nil || len(running) != 0 {
		t.Fatalf("running sessions after exit = %#v, want non-nil empty slice", running)
	}
	exitedRows, err := store.ListSessionsByState(ctx, StateExited)
	if err != nil {
		t.Fatal(err)
	}
	if len(exitedRows) != 1 || exitedRows[0].ID != session.ID {
		t.Fatalf("exited sessions = %#v", exitedRows)
	}

	if _, err := store.GetSession(ctx, host.ID, "MISSING"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing session error = %v, want sql.ErrNoRows", err)
	}
}

func TestUpsertSessionKeepsOutputSequenceMonotonic(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	host := testHost("host-a")
	if _, err := store.UpsertHost(ctx, host); err != nil {
		t.Fatal(err)
	}
	session := testSession(host.ID, "7K3D", StateRunning, 900)
	if _, err := store.UpsertSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	session.LastOutputSequence = 100
	persisted, err := store.UpsertSession(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.LastOutputSequence != 900 {
		t.Fatalf("last output sequence = %d, want 900", persisted.LastOutputSequence)
	}
}

func TestReconcileHostInterruptsMissingActiveSessions(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	hostA := testHost("host-a")
	hostB := testHost("host-b")
	for _, host := range []Host{hostA, hostB} {
		if _, err := store.UpsertHost(ctx, host); err != nil {
			t.Fatal(err)
		}
	}

	missing := testSession(hostA.ID, "MISS", StateRunning, 10)
	live := testSession(hostA.ID, "LIVE", StateDetached, 20)
	exitCode := 0
	exited := testSession(hostA.ID, "DONE", StateExited, 30)
	exited.ExitCode = &exitCode
	otherHost := testSession(hostB.ID, "OTHER", StateRunning, 40)
	for _, session := range []Session{missing, live, exited, otherHost} {
		if _, err := store.UpsertSession(ctx, session); err != nil {
			t.Fatal(err)
		}
	}

	observedLive := live
	observedLive.State = StateRunning
	observedLive.LastOutputSequence = 25
	observed := []Session{observedLive, exited}
	if err := store.ReconcileHost(ctx, hostA, observed); err != nil {
		t.Fatal(err)
	}
	assertSessionState(t, store, hostA.ID, missing.ID, StateInterrupted)
	assertSessionState(t, store, hostA.ID, live.ID, StateRunning)
	assertSessionState(t, store, hostA.ID, exited.ID, StateExited)
	assertSessionState(t, store, hostB.ID, otherHost.ID, StateRunning)

	before, err := store.ListHostSessions(ctx, hostA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReconcileHost(ctx, hostA, observed); err != nil {
		t.Fatalf("repeat reconciliation: %v", err)
	}
	after, err := store.ListHostSessions(ctx, hostA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("second reconciliation changed rows:\nbefore: %#v\nafter:  %#v", before, after)
	}
}

func TestReconcileHostKeepsLatestAttachmentTime(t *testing.T) {
	older := testTime(time.Hour)
	latest := testTime(2 * time.Hour)
	newer := testTime(3 * time.Hour)
	testCases := []struct {
		name     string
		stored   *time.Time
		observed *time.Time
		want     time.Time
	}{
		{name: "missing observation", stored: &latest, observed: nil, want: latest},
		{name: "older observation", stored: &latest, observed: &older, want: latest},
		{name: "newer observation", stored: &latest, observed: &newer, want: newer},
		{name: "first observation", stored: nil, observed: &latest, want: latest},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := openTestStore(t)
			host := testHost("host-a")
			if _, err := store.UpsertHost(ctx, host); err != nil {
				t.Fatal(err)
			}

			persisted := testSession(host.ID, "LIVE", StateDetached, 20)
			persisted.LastAttachedAt = tc.stored
			if _, err := store.UpsertSession(ctx, persisted); err != nil {
				t.Fatal(err)
			}

			observed := persisted
			observed.State = StateRunning
			observed.LastAttachedAt = tc.observed
			if err := store.ReconcileHost(ctx, host, []Session{observed}); err != nil {
				t.Fatal(err)
			}

			got, err := store.GetSession(ctx, host.ID, persisted.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.LastAttachedAt == nil || !got.LastAttachedAt.Equal(tc.want) {
				t.Fatalf("last attachment time = %v, want %v", got.LastAttachedAt, tc.want)
			}
		})
	}
}

func TestReconcileHostRejectsNonAuthoritativeInputBeforeWriting(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	host := testHost("host-a")
	if _, err := store.UpsertHost(ctx, host); err != nil {
		t.Fatal(err)
	}
	existing := testSession(host.ID, "LIVE", StateRunning, 1)
	if _, err := store.UpsertSession(ctx, existing); err != nil {
		t.Fatal(err)
	}

	duplicate := testSession(host.ID, "DUP", StateRunning, 2)
	if err := store.ReconcileHost(ctx, host, []Session{duplicate, duplicate}); err == nil {
		t.Fatal("duplicate reconciliation input succeeded")
	}
	assertSessionState(t, store, host.ID, existing.ID, StateRunning)

	wrongHost := duplicate
	wrongHost.HostID = "host-b"
	if err := store.ReconcileHost(ctx, host, []Session{wrongHost}); err == nil {
		t.Fatal("cross-host reconciliation input succeeded")
	}
	assertSessionState(t, store, host.ID, existing.ID, StateRunning)
}

func TestReconcileHostRollsBackHostWhenSessionUpdateFails(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	original := testHost("host-a")
	if _, err := store.UpsertHost(ctx, original); err != nil {
		t.Fatal(err)
	}
	running := testSession(original.ID, "LIVE", StateRunning, 1)
	if _, err := store.UpsertSession(ctx, running); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
		CREATE TRIGGER reject_session_update
		BEFORE UPDATE OF state ON sessions
		BEGIN
			SELECT RAISE(ABORT, 'injected session update failure');
		END;
	`); err != nil {
		t.Fatal(err)
	}

	updated := original
	updated.LastSeenAt = original.LastSeenAt.Add(time.Hour)
	if err := store.ReconcileHost(ctx, updated, nil); err == nil {
		t.Fatal("reconciliation with injected failure succeeded")
	}
	persisted, err := store.GetHost(ctx, original.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !persisted.LastSeenAt.Equal(original.LastSeenAt) {
		t.Fatalf("host timestamp committed despite session rollback: got %v, want %v", persisted.LastSeenAt, original.LastSeenAt)
	}
	assertSessionState(t, store, original.ID, running.ID, StateRunning)
}

func TestStoreRejectsInvalidBoundaryValues(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	host := testHost("host-a")
	if _, err := store.UpsertHost(ctx, host); err != nil {
		t.Fatal(err)
	}

	invalid := testSession(host.ID, "BAD", SessionState("paused"), 0)
	if _, err := store.UpsertSession(ctx, invalid); err == nil {
		t.Fatal("unsupported session state succeeded")
	}
	runningExitCode := 1
	invalid = testSession(host.ID, "BAD", StateRunning, 0)
	invalid.ExitCode = &runningExitCode
	if _, err := store.UpsertSession(ctx, invalid); err == nil {
		t.Fatal("running session with exit code succeeded")
	}
	invalid = testSession(host.ID, "BAD", StateExited, 0)
	if _, err := store.UpsertSession(ctx, invalid); err == nil {
		t.Fatal("exited session without exit code succeeded")
	}
	invalid = testSession("unknown-host", "BAD", StateRunning, 0)
	if _, err := store.UpsertSession(ctx, invalid); err == nil {
		t.Fatal("session for unknown host succeeded")
	}
}

func assertSQLiteSettings(t *testing.T, store *Store) {
	t.Helper()
	ctx := context.Background()
	var journalMode string
	if err := store.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if journalMode != "wal" {
		t.Fatalf("journal mode = %q, want wal", journalMode)
	}
	var foreignKeys int
	if err := store.db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 1 {
		t.Fatalf("foreign keys = %d, want 1", foreignKeys)
	}
}

func assertMigrationVersion(t *testing.T, store *Store, want int64) {
	t.Helper()
	var got int64
	if err := store.db.QueryRow(`SELECT max(version_id) FROM goose_db_version WHERE is_applied = 1`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("migration version = %d, want %d", got, want)
	}
}

func assertSessionState(t *testing.T, store *Store, hostID HostID, sessionID SessionID, want SessionState) {
	t.Helper()
	session, err := store.GetSession(context.Background(), hostID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if session.State != want {
		t.Fatalf("session %s/%s state = %s, want %s", hostID, sessionID, session.State, want)
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "mesh.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return store
}

func testHost(id HostID) Host {
	alias := "alias-" + string(id)
	tailscaleName := string(id) + ".example.ts.net"
	return Host{
		ID:            id,
		Alias:         &alias,
		MeshIdentity:  "identity-" + string(id),
		TailscaleName: &tailscaleName,
		LastSeenAt:    testTime(0),
	}
}

func testSession(hostID HostID, id SessionID, state SessionState, sequence uint64) Session {
	return Session{
		ID:                 id,
		HostID:             hostID,
		Command:            []string{"sh", "-c", "printf hello"},
		Cwd:                "/tmp/project",
		State:              state,
		CreatedAt:          testTime(0),
		LastOutputSequence: sequence,
	}
}

func testTime(offset time.Duration) time.Time {
	return time.Date(2026, time.August, 29, 12, 0, 0, 0, time.UTC).Add(offset)
}
