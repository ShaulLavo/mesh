package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strconv"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// SQLite's busy handler can sleep through cancellation. Retry only admission,
// in short native waits, so a five-second budget still honors shorter callers.
func (s *Store) beginCatalogWrite(ctx context.Context) (*sql.Tx, func(), error) {
	if !s.advisory {
		tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
		if err != nil {
			return nil, nil, fmt.Errorf("begin catalog write: %w", err)
		}
		return tx, func() {}, nil
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("reserve catalog writer: %w", err)
	}
	release := func() { releaseCatalogWriter(context.WithoutCancel(ctx), conn) }
	tx, err := admitCatalogWrite(ctx, conn)
	if err != nil {
		release()
		return nil, nil, err
	}
	return tx, release, nil
}

func admitCatalogWrite(ctx context.Context, conn *sql.Conn) (*sql.Tx, error) {
	deadline := time.Now().Add(sqliteCacheBusyTimeout)
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("admit catalog writer: %w", err)
		}
		wait := min(100*time.Millisecond, time.Until(deadline))
		if callerDeadline, ok := ctx.Deadline(); ok {
			wait = min(wait, time.Until(callerDeadline))
		}
		// Keep zero as an immediate attempt when the remaining fraction is < 1 ms.
		millis := max(0, wait.Milliseconds())
		if _, err := conn.ExecContext(ctx, "PRAGMA busy_timeout="+strconv.FormatInt(millis, 10)); err != nil {
			return nil, fmt.Errorf("set catalog admission wait: %w", err)
		}
		tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
		if err == nil {
			return tx, nil
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("admit catalog writer: %w", ctx.Err())
		}
		if !catalogWriterBusy(err) {
			return nil, fmt.Errorf("begin catalog write: %w", err)
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("%w: %w", ErrAdvisoryWriteBusy, err)
		}
	}
}

func catalogWriterBusy(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code() == sqlite3.SQLITE_BUSY
}

func releaseCatalogWriter(ctx context.Context, conn *sql.Conn) {
	if _, err := conn.ExecContext(ctx, "PRAGMA busy_timeout="+strconv.FormatInt(sqliteCacheBusyTimeout.Milliseconds(), 10)); err != nil {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	}
	_ = conn.Close()
}
