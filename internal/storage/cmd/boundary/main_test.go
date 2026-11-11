package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func migrationFixture() fstest.MapFS {
	return fstest.MapFS{
		"00001_source.sql":       {Data: []byte("-- +goose Up\nCREATE TABLE retained(value TEXT);\nINSERT INTO retained VALUES ('source');\n-- +goose Down\nDROP TABLE retained;\n")},
		"00002_intermediate.sql": {Data: []byte("-- +goose Up\nALTER TABLE retained RENAME TO temporary_retained;\n-- +goose Down\nALTER TABLE temporary_retained RENAME TO retained;\n")},
		"00003_final.sql":        {Data: []byte("-- +goose Up\nALTER TABLE temporary_retained RENAME TO retained;\n-- +goose Down\nALTER TABLE retained RENAME TO temporary_retained;\n")},
	}
}

func sourceDatabase(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mesh.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := openDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	_, err = db.ExecContext(t.Context(), `CREATE TABLE goose_db_version (id INTEGER PRIMARY KEY AUTOINCREMENT, version_id INTEGER NOT NULL, is_applied BOOLEAN NOT NULL, tstamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP); INSERT INTO goose_db_version(version_id,is_applied) VALUES(0,1);`)
	if err != nil {
		t.Fatal(err)
	}
	if err := apply(t.Context(), path, 1, "none", migrationFixture()); err != nil {
		t.Fatal(err)
	}
	return path
}

func readSource(path string) error {
	db, err := openDatabase(path)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	var value string
	if err := db.QueryRowContext(context.Background(), "SELECT value FROM retained").Scan(&value); err != nil {
		return fmt.Errorf("source query: %w", err)
	}
	return nil
}

func TestIntermediateCommittedStateCanFailWhileFinalStatePasses(t *testing.T) {
	path := sourceDatabase(t)
	if err := apply(t.Context(), path, 2, "none", migrationFixture()); err != nil {
		t.Fatal(err)
	}
	if err := readSource(path); err == nil {
		t.Fatal("intermediate state unexpectedly supports source query")
	}
	if err := apply(t.Context(), path, 3, "none", migrationFixture()); err != nil {
		t.Fatal(err)
	}
	if err := readSource(path); err != nil {
		t.Fatalf("final-only proof would pass: %v", err)
	}
}

func TestMigrationFailureLeavesSourceState(t *testing.T) {
	path := sourceDatabase(t)
	if err := apply(t.Context(), path, 2, "failure", migrationFixture()); err == nil {
		t.Fatal("migration failure was not injected")
	}
	if err := readSource(path); err != nil {
		t.Fatalf("failed migration did not roll back its transaction: %v", err)
	}
}

func TestBoundaryAbruptExitHelper(t *testing.T) {
	path := os.Getenv("MESH_BOUNDARY_TEST_DATABASE")
	if path == "" {
		return
	}
	if err := apply(t.Context(), path, 2, os.Getenv("MESH_BOUNDARY_TEST_FAULT"), migrationFixture()); err != nil {
		t.Fatal(err)
	}
	t.Fatal("expected abrupt process exit")
}

func TestMigrationInterruptionBeforeAndAfterCommit(t *testing.T) {
	for _, fault := range []string{"interrupt", "after-commit"} {
		t.Run(fault, func(t *testing.T) { checkAbruptExit(t, fault) })
	}
}

func checkAbruptExit(t *testing.T, fault string) {
	t.Helper()
	path := sourceDatabase(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "sh", "-c", `exec "$MESH_BOUNDARY_TEST_EXECUTABLE" -test.run=^TestBoundaryAbruptExitHelper$`)
	command.Env = append(os.Environ(), "MESH_BOUNDARY_TEST_EXECUTABLE="+executable, "MESH_BOUNDARY_TEST_DATABASE="+path, "MESH_BOUNDARY_TEST_FAULT="+fault)
	output, err := command.CombinedOutput()
	if command.ProcessState == nil || command.ProcessState.ExitCode() != 86 {
		t.Fatalf("fault %s did not exit inside driver: %v, %s", fault, err, output)
	}
	readErr := readSource(path)
	if (fault == "interrupt") != (readErr == nil) {
		t.Fatalf("source query after %s: %v", fault, readErr)
	}
}

func TestNonTransactionalMigrationsNeedStatementBoundaryProof(t *testing.T) {
	files := migrationFixture()
	files["00002_intermediate.sql"].Data = append([]byte("-- +goose NO TRANSACTION\n"), files["00002_intermediate.sql"].Data...)
	if _, err := boundaryFiles(files, 1, 2, "none"); err == nil || !strings.Contains(err.Error(), "statement states") {
		t.Fatalf("nontransactional migration accepted: %v", err)
	}
}
