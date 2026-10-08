// Package storage owns the per-host SQLite metadata store.
package storage

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"sync"

	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"
	_ "modernc.org/sqlite"

	"github.com/shaul/mesh/db/migrations"
	dbsqlc "github.com/shaul/mesh/internal/storage/sqlc"
)

const (
	sqliteDriver       = "sqlite"
	sqliteBusyTimeout  = "5000"
	sqliteMaxOpenConns = 4
)

// Store owns one SQLite connection pool. Close it when the daemon stops.
type Store struct {
	db      *sql.DB
	queries *dbsqlc.Queries
	path    string

	closeOnce sync.Once
	closeErr  error
}

// Open opens databasePath, applies every pending migration, and returns a Store.
// The caller owns the parent state directory and supplies its path explicitly.
func Open(ctx context.Context, databasePath string) (*Store, error) {
	return open(ctx, databasePath, sqliteBusyTimeout)
}

// OpenAdvisory skips lock contention instead of delaying authoritative work.
// The DSN applies the policy to every connection opened by the pool.
func OpenAdvisory(ctx context.Context, databasePath string) (*Store, error) {
	return open(ctx, databasePath, "0")
}

func open(ctx context.Context, databasePath, busyTimeout string) (*Store, error) {
	dsn, err := sqliteDSN(databasePath, busyTimeout)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open(sqliteDriver, dsn)
	if err != nil {
		return nil, fmt.Errorf("storage: open %s: %w", databasePath, err)
	}
	db.SetMaxOpenConns(sqliteMaxOpenConns)
	db.SetMaxIdleConns(sqliteMaxOpenConns)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: connect to %s: %w", databasePath, err)
	}
	if err := migrate(ctx, db, busyTimeout == "0"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: migrate %s: %w", databasePath, err)
	}
	return &Store{db: db, queries: dbsqlc.New(db), path: databasePath}, nil
}

// Close releases every database handle. It is safe to call more than once.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.db.Close()
	})
	return s.closeErr
}

// UpsertHost records the complete current observation of a host.
func (s *Store) UpsertHost(ctx context.Context, host Host) (Host, error) {
	params, err := hostParams(host)
	if err != nil {
		return Host{}, err
	}
	row, err := s.queries.UpsertHost(ctx, params)
	if err != nil {
		return Host{}, fmt.Errorf("storage: upsert host %s: %w", host.ID, err)
	}
	persisted, err := hostFromRow(row)
	if err != nil {
		return Host{}, fmt.Errorf("storage: read upserted host %s: %w", host.ID, err)
	}
	return persisted, nil
}

// GetHost returns one host by its stable ID.
func (s *Store) GetHost(ctx context.Context, id HostID) (Host, error) {
	if err := validateHostID(id); err != nil {
		return Host{}, err
	}
	row, err := s.queries.GetHost(ctx, string(id))
	if err != nil {
		return Host{}, fmt.Errorf("storage: get host %s: %w", id, err)
	}
	host, err := hostFromRow(row)
	if err != nil {
		return Host{}, fmt.Errorf("storage: get host %s: %w", id, err)
	}
	return host, nil
}

// ListHosts returns hosts in descending order of last observation.
func (s *Store) ListHosts(ctx context.Context) ([]Host, error) {
	rows, err := s.queries.ListHosts(ctx)
	if err != nil {
		return nil, fmt.Errorf("storage: list hosts: %w", err)
	}
	hosts := make([]Host, 0, len(rows))
	for _, row := range rows {
		host, err := hostFromRow(row)
		if err != nil {
			return nil, fmt.Errorf("storage: list hosts: %w", err)
		}
		hosts = append(hosts, host)
	}
	return hosts, nil
}

