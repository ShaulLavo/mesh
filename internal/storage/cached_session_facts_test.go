package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
	"github.com/shaul/mesh/db/migrations"
)

func factsObservation(host HostID, id SessionID, state SessionState) ObservedSession {
	row := ObservedSession{Session: testSession(host, id, state, 20), Facts: &SessionFacts{}}
	if state == StateExited {
		code := 37
		row.ExitCode = &code
		row.Facts.Hibernated = true
		row.Facts.RecoveryPending = true
	}
	return row
}

func saveFacts(t *testing.T, store *Store, host Host, rows ...ObservedSession) {
	t.Helper()
	if err := store.ReconcileHost(t.Context(), host, rows); err != nil {
		t.Fatal(err)
	}
}

func readFacts(t *testing.T, store *Store, host HostID) []ObservedSession {
	t.Helper()
	rows, err := store.ListObservedHostSessions(t.Context(), host)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestCachedSessionFactsRoundTripAndReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mesh.db")
	store, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	host := testHost("remote")
	exited := factsObservation(host.ID, "EXIT", StateExited)
	detached := factsObservation(host.ID, "DET", StateDetached)
	at := testTime(time.Hour)
	exited.Facts.Label, detached.Facts.Label = "hibernated", "detached"
	exited.Facts.DetachedAt, detached.Facts.DetachedAt = &at, &at
	running := factsObservation(host.ID, "RUN", StateRunning)
	saveFacts(t, store, host, exited, detached, running)
	want := readFacts(t, store, host.ID)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if got := readFacts(t, reopened, host.ID); !reflect.DeepEqual(got, want) {
		t.Fatalf("reopened = %#v, want %#v", got, want)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	advisory, err := OpenAdvisory(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = advisory.Close() })
	if got := readFacts(t, advisory, host.ID); !reflect.DeepEqual(got, want) {
		t.Fatal("advisory lost complete facts")
	}
	exited.Facts = &SessionFacts{}
	detached.Facts = &SessionFacts{}
	saveFacts(t, advisory, host, exited, detached, running)
	for _, row := range readFacts(t, advisory, host.ID) {
		if row.Facts == nil || !reflect.DeepEqual(*row.Facts, SessionFacts{}) {
			t.Fatalf("replacement did not clear facts: %+v", row)
		}
	}
}

func TestCachedSessionFactsMigrationPreservesRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mesh.db")
	db := historicalDatabase(t, path, 14)
	statements := []string{
		`INSERT INTO hosts VALUES ('local','local-key',NULL,1), ('remote','remote-key','remote-name',2)`,
		`INSERT INTO sessions VALUES ('SAME','local','["sh"]','/local','running',1,2,NULL,10), ('SAME','remote','["bash"]','/remote','exited',3,NULL,37,20)`,
		`INSERT INTO services(name,kind,target) VALUES ('site','static','/work/site')`,
		`INSERT INTO cached_services(host_id,private_name,name,kind,target,healthy,problem,observed_at,local_only,listens) VALUES ('remote','','3000','proxy','3000',1,'',1,1,'[{"public":3000,"upstream":3001}]')`,
	}
	for _, statement := range statements {
		factsSQL(t, db, statement)
	}
	queries := []string{"SELECT * FROM hosts ORDER BY id", "SELECT * FROM sessions ORDER BY host_id,id", "SELECT * FROM services", "SELECT * FROM cached_services"}
	before := make([][][]any, len(queries))
	for i, query := range queries {
		before[i] = migrationRows(t, db, query)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for i, query := range queries {
		assertMigrationRows(t, store.db, query, before[i])
	}
	for _, host := range []HostID{"local", "remote"} {
		assertUnknownFacts(t, store, host, 1)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, store.db, migrations.Files)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(t.Context(), 14); err != nil {
		t.Fatal(err)
	}
	assertMigrationRows(t, store.db, `SELECT name FROM sqlite_master WHERE name IN ('cached_session_facts','sessions_forget_cached_facts')`, nil)
	for i, query := range queries {
		assertMigrationRows(t, store.db, query, before[i])
	}
	if _, err := provider.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	for i, query := range queries {
		assertMigrationRows(t, store.db, query, before[i])
	}
	assertUnknownFacts(t, store, "remote", 1)
}

