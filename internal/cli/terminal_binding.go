package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/session"
)

const (
	terminalBindingVersion = 1
	maximumBindingBytes    = 64 << 10
)

// TerminalBinding is the session a terminal last opened.
//
// It records intent, not a completed attachment. The client blocks in a read
// that a host crash never returns from, and a closed tab runs no deferred code,
// so the record has to be durable before the attach begins rather than after it
// ends.
type TerminalBinding struct {
	Version int    `json:"version"`
	Source  string `json:"source"`
	// HostID is empty for a session on this machine. The alias is derived at
	// read time: "this host" is not a real alias, and `mesh rename` would rot a
	// stored one.
	HostID    string `json:"hostId,omitempty"`
	SessionID string `json:"sessionId"`
	// OriginID is the session recovery is asked to bring back. Recovery mints a
	// new id every time it runs, so SessionID moves and OriginID stays put.
	OriginID string `json:"originId,omitempty"`
	// CreatedAt guards against a rebound id. Session ids are four characters
	// and are freed when a directory is removed, so an id alone can match a
	// later, unrelated session.
	CreatedAt time.Time `json:"createdAt,omitempty"`
	BoundAt   time.Time `json:"boundAt"`
}

func (b TerminalBinding) validate() error {
	if b.Version != terminalBindingVersion {
		return fmt.Errorf("terminal binding version %d is unsupported", b.Version)
	}
	if _, err := session.ParseID(b.SessionID); err != nil {
		return fmt.Errorf("terminal binding session: %w", err)
	}
	if b.OriginID != "" {
		if _, err := session.ParseID(b.OriginID); err != nil {
			return fmt.Errorf("terminal binding origin: %w", err)
		}
	}
	return nil
}

// bindingDir mirrors the per-feature layout every other package uses under the
// state directory: derived, machine-local, and thrown away with it.
func bindingDir() (string, error) {
	stateDir, err := paths.StateDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(stateDir, "terminals")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create terminal binding directory %s: %w", dir, err)
	}
	return dir, nil
}

func bindingPath(key string) (string, error) {
	dir, err := bindingDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, key+".json"), nil
}

// loadTerminalBinding reports the binding for a terminal, and whether there was
// one. A record that no longer parses is treated as absent: a tab losing its
// memory is a small annoyance, and failing every command over it is worse.
func loadTerminalBinding(key string) (TerminalBinding, bool) {
	path, err := bindingPath(key)
	if err != nil {
		return TerminalBinding{}, false
	}
	file, err := os.Open(path) //nolint:gosec // path is a hashed terminal key under the state directory
	if err != nil {
		return TerminalBinding{}, false
	}
	defer func() { _ = file.Close() }()
	contents, err := io.ReadAll(io.LimitReader(file, maximumBindingBytes+1))
	if err != nil || len(contents) > maximumBindingBytes {
		return TerminalBinding{}, false
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var binding TerminalBinding
	if err := decoder.Decode(&binding); err != nil {
		return TerminalBinding{}, false
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return TerminalBinding{}, false
	}
	if err := binding.validate(); err != nil {
		return TerminalBinding{}, false
	}
	return binding, true
}

// saveTerminalBinding replaces a terminal's record atomically. There is no lock
// here on purpose: every write replaces the whole record rather than editing
// one, so the rename is the only ordering that matters, and a lock would add a
// way for `mesh` to stall in a tab without adding correctness.
func saveTerminalBinding(key string, binding TerminalBinding) error {
	binding.Version = terminalBindingVersion
	if err := binding.validate(); err != nil {
		return err
	}
	path, err := bindingPath(key)
	if err != nil {
		return err
	}
	contents, err := json.Marshal(binding)
	if err != nil {
		return fmt.Errorf("encode terminal binding: %w", err)
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".terminal-*")
	if err != nil {
		return fmt.Errorf("create terminal binding: %w", err)
	}
	defer func() { _ = os.Remove(file.Name()) }()
	defer func() { _ = file.Close() }()
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("secure terminal binding: %w", err)
	}
	if _, err := file.Write(append(contents, '\n')); err != nil {
		return fmt.Errorf("write terminal binding: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync terminal binding: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close terminal binding: %w", err)
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return fmt.Errorf("publish terminal binding %s: %w", path, err)
	}
	return syncBindingDir(filepath.Dir(path))
}

func forgetTerminalBinding(key string) error {
	path, err := bindingPath(key)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove terminal binding %s: %w", path, err)
	}
	return syncBindingDir(filepath.Dir(path))
}

// syncBindingDir makes the rename itself durable. Filesystems that do not
// support directory sync report EINVAL, which is not a failure to record.
func syncBindingDir(dir string) error {
	handle, err := os.Open(dir) //nolint:gosec // dir is the binding directory this package created under the state directory
	if err != nil {
		return fmt.Errorf("open terminal binding directory: %w", err)
	}
	defer func() { _ = handle.Close() }()
	if err := handle.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return fmt.Errorf("sync terminal binding directory: %w", err)
	}
	return nil
}