// UpsertSession records the complete current observation of a session.
func (s *Store) UpsertSession(ctx context.Context, session Session) (Session, error) {
	params, err := sessionParams(session)
	if err != nil {
		return Session{}, err
	}
	row, err := s.queries.UpsertSession(ctx, params)
	if err != nil {
		return Session{}, fmt.Errorf("storage: upsert session %s/%s: %w", session.HostID, session.ID, err)
	}
	persisted, err := sessionFromRow(row)
	if err != nil {
		return Session{}, fmt.Errorf("storage: read upserted session %s/%s: %w", session.HostID, session.ID, err)
	}
	return persisted, nil
}

// GetSession returns one session by its host and session IDs.
func (s *Store) GetSession(ctx context.Context, hostID HostID, sessionID SessionID) (Session, error) {
	if err := validateHostID(hostID); err != nil {
		return Session{}, err
	}
	if err := validateSessionID(sessionID); err != nil {
		return Session{}, err
	}
	row, err := s.queries.GetSession(ctx, dbsqlc.GetSessionParams{
		HostID: string(hostID),
		ID:     string(sessionID),
	})
	if err != nil {
		return Session{}, fmt.Errorf("storage: get session %s/%s: %w", hostID, sessionID, err)
	}
	session, err := sessionFromRow(row)
	if err != nil {
		return Session{}, fmt.Errorf("storage: get session %s/%s: %w", hostID, sessionID, err)
	}
	return session, nil
}

// ListHostSessions returns every session for hostID, newest first.
func (s *Store) ListHostSessions(ctx context.Context, hostID HostID) ([]Session, error) {
	if err := validateHostID(hostID); err != nil {
		return nil, err
	}
	rows, err := s.queries.ListHostSessions(ctx, string(hostID))
	if err != nil {
		return nil, fmt.Errorf("storage: list host %s sessions: %w", hostID, err)
	}
	return sessionsFromRows(rows, fmt.Sprintf("list host %s sessions", hostID))
}

// ListSessionsByState returns every session in state, newest first.
func (s *Store) ListSessionsByState(ctx context.Context, state SessionState) ([]Session, error) {
	if err := validateState(state); err != nil {
		return nil, err
	}
	rows, err := s.queries.ListSessionsByState(ctx, string(state))
	if err != nil {
		return nil, fmt.Errorf("storage: list %s sessions: %w", state, err)
	}
	return sessionsFromRows(rows, fmt.Sprintf("list %s sessions", state))
}

// SetSessionState updates the observed state without changing session metadata.
func (s *Store) SetSessionState(ctx context.Context, hostID HostID, sessionID SessionID, state SessionState, exitCode *int) (Session, error) {
	if err := validateHostID(hostID); err != nil {
		return Session{}, err
	}
	if err := validateSessionID(sessionID); err != nil {
		return Session{}, err
	}
	if err := validateState(state); err != nil {
		return Session{}, err
	}
	if err := validateExit(state, exitCode); err != nil {
		return Session{}, fmt.Errorf("storage: session %s: %w", sessionID, err)
	}
	row, err := s.queries.SetSessionState(ctx, dbsqlc.SetSessionStateParams{
		State:    string(state),
		ExitCode: intToInt64(exitCode),
		HostID:   string(hostID),
		ID:       string(sessionID),
	})
	if err != nil {
		return Session{}, fmt.Errorf("storage: set session %s/%s state: %w", hostID, sessionID, err)
	}
	persisted, err := sessionFromRow(row)
	if err != nil {
		return Session{}, fmt.Errorf("storage: read session %s/%s state: %w", hostID, sessionID, err)
	}
	return persisted, nil
}