func factsSQL(t *testing.T, db *sql.DB, statement string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), statement, args...); err != nil {
		t.Fatal(err)
	}
}

func assertUnknownFacts(t *testing.T, store *Store, host HostID, count int) {
	t.Helper()
	rows := readFacts(t, store, host)
	if len(rows) != count {
		t.Fatalf("rows = %d, want %d", len(rows), count)
	}
	for _, row := range rows {
		if row.Facts != nil {
			t.Fatalf("facts not invalidated: %+v", row)
		}
	}
}

func TestCachedSessionFactsEveryWriter(t *testing.T) {
	for _, writer := range []string{"upsert", "state", "delta", "reconcile-nil", "reconcile-empty", "incomplete", "retire", "delta-retire", "host-delete", "noop-sql"} {
		t.Run(writer, func(t *testing.T) { checkFactsWriter(t, writer) })
	}
}

func checkFactsWriter(t *testing.T, writer string) {
	store := openTestStore(t)
	host := testHost("a")
	other := testHost("b")
	row := factsObservation(host.ID, "SAME", StateExited)
	peer := factsObservation(other.ID, "SAME", StateExited)
	saveFacts(t, store, host, row)
	saveFacts(t, store, other, peer)
	var err error
	switch writer {
	case "upsert":
		_, err = store.UpsertSession(t.Context(), row.Session)
	case "state":
		_, err = store.SetSessionState(t.Context(), host.ID, row.ID, StateExited, row.ExitCode)
	case "delta":
		err = store.ApplyHostChanges(t.Context(), host.ID, HostChanges{Sessions: []Session{row.Session}})
	case "reconcile-nil":
		err = store.ReconcileHost(t.Context(), host, nil)
	case "reconcile-empty":
		err = store.ReconcileHost(t.Context(), host, []ObservedSession{})
	case "incomplete":
		row.Facts = nil
		err = store.ReconcileHost(t.Context(), host, []ObservedSession{row})
	case "retire":
		_, err = store.RetireSessions(t.Context(), host.ID, []SessionID{row.ID})
	case "delta-retire":
		err = store.ApplyHostChanges(t.Context(), host.ID, HostChanges{Retired: []SessionID{row.ID}})
	case "host-delete":
		_, err = store.db.ExecContext(t.Context(), `DELETE FROM hosts WHERE id = ?`, host.ID)
	case "noop-sql":
		_, err = store.db.ExecContext(t.Context(), `UPDATE sessions SET state = state WHERE host_id = ?`, host.ID)
	}
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.db.QueryRowContext(t.Context(), `SELECT count(*) FROM cached_session_facts WHERE host_id = ?`, host.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("writer retained facts")
	}
	got := readFacts(t, store, other.ID)
	if len(got) != 1 || !reflect.DeepEqual(got[0], peer) {
		t.Fatalf("other host changed: %+v", got)
	}
}

func TestCachedSessionFactsDisappearedAndEmpty(t *testing.T) {
	store := openTestStore(t)
	host := testHost("a")
	active := factsObservation(host.ID, "ACTIVE", StateRunning)
	exited := factsObservation(host.ID, "EXIT", StateExited)
	kept := factsObservation(host.ID, "KEEP", StateDetached)
	saveFacts(t, store, host, active, exited, kept)
	saveFacts(t, store, host, kept)
	rows := readFacts(t, store, host.ID)
	for _, row := range rows {
		checkDisappearedFact(t, row)
	}
	saveFacts(t, store, host)
	assertUnknownFacts(t, store, host.ID, 3)
	assertSessionState(t, store, host.ID, kept.ID, StateInterrupted)
}

