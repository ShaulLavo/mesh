package machinename

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/shaul/mesh/internal/identity"
)

var ErrRevision = errors.New("machine name changed; read its current name and retry")
var ErrTarget = errors.New("rename targets another machine; use the destination's exact ID")

const stateName = "machine-name.json"
const maximumStateBytes = 4096

type record struct {
	Version int `json:"version"`
	Claim
	PreviousRevision *uint64 `json:"previousRevision,omitempty"`
}

// Store has one writer, the daemon holding daemon.lock. Clients rename through
// control requests; they never write this file or replace the host identity.
type Store struct {
	mu            sync.Mutex
	directory     string
	current       record
	pending       *record
	syncDirectory func(string) error
}

func Open(ctx context.Context, directory, id, initial string) (*Store, error) {
	return openStore(ctx, directory, id, initial, syncDirectory)
}

func openStore(ctx context.Context, directory, id, initial string, syncDir func(string) error) (*Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("open machine name: %w", err)
	}
	if _, err := identity.IdentityKey(id); err != nil {
		return nil, fmt.Errorf("machine name identity: %w", err)
	}
	if directory == "" {
		return nil, errors.New("machine name state directory is empty")
	}
	current, err := readRecord(filepath.Join(directory, stateName))
	if err == nil {
		if err := validateRecord(current, id); err != nil {
			return nil, err
		}
		// A restarted process can see a replacement whose directory sync failed.
		if err := syncDir(directory); err != nil {
			return nil, err
		}
		return &Store{directory: directory, current: current, syncDirectory: syncDir}, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	name, err := Normalize(initial)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create machine name directory: %w", err)
	}
	current = record{Version: 1, Claim: Claim{ID: id, MachineName: name, Revision: 1}}
	if _, err := publishRecord(directory, current, syncDir); err != nil {
		return nil, err
	}
	return &Store{directory: directory, current: current, syncDirectory: syncDir}, nil
}

func (s *Store) Current() Claim {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current.Claim
}

// Rename compares the observed revision before committing. Only the latest
// committed change can be retried with its previous revision after a lost reply.
func (s *Store) Rename(ctx context.Context, target, value string, expected uint64) (Claim, bool, error) {
	name, err := Normalize(value)
	if err != nil {
		return Claim{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Claim{}, false, fmt.Errorf("rename machine: %w", err)
	}
	if target != s.current.ID {
		return Claim{}, false, ErrTarget
	}
	// A rename can become visible before directory sync fails. Finish that commit
	// before acknowledging a retry or allowing another revision to overwrite it.
	if s.pending != nil {
		if err := s.syncDirectory(s.directory); err != nil {
			return Claim{}, false, err
		}
		s.current = *s.pending
		s.pending = nil
	}
	current := s.current
	if expected != current.Revision {
		if current.PreviousRevision != nil && expected == *current.PreviousRevision && name == current.MachineName {
			return current.Claim, false, nil
		}
		return Claim{}, false, ErrRevision
	}
	if name == current.MachineName {
		return current.Claim, false, nil
	}
	if current.Revision == math.MaxUint64 {
		return Claim{}, false, errors.New("machine name revision limit reached")
	}
	previous := current.Revision
	current.MachineName = name
	current.Revision++
	current.PreviousRevision = &previous
	published, err := publishRecord(s.directory, current, s.syncDirectory)
	if err != nil {
		if published {
			s.pending = &current
		}
		return Claim{}, false, err
	}
	s.current = current
	return current.Claim, true, nil
}

func validateRecord(current record, id string) error {
	name, err := Normalize(current.MachineName)
	if err != nil {
		return err
	}
	if current.Version != 1 || current.ID != id || name != current.MachineName || current.Revision == 0 {
		return errors.New("machine name state has an invalid version, identity, name, or revision")
	}
	if current.Revision > 1 && current.PreviousRevision == nil {
		return errors.New("machine name state is missing its committed retry revision")
	}
	if current.PreviousRevision != nil && (*current.PreviousRevision == 0 || *current.PreviousRevision != current.Revision-1) {
		return errors.New("machine name state has an invalid previous revision")
	}
	return nil
}

func readRecord(path string) (record, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // fixed daemon-owned name state; reject symlinks and special files
	if err != nil {
		return record{}, fmt.Errorf("open machine name state: %w", err)
	}
	defer file.Close() //nolint:errcheck // read-only state descriptor
	info, err := file.Stat()
	if err != nil {
		return record{}, fmt.Errorf("inspect machine name state: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return record{}, errors.New("machine name state must be a regular file with permissions 0600")
	}
	contents, err := io.ReadAll(io.LimitReader(file, maximumStateBytes+1))
	if err != nil {
		return record{}, fmt.Errorf("read machine name state: %w", err)
	}
	if len(contents) > maximumStateBytes {
		return record{}, errors.New("machine name state exceeds 4 KiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var current record
	if err := decoder.Decode(&current); err != nil {
		return record{}, fmt.Errorf("decode machine name state: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return record{}, errors.New("machine name state has trailing data")
	}
	return current, nil
}

func publishRecord(directory string, current record, syncDir func(string) error) (bool, error) {
	contents, err := json.Marshal(current)
	if err != nil {
		return false, fmt.Errorf("encode machine name state: %w", err)
	}
	file, err := os.CreateTemp(directory, ".machine-name-*")
	if err != nil {
		return false, fmt.Errorf("stage machine name state: %w", err)
	}
	defer os.Remove(file.Name()) //nolint:errcheck // cleanup after atomic publication
	defer file.Close()           //nolint:errcheck // closed before publication
	if _, err := file.Write(append(contents, '\n')); err != nil {
		return false, fmt.Errorf("write machine name state: %w", err)
	}
	if err := file.Sync(); err != nil {
		return false, fmt.Errorf("sync machine name state: %w", err)
	}
	if err := file.Close(); err != nil {
		return false, fmt.Errorf("close machine name state: %w", err)
	}
	if err := os.Rename(file.Name(), filepath.Join(directory, stateName)); err != nil {
		return false, fmt.Errorf("publish machine name state: %w", err)
	}
	if err := syncDir(directory); err != nil {
		return true, err
	}
	return true, nil
}

func syncDirectory(directory string) error {
	dir, err := os.Open(directory) //nolint:gosec // caller-owned daemon state directory
	if err != nil {
		return fmt.Errorf("open machine name directory: %w", err)
	}
	defer dir.Close() //nolint:errcheck // directory contains no buffered writes
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync machine name directory: %w", err)
	}
	return nil
}
