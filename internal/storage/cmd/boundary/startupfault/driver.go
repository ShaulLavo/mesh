// Package startupfault is linked only by the native proof's Go build overlay.
// Ordinary Mesh executables never import it.
package startupfault

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"strconv"

	"modernc.org/sqlite"
)

const DriverName = "mesh-boundary-sqlite"

func init() { sql.Register(DriverName, faultDriver{}) }

type faultDriver struct{}

func (faultDriver) Open(name string) (driver.Conn, error) {
	connection, err := (&sqlite.Driver{}).Open(name)
	if err != nil {
		return nil, fmt.Errorf("startup fault: SQLite open: %w", err)
	}
	return &faultConnection{Conn: connection}, nil
}

type faultConnection struct {
	driver.Conn
}

func (c *faultConnection) Ping(ctx context.Context) error {
	connection, ok := c.Conn.(driver.Pinger)
	if !ok {
		return errors.New("startup fault: SQLite lacks Ping")
	}
	return forwardError(connection.Ping(ctx), "ping")
}

func (c *faultConnection) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	connection, ok := c.Conn.(driver.ConnPrepareContext)
	if !ok {
		return nil, errors.New("startup fault: SQLite lacks PrepareContext")
	}
	statement, err := connection.PrepareContext(ctx, query)
	return statement, forwardError(err, "prepare")
}

func (c *faultConnection) ResetSession(ctx context.Context) error {
	connection, ok := c.Conn.(driver.SessionResetter)
	if !ok {
		return errors.New("startup fault: SQLite lacks ResetSession")
	}
	return forwardError(connection.ResetSession(ctx), "reset session")
}

func (c *faultConnection) IsValid() bool {
	connection, ok := c.Conn.(driver.Validator)
	return ok && connection.IsValid()
}

func (c *faultConnection) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *faultConnection) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	connection, ok := c.Conn.(driver.ConnBeginTx)
	if !ok {
		return nil, errors.New("startup fault: SQLite lacks BeginTx")
	}
	transaction, err := connection.BeginTx(ctx, options)
	if err != nil {
		return nil, fmt.Errorf("startup fault: SQLite begin: %w", err)
	}
	return &faultTransaction{Tx: transaction, connection: c.Conn}, nil
}

func (c *faultConnection) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	connection, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	result, err := connection.ExecContext(ctx, query, args)
	return result, forwardError(err, "execute")
}

func (c *faultConnection) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	connection, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	rows, err := connection.QueryContext(ctx, query, args)
	return rows, forwardError(err, "query")
}

func (c *faultConnection) Exec(query string, args []driver.Value) (driver.Result, error) {
	connection, ok := c.Conn.(interface {
		Exec(string, []driver.Value) (driver.Result, error)
	})
	if !ok {
		return nil, driver.ErrSkip
	}
	result, err := connection.Exec(query, args)
	return result, forwardError(err, "execute")
}

func (c *faultConnection) Query(query string, args []driver.Value) (driver.Rows, error) {
	connection, ok := c.Conn.(interface {
		Query(string, []driver.Value) (driver.Rows, error)
	})
	if !ok {
		return nil, driver.ErrSkip
	}
	rows, err := connection.Query(query, args)
	return rows, forwardError(err, "query")
}

type faultTransaction struct {
	driver.Tx
	connection driver.Conn
}

func (t *faultTransaction) Commit() error {
	version := selectedVersion(t.connection)
	if version > 0 {
		marker("armed", version)
	}
	if version > 0 && os.Getenv("MESH_STARTUP_FAULT_SIDE") == "before" {
		return t.inject(version, true)
	}
	if err := t.Tx.Commit(); err != nil {
		return fmt.Errorf("startup fault: SQLite commit: %w", err)
	}
	if version > 0 && os.Getenv("MESH_STARTUP_FAULT_SIDE") == "after" {
		return t.inject(version, false)
	}
	return nil
}

func selectedVersion(connection driver.Conn) int64 {
	wanted, err := strconv.ParseInt(os.Getenv("MESH_STARTUP_FAULT_VERSION"), 10, 64)
	if err != nil || wanted <= 0 {
		return 0
	}
	query, ok := connection.(driver.QueryerContext)
	if !ok {
		return 0
	}
	rows, err := query.QueryContext(context.Background(), "SELECT MAX(version_id) FROM goose_db_version WHERE is_applied = 1", nil)
	if err != nil {
		return 0
	}
	defer func() { _ = rows.Close() }()
	values := make([]driver.Value, 1)
	if err := rows.Next(values); err != nil || values[0] != wanted {
		return 0
	}
	return wanted
}

func (t *faultTransaction) inject(version int64, before bool) error {
	marker("fired", version)
	if os.Getenv("MESH_STARTUP_FAULT_MODE") == "interrupt" {
		os.Exit(86)
	}
	if before {
		if err := t.Rollback(); err != nil {
			return fmt.Errorf("startup fault: rollback before failed commit: %w", err)
		}
	}
	return errors.New("injected candidate migration commit failure")
}

func marker(event string, version int64) {
	_, _ = fmt.Fprintf(os.Stderr, "mesh startup commit hook event=%s version=%d side=%s mode=%s pid=%d\n",
		event, version, os.Getenv("MESH_STARTUP_FAULT_SIDE"), os.Getenv("MESH_STARTUP_FAULT_MODE"), os.Getpid())
}

func forwardError(err error, operation string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, driver.ErrSkip) {
		return driver.ErrSkip
	}
	if errors.Is(err, driver.ErrBadConn) {
		return driver.ErrBadConn
	}
	return fmt.Errorf("startup fault: SQLite %s: %w", operation, err)
}