func checkDisappearedFact(t *testing.T, row ObservedSession) {
	t.Helper()
	if row.ID == "KEEP" {
		if row.Facts == nil {
			t.Fatal("kept row lost facts")
		}
		return
	}
	if row.Facts != nil {
		t.Fatal("disappeared row retained facts")
	}
	if row.ID == "ACTIVE" && row.State != StateInterrupted {
		t.Fatal("missing active row not interrupted")
	}
	if row.ID == "EXIT" && row.State != StateExited {
		t.Fatal("history changed")
	}
}

func TestCachedSessionFactsAttachments(t *testing.T) {
	for _, name := range []string{"nil", "older", "newer", "same-millisecond", "both-nil", "sequence-only"} {
		t.Run(name, func(t *testing.T) { checkFactsAttachment(t, name) })
	}
}

func checkFactsAttachment(t *testing.T, name string) {
	store := openTestStore(t)
	host := testHost("a")
	row := factsObservation(host.ID, "SAME", StateExited)
	later := testTime(20 * time.Second)
	row.LastAttachedAt = &later
	if name == "both-nil" {
		row.LastAttachedAt = nil
	}
	saveFacts(t, store, host, row)
	row.Facts = &SessionFacts{Label: "new"}
	incoming := later.Add(-10 * time.Second)
	row.LastAttachedAt = &incoming
	eligible := false
	switch name {
	case "nil":
		row.LastAttachedAt = nil
	case "newer":
		incoming = later.Add(time.Second)
		eligible = true
	case "same-millisecond":
		incoming = later.Add(900 * time.Microsecond)
		eligible = true
	case "both-nil":
		row.LastAttachedAt = nil
		eligible = true
	case "sequence-only":
		incoming = later
		row.LastOutputSequence = 1
		eligible = true
	}
	detach := testTime(15 * time.Second)
	row.Facts.DetachedAt = &detach
	saveFacts(t, store, host, row)
	got := readFacts(t, store, host.ID)[0]
	if (got.Facts != nil) != eligible {
		t.Fatalf("eligibility = %+v", got)
	}
	if got.LastOutputSequence != 20 {
		t.Fatal("sequence maximum changed")
	}
	if !eligible && (got.LastAttachedAt == nil || !got.LastAttachedAt.Equal(later)) {
		t.Fatal("attachment maximum changed")
	}
	if eligible && !reflect.DeepEqual(got.Facts, row.Facts) {
		t.Fatal("new facts not replaced")
	}
}

func TestCachedSessionFactsValidationAndByteLimits(t *testing.T) {
	for _, label := range []string{strings.Repeat("a", 2048), strings.Repeat("a", 4096), strings.Repeat("é", 2048), strings.Repeat("é", 2048) + "x", strings.Repeat("a", 4097)} {
		t.Run(fmt.Sprint(len(label))+"-bytes-"+fmt.Sprint(strings.HasPrefix(label, "é")), func(t *testing.T) { checkFactsLabel(t, label) })
	}
	store := openTestStore(t)
	host := testHost("a")
	row := factsObservation(host.ID, "GOOD", StateRunning)
	saveFacts(t, store, host, row)
	before := readFacts(t, store, host.ID)
	invalid := row
	invalid.Facts = &SessionFacts{Hibernated: true}
	assertFactsRejected(t, store, host, invalid, before)
	negative := time.UnixMilli(-1)
	invalid.Facts = &SessionFacts{DetachedAt: &negative}
	assertFactsRejected(t, store, host, invalid, before)
	factsSQL(t, store.db, `UPDATE cached_session_facts SET hibernated = 1 WHERE host_id = 'a'`)
	if _, err := store.ListObservedHostSessions(t.Context(), host.ID); err == nil {
		t.Fatal("inconsistent facts decoded")
	}
	if _, err := store.ListHostSessions(t.Context(), host.ID); err != nil {
		t.Fatal("base rows depend on facts", err)
	}
}