// ReconcileHost atomically records a host and replaces its active session
// observations. Existing running or detached rows missing from observed become
// interrupted. Exited and already interrupted history remains intact.
func (s *Store) ReconcileHost(ctx context.Context, host Host, observed []Session) error {
	hostValues, err := hostParams(host)
	if err != nil {
		return err
	}
	hostID := host.ID
	params, err := sessionChangeParams(hostID, observed)
	if err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("storage: reconcile host %s: begin transaction: %w", hostID, err)
	}
	defer tx.Rollback() //nolint:errcheck // commit decides the transaction outcome
	queries := s.queries.WithTx(tx)
	if _, err := queries.UpsertHost(ctx, hostValues); err != nil {
		return fmt.Errorf("storage: reconcile host %s: upsert host: %w", hostID, err)
	}
	if err := queries.InterruptActiveSessionsForHost(ctx, string(hostID)); err != nil {
		return fmt.Errorf("storage: reconcile host %s: interrupt missing sessions: %w", hostID, err)
	}
	for _, p := range params {
		if _, err := queries.UpsertSession(ctx, p); err != nil {
			return fmt.Errorf("storage: reconcile host %s: upsert session %s: %w", hostID, p.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: reconcile host %s: commit: %w", hostID, err)
	}
	return nil
}

func sessionsFromRows(rows []dbsqlc.Session, operation string) ([]Session, error) {
	sessions := make([]Session, 0, len(rows))
	for _, row := range rows {
		session, err := sessionFromRow(row)
		if err != nil {
			return nil, fmt.Errorf("storage: %s: %w", operation, err)
		}
		sessions = append(sessions, session)
	}
	return sessions, nil
}

func migrate(ctx context.Context, db *sql.DB, advisory bool) error {
	if advisory {
		// Goose retries missing version-table creation even with SQLite's writer wait disabled.
		if err := ensureAdvisoryVersionTable(ctx, db); err != nil {
			return fmt.Errorf("initialize advisory migration history: %w", err)
		}
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrations.Files)
	if err != nil {
		return fmt.Errorf("create Goose provider: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("apply Goose migrations: %w", err)
	}
	return nil
}

func ensureAdvisoryVersionTable(ctx context.Context, db *sql.DB) error {
	store, err := database.NewStore(database.DialectSQLite3, goose.DefaultTablename)
	if err != nil {
		return fmt.Errorf("create migration history store: %w", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration history setup: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var exists bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?)", goose.DefaultTablename).Scan(&exists); err != nil {
		return fmt.Errorf("check migration history table: %w", err)
	}
	if exists {
		return nil
	}
	if err := store.CreateVersionTable(ctx, tx); err != nil {
		return fmt.Errorf("create migration history table: %w", err)
	}
	if err := store.Insert(ctx, tx, database.InsertRequest{Version: 0}); err != nil {
		return fmt.Errorf("record initial migration version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration history setup: %w", err)
	}
	return nil
}

func sqliteDSN(databasePath, busyTimeout string) (string, error) {
	if databasePath == "" {
		return "", fmt.Errorf("storage: empty database path")
	}
	abs, err := filepath.Abs(databasePath)
	if err != nil {
		return "", fmt.Errorf("storage: resolve database path %s: %w", databasePath, err)
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
	query := u.Query()
	query.Set("_busy_timeout", busyTimeout)
	query.Set("_foreign_keys", "on")
	query.Set("_journal_mode", "WAL")
	query.Set("_synchronous", "NORMAL")
	u.RawQuery = query.Encode()
	return u.String(), nil
}

// RetireSessions deletes the given finished sessions for one host. The caller
// decides what to retire; this refuses to touch a running session, so a
// mistaken ID cannot remove a live record.
//
// Nothing deleted a session row before, so the reconciler re-read and
// re-upserted the whole history every tick and session.listed eventually
// outgrew the 4 MiB frame cap, at which point mesh ls failed permanently.
func (s *Store) RetireSessions(ctx context.Context, hostID HostID, retired []SessionID) (int64, error) {
	if len(retired) == 0 {
		return 0, nil
	}
	ids := make([]string, 0, len(retired))
	for _, id := range retired {
		ids = append(ids, string(id))
	}
	deleted, err := s.queries.DeleteRetiredHostSessions(ctx, dbsqlc.DeleteRetiredHostSessionsParams{
		HostID:  string(hostID),
		Retired: ids,
	})
	if err != nil {
		return 0, fmt.Errorf("storage: retire sessions for host %s: %w", hostID, err)
	}
	return deleted, nil
}
