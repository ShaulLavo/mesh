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

	closeOnce sync.Once
	closeErr  error
}

// Open opens databasePath, applies every pending migration, and returns a Store.
// The caller owns the parent state directory and supplies its path explicitly.
func Open(ctx context.Context, databasePath string) (*Store, error) {
	dsn, err := sqliteDSN(databasePath)
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
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: migrate %s: %w", databasePath, err)
	}
	return &Store{db: db, queries: dbsqlc.New(db)}, nil
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
	params := make([]dbsqlc.UpsertSessionParams, 0, len(observed))
	seen := make(map[SessionID]struct{}, len(observed))
	for _, session := range observed {
		if session.HostID != hostID {
			return fmt.Errorf("storage: reconcile host %s: session %s belongs to host %s", hostID, session.ID, session.HostID)
		}
		if _, ok := seen[session.ID]; ok {
			return fmt.Errorf("storage: reconcile host %s: duplicate session %s", hostID, session.ID)
		}
		seen[session.ID] = struct{}{}
		p, err := sessionParams(session)
		if err != nil {
			return fmt.Errorf("storage: reconcile host %s: %w", hostID, err)
		}
		params = append(params, p)
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

func migrate(ctx context.Context, db *sql.DB) error {
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrations.Files)
	if err != nil {
		return fmt.Errorf("create Goose provider: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("apply Goose migrations: %w", err)
	}
	return nil
}

func sqliteDSN(databasePath string) (string, error) {
	if databasePath == "" {
		return "", fmt.Errorf("storage: empty database path")
	}
	abs, err := filepath.Abs(databasePath)
	if err != nil {
		return "", fmt.Errorf("storage: resolve database path %s: %w", databasePath, err)
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
	query := u.Query()
	query.Set("_busy_timeout", sqliteBusyTimeout)
	query.Set("_foreign_keys", "on")
	query.Set("_journal_mode", "WAL")
	query.Set("_synchronous", "NORMAL")
	u.RawQuery = query.Encode()
	return u.String(), nil
}
