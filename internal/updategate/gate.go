// Package updategate prevents new workers from entering an uncommitted installation.
package updategate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var ErrUpdating = errors.New("Mesh update is validating; retry session creation shortly")

type UpdatingError struct{ Operation string }

func (e *UpdatingError) Error() string {
	return fmt.Sprintf("%s (operation %s)", ErrUpdating, e.Operation)
}
func (e *UpdatingError) Unwrap() error { return ErrUpdating }

func Path(stateDir string) string { return filepath.Join(stateDir, "update", "activation.gate") }

func Check(stateDir string) error {
	data, err := os.ReadFile(Path(stateDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read update creation gate: %w", err)
	}
	return &UpdatingError{Operation: strings.TrimSpace(string(data))}
}

func Set(stateDir, operation string) error {
	if operation == "" || strings.ContainsAny(operation, "\r\n") {
		return errors.New("invalid update operation")
	}
	dir := filepath.Dir(Path(stateDir))
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".gate-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if _, err = file.WriteString(operation + "\n"); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Rename(file.Name(), Path(stateDir)); err != nil {
		return err
	}
	return syncDir(dir)
}

func Clear(stateDir, operation string) error {
	data, err := os.ReadFile(Path(stateDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(data)) != operation {
		return errors.New("update gate belongs to another operation")
	}
	if err = os.Remove(Path(stateDir)); err != nil {
		return err
	}
	return syncDir(filepath.Dir(Path(stateDir)))
}

func syncDir(path string) error {
	dir, err := os.Open(path) //nolint:gosec // fixed journal, gate, or executable path beneath configured local directories
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}