func assertFactsRejected(t *testing.T, store *Store, host Host, row ObservedSession, before []ObservedSession) {
	t.Helper()
	if err := store.ReconcileHost(t.Context(), host, []ObservedSession{row}); err == nil {
		t.Fatal("invalid facts accepted")
	}
	if got := readFacts(t, store, host.ID); !reflect.DeepEqual(got, before) {
		t.Fatal("invalid write changed snapshot")
	}
}

func checkFactsLabel(t *testing.T, label string) {
	store := openTestStore(t)
	host := testHost("a")
	row := factsObservation(host.ID, "LABEL", StateRunning)
	row.Facts.Label = label
	err := store.ReconcileHost(t.Context(), host, []ObservedSession{row})
	if len(label) <= 4096 {
		if err != nil {
			t.Fatal(err)
		}
		if readFacts(t, store, host.ID)[0].Facts.Label != label {
			t.Fatal("label changed")
		}
		return
	}
	if err == nil {
		t.Fatal("over-byte-limit Go write accepted")
	}
	row.Facts.Label = "valid"
	saveFacts(t, store, host, row)
	if _, err := store.db.ExecContext(t.Context(), `UPDATE cached_session_facts SET label = ?`, label); err == nil {
		t.Fatal("over-byte-limit SQL write accepted")
	}
	factsSQL(t, store.db, `PRAGMA ignore_check_constraints = ON`)
	factsSQL(t, store.db, `UPDATE cached_session_facts SET label = ?`, label)
	if _, err := store.ListObservedHostSessions(t.Context(), host.ID); err == nil {
		t.Fatal("over-byte-limit stored label decoded")
	}
}

func TestCachedSessionFactsRollbackInsideTransaction(t *testing.T) {
	for _, failure := range []string{"after-delete", "after-insert"} {
		t.Run(failure, func(t *testing.T) { checkFactsRollback(t, failure) })
	}
}

func checkFactsRollback(t *testing.T, failure string) {
	store := openTestStore(t)
	host := testHost("a")
	a := factsObservation(host.ID, "A", StateRunning)
	b := factsObservation(host.ID, "B", StateExited)
	saveFacts(t, store, host, a, b)
	before := readFacts(t, store, host.ID)
	message := "after facts deletion"
	trigger := `CREATE TRIGGER fail_facts BEFORE UPDATE ON sessions BEGIN SELECT CASE WHEN EXISTS (SELECT 1 FROM cached_session_facts WHERE host_id = 'a') THEN RAISE(ABORT,'facts not deleted') ELSE RAISE(ABORT,'after facts deletion') END; END`
	if failure == "after-insert" {
		message = "after first facts insertion"
		trigger = `CREATE TRIGGER fail_facts BEFORE INSERT ON cached_session_facts WHEN NEW.session_id = 'B' BEGIN SELECT CASE WHEN EXISTS (SELECT 1 FROM cached_session_facts WHERE host_id = 'a' AND session_id = 'A' AND label = 'changed') THEN RAISE(ABORT,'after first facts insertion') ELSE RAISE(ABORT,'first fact not inserted') END; END`
	}
	factsSQL(t, store.db, trigger)
	a.Cwd, a.Facts.Label = "/changed", "changed"
	host.LastSeenAt = host.LastSeenAt.Add(time.Hour)
	if err := store.ReconcileHost(t.Context(), host, []ObservedSession{a, b}); err == nil || !strings.Contains(err.Error(), message) {
		t.Fatalf("transaction did not reach injected failure: %v", err)
	}
	if got := readFacts(t, store, host.ID); !reflect.DeepEqual(got, before) {
		t.Fatalf("rollback snapshot = %+v, want %+v", got, before)
	}
	gotHost, err := store.GetHost(t.Context(), host.ID)
	if err != nil || !reflect.DeepEqual(gotHost, testHost("a")) {
		t.Fatal("host not rolled back", gotHost, err)
	}
}

