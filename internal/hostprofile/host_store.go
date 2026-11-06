package hostprofile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

type HostStore struct {
	dir      string
	hostID   string
	now      func() time.Time
	syncDir  func(*os.File) error
	syncFile func(*os.File) error
	write    func(*os.File, []byte) error
	rename   func(*os.File, string) error
}

func OpenHostStore(configDir, hostID string) *HostStore {
	return &HostStore{dir: configDir, hostID: hostID, now: time.Now, syncDir: syncFile, syncFile: syncFile, write: writeAll, rename: renameProfile}
}

func (s *HostStore) Read() (Snapshot[HostProfile], error) {
	dir, err := openDirectory(s.dir, false)
	if errors.Is(err, os.ErrNotExist) {
		return absentSnapshot(), nil
	}
	if err != nil {
		return unavailableSnapshot(err), err
	}
	defer func() { _ = dir.Close() }()
	return s.read(dir)
}

func absentSnapshot() Snapshot[HostProfile] {
	revision := uint64(0)
	return Snapshot[HostProfile]{State: StateAbsent, Revision: &revision}
}
func unavailableSnapshot(err error) Snapshot[HostProfile] {
	return Snapshot[HostProfile]{State: StateUnavailable, Problem: err.Error()}
}

func (s *HostStore) read(dir *os.File) (Snapshot[HostProfile], error) {
	data, err := readFile(dir)
	if errors.Is(err, os.ErrNotExist) {
		return absentSnapshot(), nil
	}
	if err != nil {
		return unavailableSnapshot(err), err
	}
	return s.snapshot(data), nil
}

func (s *HostStore) snapshot(data []byte) Snapshot[HostProfile] {
	p, err := parseHost(data)
	if err != nil {
		return Snapshot[HostProfile]{State: StateInvalid, Problem: err.Error()}
	}
	if p.HostID != s.hostID {
		return Snapshot[HostProfile]{State: StateInvalid, Revision: &p.Revision, Problem: "profile.json belongs to another Mesh identity"}
	}
	return Snapshot[HostProfile]{State: StateValid, Value: &p, Revision: &p.Revision}
}

func parseHost(data []byte) (HostProfile, error) {
	var p HostProfile
	fields := append(append([]string{}, editableFields...), "hostId", "revision", "updatedAt")
	if err := strictObject(data, HostLimits.Encoded, fields); err != nil {
		return p, err
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return p, invalid("document", err.Error())
	}
	if err := validateText("hostId", p.HostID, 256, false); err != nil {
		return p, err
	}
	if p.HostID == "" || p.Revision == 0 || p.UpdatedAt.IsZero() {
		return p, invalid("metadata", "requires hostId, positive revision and updatedAt")
	}
	normalized, err := normalizeEditable(p.Editable, HostLimits)
	if err != nil {
		return p, err
	}
	p.Editable = normalized
	return p, nil
}

func (s *HostStore) Set(ctx context.Context, expected uint64, next Editable) (HostProfile, bool, error) {
	normalized, err := normalizeEditable(next, HostLimits)
	if err != nil {
		return HostProfile{}, false, err
	}
	if s.hostID == "" {
		return HostProfile{}, false, errors.New("profile write requires an existing Mesh identity")
	}
	if err := ctx.Err(); err != nil {
		return HostProfile{}, false, fmt.Errorf("profile write cancelled: %w", err)
	}
	dir, err := openDirectory(s.dir, true)
	if err != nil {
		return HostProfile{}, false, err
	}
	defer func() { _ = dir.Close() }()
	lock, err := acquireLock(ctx, dir)
	if err != nil {
		return HostProfile{}, false, err
	}
	defer func() { _ = lock.Close() }()
	return s.setLocked(ctx, dir, expected, normalized)
}

func (s *HostStore) setLocked(ctx context.Context, dir *os.File, expected uint64, next Editable) (HostProfile, bool, error) {
	snapshot, err := s.read(dir)
	if err != nil || snapshot.Revision == nil {
		return HostProfile{}, false, s.repairError(err)
	}
	revision := *snapshot.Revision
	if err := ctx.Err(); err != nil {
		return HostProfile{}, false, fmt.Errorf("profile write cancelled: %w", err)
	}
	if snapshot.Value != nil && expected < math.MaxUint64 && expected+1 == revision && sameEditable(snapshot.Value.Editable, next) {
		if err := s.settle(dir); err != nil {
			return *snapshot.Value, false, err
		}
		return *snapshot.Value, false, nil
	}
	if expected != revision {
		return HostProfile{}, false, &ConflictError{Expected: expected, Current: revision}
	}
	if revision == math.MaxUint64 {
		return HostProfile{}, false, errors.New("profile revision is exhausted; profile.json requires backup and repair")
	}
	p := HostProfile{Editable: next, HostID: s.hostID, Revision: revision + 1, UpdatedAt: s.now().UTC()}
	data, err := json.Marshal(p)
	if err != nil {
		return HostProfile{}, false, fmt.Errorf("encode profile: %w", err)
	}
	if len(data) > HostLimits.Encoded {
		return HostProfile{}, false, invalid("document", fmt.Sprintf("stored encoded size exceeds %d bytes", HostLimits.Encoded))
	}
	if err := s.publish(dir, data); err != nil {
		return p, false, err
	}
	return p, true, nil
}

func (s *HostStore) repairError(cause error) error {
	if cause == nil {
		cause = errors.New("structurally invalid profile or revision")
	}
	return fmt.Errorf("read %s safely: %w; back up and repair this file before writing", filepath.Join(s.dir, "profile.json"), cause)
}

func sameEditable(a, b Editable) bool {
	first, _ := json.Marshal(a)
	second, _ := json.Marshal(b)
	return bytes.Equal(first, second)
}

func (s *HostStore) settle(dir *os.File) error {
	file, err := openPrivateFile(dir, "profile.json", unix.O_RDONLY)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	if err := s.syncFile(file); err != nil {
		return fmt.Errorf("sync visible profile: %w", err)
	}
	if err := s.syncDir(dir); err != nil {
		return fmt.Errorf("profile saved but not confirmed on disk: %w; run the same command again", err)
	}
	return nil
}

func (s *HostStore) publish(dir *os.File, data []byte) error {
	file, name, err := temporaryFile(dir)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Unlinkat(int(dir.Fd()), name, 0) }()
	defer func() { _ = file.Close() }()
	if err := s.write(file, data); err != nil {
		return fmt.Errorf("write temporary profile: %w", err)
	}
	if err := s.syncFile(file); err != nil {
		return fmt.Errorf("sync temporary profile: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close temporary profile: %w", err)
	}
	if err := s.rename(dir, name); err != nil {
		return fmt.Errorf("rename profile: %w", err)
	}
	if err := s.syncDir(dir); err != nil {
		return fmt.Errorf("profile saved but not confirmed on disk: %w; run the same command again", err)
	}
	return nil
}
