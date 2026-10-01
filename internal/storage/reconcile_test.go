package storage

import (
	"reflect"
	"testing"
	"time"
)

func TestApplyHostChangesWritesOnlyDelta(t *testing.T) {
	store := openTestStore(t)
	host := testHost("host-a")
	live := testSession(host.ID, "LIVE", StateRunning, 20)
	other := testSession(host.ID, "OTHER", StateDetached, 30)
	retired := testSession(host.ID, "OLD", StateInterrupted, 40)
	if err := store.ApplyHostChanges(t.Context(), host.ID, HostChanges{Host: &host, Sessions: []Session{live, other, retired}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TABLE writes (id TEXT); CREATE TRIGGER record_session_update AFTER UPDATE ON sessions BEGIN INSERT INTO writes VALUES (new.id); END`); err != nil {
		t.Fatal(err)
	}
	live.State = StateDetached
	if err := store.ApplyHostChanges(t.Context(), host.ID, HostChanges{Sessions: []Session{live}, Retired: []SessionID{retired.ID}}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT count(*) FROM writes WHERE id = 'LIVE'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("changed session updates = %d, want 1", count)
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM writes WHERE id = 'OTHER'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("unchanged session updates = %d, want 0", count)
	}
	rows, err := store.ListHostSessions(t.Context(), host.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("retirement left %d sessions", len(rows))
	}
}

func TestApplyHostChangesRollsBackWholePass(t *testing.T) {
	store := openTestStore(t)
	host := testHost("host-a")
	live := testSession(host.ID, "LIVE", StateRunning, 20)
	retired := testSession(host.ID, "OLD", StateInterrupted, 40)
	if err := store.ApplyHostChanges(t.Context(), host.ID, HostChanges{Host: &host, Sessions: []Session{live, retired}}); err != nil {
		t.Fatal(err)
	}
	before, err := store.ListHostSessions(t.Context(), host.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER reject_retirement BEFORE DELETE ON sessions BEGIN SELECT RAISE(ABORT, 'blocked retirement'); END`); err != nil {
		t.Fatal(err)
	}
	host.LastSeenAt = host.LastSeenAt.Add(time.Minute)
	live.State = StateDetached
	if err := store.ApplyHostChanges(t.Context(), host.ID, HostChanges{Host: &host, Sessions: []Session{live}, Retired: []SessionID{retired.ID}}); err == nil {
		t.Fatal("retirement failure was swallowed")
	}
	after, err := store.ListHostSessions(t.Context(), host.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("failed pass changed sessions: before=%v after=%v", before, after)
	}
	persisted, err := store.GetHost(t.Context(), host.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !persisted.LastSeenAt.Equal(testHost("host-a").LastSeenAt) {
		t.Fatalf("failed pass changed host: %+v", persisted)
	}
}