func TestCachedSessionFactsJoinedSnapshot(t *testing.T) {
	store := openTestStore(t)
	host := testHost("a")
	row := factsObservation(host.ID, "ROW", StateRunning)
	row.Cwd, row.Facts.Label = "before", "before"
	saveFacts(t, store, host, row)
	tx, err := store.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(t.Context(), `UPDATE sessions SET cwd = 'after' WHERE host_id = 'a'`); err != nil {
		t.Fatal(err)
	}
	got := readFacts(t, store, host.ID)[0]
	if got.Cwd != "before" || got.Facts == nil || got.Facts.Label != "before" {
		t.Fatal("uncommitted invalidation leaked", got)
	}
	if _, err := tx.ExecContext(t.Context(), `INSERT INTO cached_session_facts VALUES ('a','ROW',0,0,'after',NULL)`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	got = readFacts(t, store, host.ID)[0]
	if got.Cwd != "after" || got.Facts == nil || got.Facts.Label != "after" {
		t.Fatal("joined committed snapshot mixed", got)
	}
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(t.Context())
	go func() { done <- alternateFactsSnapshots(ctx, store, host, row) }()
	defer func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	}()
	for range 40 {
		assertCoherentFacts(t, store, host.ID)
	}
}

func alternateFactsSnapshots(ctx context.Context, store *Store, host Host, row ObservedSession) error {
	for i := range 40 {
		row.Cwd = fmt.Sprint(i)
		row.Facts.Label = row.Cwd
		if err := store.ReconcileHost(ctx, host, []ObservedSession{row}); err != nil {
			return err
		}
	}
	return nil
}

func assertCoherentFacts(t *testing.T, store *Store, host HostID) {
	t.Helper()
	row := readFacts(t, store, host)[0]
	if row.Facts == nil || row.Cwd != row.Facts.Label {
		t.Fatalf("mixed shared row and facts: %+v", row)
	}
}

func TestCachedSessionFactsInvalidStoredScalars(t *testing.T) {
	for _, statement := range []string{"UPDATE cached_session_facts SET hibernated = 2", "UPDATE cached_session_facts SET recovery_pending = -1", "UPDATE cached_session_facts SET detached_at = -1"} {
		t.Run(strings.TrimPrefix(statement, "UPDATE cached_session_facts SET "), func(t *testing.T) { checkInvalidStoredFact(t, statement) })
	}
}

func checkInvalidStoredFact(t *testing.T, statement string) {
	store := openTestStore(t)
	store.db.SetMaxOpenConns(1)
	host := testHost("a")
	row := factsObservation(host.ID, "ROW", StateRunning)
	saveFacts(t, store, host, row)
	if _, err := store.db.ExecContext(t.Context(), statement); err == nil {
		t.Fatal("SQL constraint accepted invalid scalar")
	}
	factsSQL(t, store.db, `PRAGMA ignore_check_constraints = ON`)
	factsSQL(t, store.db, statement)
	if _, err := store.ListObservedHostSessions(t.Context(), host.ID); err == nil {
		t.Fatal("invalid stored facts decoded")
	}
	if _, err := store.GetSession(t.Context(), host.ID, row.ID); err != nil {
		t.Fatal("authoritative read depends on facts", err)
	}
}

func TestCachedSessionFactsRetirementKeepsActive(t *testing.T) {
	store := openTestStore(t)
	host := testHost("a")
	row := factsObservation(host.ID, "ACTIVE", StateRunning)
	saveFacts(t, store, host, row)
	count, err := store.RetireSessions(t.Context(), host.ID, []SessionID{row.ID})
	if err != nil || count != 0 {
		t.Fatal(count, err)
	}
	if err := store.ApplyHostChanges(t.Context(), host.ID, HostChanges{Retired: []SessionID{row.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyHostChanges(t.Context(), host.ID, HostChanges{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertHost(t.Context(), host); err != nil {
		t.Fatal(err)
	}
	if got := readFacts(t, store, host.ID); len(got) != 1 || !reflect.DeepEqual(got[0], row) {
		t.Fatal("non-session writes changed facts", got)
	}
}
