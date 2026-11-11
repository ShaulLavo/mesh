// Command boundary exercises candidate migrations without adding release-binary flags.
package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing/fstest"

	"github.com/pressly/goose/v3"
	"modernc.org/sqlite"

	"github.com/shaul/mesh/db/migrations"
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("boundary", flag.ContinueOnError)
	databasePath := flags.String("database", "", "isolated source database")
	version := flags.Int64("version", 0, "last candidate migration to commit")
	fault := flags.String("fault", "none", "none, failure, interrupt, or after-commit")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("boundary: flags: %w", err)
	}
	if *databasePath == "" || *version <= 0 || flags.NArg() != 0 {
		return errors.New("boundary: -database and positive -version are required")
	}
	return apply(ctx, *databasePath, *version, *fault, migrations.Files)
}

func openDatabase(path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("boundary: database path: %w", err)
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
	u.RawQuery = "mode=rw&_busy_timeout=5000&_foreign_keys=on&_journal_mode=WAL&_synchronous=NORMAL"
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("boundary: database: %w", err)
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func apply(ctx context.Context, path string, version int64, fault string, sources fs.FS) error {
	if fault != "none" && fault != "failure" && fault != "interrupt" && fault != "after-commit" {
		return fmt.Errorf("boundary: unknown fault %q", fault)
	}
	db, err := openDatabase(path)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	var current int64
	if err := db.QueryRowContext(ctx, "SELECT MAX(version_id) FROM goose_db_version WHERE is_applied = 1").Scan(&current); err != nil {
		return fmt.Errorf("boundary: source version: %w", err)
	}
	files, err := boundaryFiles(sources, current, version, fault)
	if err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, files, goose.WithTableName(goose.DefaultTablename))
	if err != nil {
		return fmt.Errorf("boundary: Goose provider: %w", err)
	}
	if _, err := provider.UpTo(ctx, version); err != nil {
		return fmt.Errorf("boundary: Goose migrations through %d: %w", version, err)
	}
	actual, err := provider.GetDBVersion(ctx)
	if err != nil || actual != version {
		return fmt.Errorf("boundary: expected state %d, got %d: %w", version, actual, err)
	}
	if fault == "after-commit" {
		os.Exit(86)
	}
	return nil
}

func boundaryFiles(sources fs.FS, current, version int64, fault string) (fs.FS, error) {
	entries, err := fs.Glob(sources, "*.sql")
	if err != nil {
		return nil, fmt.Errorf("boundary: list migrations: %w", err)
	}
	files := fstest.MapFS{}
	for _, name := range entries {
		if err := addMigration(files, sources, name, current, version, fault); err != nil {
			return nil, err
		}
	}
	return files, nil
}

func addMigration(files fstest.MapFS, sources fs.FS, name string, current, version int64, fault string) error {
	prefix, _, _ := strings.Cut(name, "_")
	number, err := strconv.ParseInt(prefix, 10, 64)
	if err != nil {
		return fmt.Errorf("boundary: migration %s: %w", name, err)
	}
	data, err := fs.ReadFile(sources, name)
	if err != nil {
		return fmt.Errorf("boundary: read %s: %w", name, err)
	}
	if number > current && strings.Contains(string(data), "-- +goose NO TRANSACTION") {
		return fmt.Errorf("boundary: %s has reachable uncommitted statement states; transactional boundary proof is insufficient", name)
	}
	if number == version && (fault == "failure" || fault == "interrupt") {
		data = injectFault(data, fault)
	}
	files[name] = &fstest.MapFile{Data: data, Mode: 0o600}
	return nil
}

func injectFault(data []byte, fault string) []byte {
	statement := "\nSELECT mesh_boundary_failure();\n"
	if fault == "interrupt" {
		statement = "\nSELECT mesh_boundary_interrupt();\n"
	}
	up, down, found := strings.Cut(string(data), "-- +goose Down")
	if !found {
		return []byte(up + statement)
	}
	return []byte(up + statement + "-- +goose Down" + down)
}

func init() {
	sqlite.MustRegisterScalarFunction("mesh_boundary_interrupt", 0, func(_ *sqlite.FunctionContext, _ []driver.Value) (driver.Value, error) {
		// Exit inside the migration transaction without closing its connection.
		os.Exit(86)
		return nil, nil
	})
}
