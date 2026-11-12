package storage

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"

	dbsqlc "github.com/shaul/mesh/internal/storage/sqlc"
)

// OpenReadOnly opens existing metadata without creating a database or applying
// migrations. SQLite also rejects writes made through the returned Store.
func OpenReadOnly(ctx context.Context, databasePath string) (*Store, error) {
	absolute, err := filepath.Abs(databasePath)
	if err != nil {
		return nil, fmt.Errorf("storage: resolve read-only database: %w", err)
	}
	query := url.Values{"mode": {"ro"}}
	query.Add("_pragma", "busy_timeout("+strconv.FormatInt(sqliteCacheReadBusyTimeout.Milliseconds(), 10)+")")
	query.Add("_pragma", "query_only(1)")
	dsn := (&url.URL{Scheme: "file", Path: absolute, RawQuery: query.Encode()}).String()
	db, err := sql.Open(sqliteDriver, dsn)
	if err != nil {
		return nil, fmt.Errorf("storage: open read-only database: %w", err)
	}
	db.SetMaxOpenConns(sqliteMaxOpenConns)
	db.SetMaxIdleConns(sqliteMaxOpenConns)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: connect to read-only database: %w", err)
	}
	return &Store{db: db, queries: dbsqlc.New(db), path: absolute, advisory: true}, nil
}
