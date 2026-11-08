package access

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

type Store struct {
	stateDir, target string
	now              func() time.Time
	monoNow          func() time.Time
	mu               sync.Mutex
	cached           *PolicyView
	stamp            unix.Stat_t
	syncFile         func(*os.File) error
	syncDir          func(*os.File) error
	rename           func(int, string, int, string) error
}

func Open(stateDir, target string, now func() time.Time) (*Store, error) {
	if stateDir == "" {
		return nil, errors.New("access: state directory required")
	}
	if err := ValidateSubject(Subject(target)); err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	return &Store{stateDir: stateDir, target: target, now: now, monoNow: time.Now, syncFile: (*os.File).Sync, syncDir: (*os.File).Sync, rename: unix.Renameat}, nil
}

// Registered includes expired and other-target registrations for writer exclusion.
// It never creates files and never converts an unsafe observation into absence.
func Registered(stateDir string, subject Subject) (bool, error) {
	policy, err := (&Store{stateDir: stateDir}).snapshot()
	if err != nil {
		return false, err
	}
	_, ok := findEntry(policy, subject)
	return ok, nil
}
func (s *Store) Membership(subject Subject) (bool, error) {
	policy, err := s.snapshot()
	if err != nil {
		return false, err
	}
	_, ok := findEntry(policy, subject)
	return ok, nil
}
func (s *Store) List(ctx context.Context) (PolicyView, error) {
	if err := ctx.Err(); err != nil {
		return PolicyView{}, fmt.Errorf("access: list cancelled: %w", err)
	}
	policy, err := s.snapshot()
	if err != nil {
		return PolicyView{}, err
	}
	result := *policy
	result.Entries = make([]Entry, len(policy.Entries))
	for i, entry := range policy.Entries {
		result.Entries[i] = cloneEntry(entry)
	}
	return result, nil
}
func cloneEntry(entry Entry) Entry {
	entry.Permissions = append([]string{}, entry.Permissions...)
	return entry
}
func findEntry(policy *PolicyView, subject Subject) (Entry, bool) {
	for _, entry := range policy.Entries {
		if entry.Subject == subject {
			return entry, true
		}
	}
	return Entry{}, false
}
func (s *Store) Bind(subject Subject) (Binding, error) {
	if err := ValidateSubject(subject); err != nil {
		return Binding{}, err
	}
	policy, err := s.snapshot()
	if err != nil {
		return Binding{}, err
	}
	entry, ok := findEntry(policy, subject)
	if !ok || !s.now().Before(entry.ExpiresAt) {
		return Binding{}, ErrDenied
	}
	capAt := s.monoNow().Add(entry.ExpiresAt.Sub(s.now()))
	principal := Principal{target: s.target, entry: cloneEntry(entry)}
	current := func() bool {
		if !s.monoNow().Before(capAt) || !s.now().Before(entry.ExpiresAt) {
			return false
		}
		policy, err := s.snapshot()
		if err != nil {
			return false
		}
		next, ok := findEntry(policy, subject)
		return ok && next.Incarnation == entry.Incarnation && next.ExpiresAt.Equal(entry.ExpiresAt) && slices.Equal(next.Permissions, entry.Permissions)
	}
	return Binding{state: &bindingState{principal: principal, current: current}}, nil
}
func (s *Store) snapshot() (*PolicyView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	policy, err := s.readSnapshot()
	if err != nil {
		s.cached = nil
	}
	return policy, err
}
func (s *Store) readSnapshot() (*PolicyView, error) {
	dir, err := scopeDirectory(s.stateDir, false)
	if errors.Is(err, os.ErrNotExist) {
		s.cached = nil
		return s.empty(), nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()
	return s.readDirectory(dir)
}
func (s *Store) readDirectory(dir *os.File) (*PolicyView, error) {
	file, stamp, err := privateFile(dir, "scopes.json", unix.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		s.cached = nil
		return s.empty(), nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	if stamp.Size > MaximumPolicy {
		return nil, errors.New("access: policy exceeds size limit")
	}
	stamp.Atim = unix.Timespec{}
	if s.cached != nil && stamp == s.stamp {
		return s.cached, nil
	}
	data, err := io.ReadAll(io.LimitReader(file, MaximumPolicy+1))
	if err != nil {
		return nil, fmt.Errorf("access: read policy: %w", err)
	}
	var after unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &after); err != nil {
		return nil, fmt.Errorf("access: inspect policy after read: %w", err)
	}
	after.Atim = unix.Timespec{}
	if after != stamp {
		return nil, errors.New("access: policy changed during read")
	}
	policy, err := parsePolicy(data)
	if err != nil {
		return nil, err
	}
	if s.target != "" && policy.Target != s.target {
		return nil, errors.New("access: policy target identity differs")
	}
	s.cached, s.stamp = &policy, stamp
	return s.cached, nil
}
func (s *Store) snapshotAt(dir *os.File) (*PolicyView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	policy, err := s.readDirectory(dir)
	if err != nil {
		s.cached = nil
	}
	return policy, err
}
func (s *Store) empty() *PolicyView {
	return &PolicyView{Version: 1, Target: s.target, Entries: []Entry{}}
}
func parsePolicy(data []byte) (PolicyView, error) {
	var policy PolicyView
	if err := strictJSON(data, &policy); err != nil {
		return policy, err
	}
	if policy.Version != 1 || policy.Revision == 0 || len(policy.Entries) > 256 || policy.Entries == nil {
		return policy, errors.New("access: invalid policy version, revision or entries")
	}
	if err := ValidateSubject(Subject(policy.Target)); err != nil {
		return policy, err
	}
	seen := make(map[Subject]bool, len(policy.Entries))
	for _, entry := range policy.Entries {
		if err := validateEntry(entry); err != nil {
			return policy, err
		}
		if seen[entry.Subject] || string(entry.Subject) == policy.Target {
			return policy, errors.New("access: duplicate or self subject")
		}
		seen[entry.Subject] = true
	}
	return policy, nil
}
func validateEntry(entry Entry) error {
	if err := ValidateSubject(entry.Subject); err != nil {
		return err
	}
	incarnation, err := hex.DecodeString(entry.Incarnation)
	if err != nil || len(incarnation) != 16 || hex.EncodeToString(incarnation) != entry.Incarnation {
		return errors.New("access: invalid grant incarnation")
	}
	if !ValidPermissions(entry.Permissions) {
		return errors.New("access: invalid scoped permission set")
	}
	if entry.CreatedAt.IsZero() || !entry.ExpiresAt.After(entry.CreatedAt) || entry.ExpiresAt.Sub(entry.CreatedAt) > 7*24*time.Hour {
		return errors.New("access: invalid grant times")
	}
	return nil
}
func (s *Store) Grant(ctx context.Context, expected uint64, input GrantInput, observe ObserveOtherRoles) (Change, error) {
	if err := ValidateSubject(input.Subject); err != nil {
		return Change{}, err
	}
	if !ValidPermissions(input.Permissions) {
		return Change{}, errors.New("access: invalid scoped permission set")
	}
	if string(input.Subject) == s.target || observe == nil {
		return Change{}, errors.New("access: dedicated subject and role observer required")
	}
	return s.change(ctx, expected, input.Subject, func(ctx context.Context, policy *PolicyView) (*Entry, error) {
		roles, err := observe(ctx, input.Subject)
		if err != nil {
			return nil, fmt.Errorf("access: observe other key classes: %w", err)
		}
		if roles.FullDevice || roles.UpdateAdministrator {
			return nil, errors.New("access: identity already has a full or update role")
		}
		return s.nextGrant(policy, input)
	})
}
func (s *Store) nextGrant(policy *PolicyView, input GrantInput) (*Entry, error) {
	if existing, ok := findEntry(policy, input.Subject); ok && existing.ExpiresAt.Equal(input.ExpiresAt) && slices.Equal(existing.Permissions, input.Permissions) {
		entry := cloneEntry(existing)
		return &entry, nil
	}
	now := s.now().UTC()
	if !input.ExpiresAt.After(now) || input.ExpiresAt.After(now.Add(7*24*time.Hour)) {
		return nil, errors.New("access: expiry must be in the next seven days")
	}
	incarnation, err := randomName()
	if err != nil {
		return nil, err
	}
	return &Entry{Subject: input.Subject, Incarnation: incarnation, Permissions: append([]string{}, input.Permissions...), CreatedAt: now, ExpiresAt: input.ExpiresAt.UTC()}, nil
}
func (s *Store) Revoke(ctx context.Context, expected uint64, subject Subject) (Change, error) {
	if err := ValidateSubject(subject); err != nil {
		return Change{}, err
	}
	return s.change(ctx, expected, subject, func(context.Context, *PolicyView) (*Entry, error) { return nil, nil })
}
func (s *Store) change(ctx context.Context, expected uint64, subject Subject, replacement func(context.Context, *PolicyView) (*Entry, error)) (Change, error) {
	var result Change
	err := WithClassWriter(ctx, s.stateDir, 1500*time.Millisecond, func(wait context.Context) error {
		dir, err := scopeDirectory(s.stateDir, true)
		if err != nil {
			return err
		}
		defer func() { _ = dir.Close() }()
		lock, err := writerLock(wait, dir, "scopes.lock")
		if err != nil {
			return err
		}
		defer func() { _ = lock.Close() }()
		result, err = s.changeLocked(wait, dir, expected, subject, replacement)
		return err
	})
	return result, err
}
func (s *Store) changeLocked(ctx context.Context, dir *os.File, expected uint64, subject Subject, replacement func(context.Context, *PolicyView) (*Entry, error)) (Change, error) {
	current, err := s.snapshotAt(dir)
	if err != nil {
		return Change{}, err
	}
	next, err := replacement(ctx, current)
	if err != nil {
		return Change{}, err
	}
	old, exists := findEntry(current, subject)
	identical := (!exists && next == nil) || (exists && next != nil && old.Incarnation == next.Incarnation)
	result := Change{Revision: current.Revision, Entry: next}
	if expected != current.Revision && !(identical && expected < math.MaxUint64 && expected+1 == current.Revision) {
		return result, ErrRevision
	}
	if err := ctx.Err(); err != nil {
		return result, fmt.Errorf("access: mutation cancelled: %w", err)
	}
	if identical {
		return result, s.settle(dir, current.Revision)
	}
	return s.publishChange(dir, current, subject, next)
}
func (s *Store) publishChange(dir *os.File, current *PolicyView, subject Subject, next *Entry) (Change, error) {
	result := Change{Revision: current.Revision, Entry: next}
	if current.Revision == math.MaxUint64 {
		return result, errors.New("access: policy revision exhausted")
	}
	policy := PolicyView{Version: 1, Target: s.target, Revision: current.Revision + 1, Entries: make([]Entry, 0, len(current.Entries)+1)}
	for _, entry := range current.Entries {
		if entry.Subject != subject {
			policy.Entries = append(policy.Entries, entry)
		}
	}
	if next != nil {
		policy.Entries = append(policy.Entries, *next)
	}
	if len(policy.Entries) > 256 {
		return result, errors.New("access: too many scoped identities")
	}
	slices.SortFunc(policy.Entries, func(a, b Entry) int { return strings.Compare(string(a.Subject), string(b.Subject)) })
	result.Revision = policy.Revision
	if err := s.publish(dir, policy); err != nil {
		return result, err
	}
	result.Changed = true
	return result, nil
}

type VisibleUnconfirmed struct {
	Revision uint64
	Cause    error
}

func (e *VisibleUnconfirmed) Error() string {
	return fmt.Sprintf("access: revision %d is visible but not confirmed on disk: %v; retry the same command", e.Revision, e.Cause)
}
func (e *VisibleUnconfirmed) Unwrap() error { return e.Cause }
func (s *Store) settle(dir *os.File, revision uint64) error {
	if revision == 0 {
		return nil
	}
	file, _, err := privateFile(dir, "scopes.json", unix.O_RDONLY)
	if err != nil {
		return err
	}
	syncErr := s.syncFile(file)
	closeErr := file.Close()
	if err := errors.Join(syncErr, closeErr, s.syncDir(dir), syncScopeParent(dir, s.syncDir)); err != nil {
		return &VisibleUnconfirmed{Revision: revision, Cause: err}
	}
	return nil
}
func (s *Store) publish(dir *os.File, policy PolicyView) error {
	data, err := json.Marshal(policy)
	if err != nil {
		return fmt.Errorf("access: encode policy: %w", err)
	}
	if len(data) > MaximumPolicy {
		return errors.New("access: encoded policy exceeds size limit")
	}
	random, err := randomName()
	if err != nil {
		return err
	}
	name := ".scopes-" + random
	file, _, err := privateFile(dir, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Unlinkat(int(dir.Fd()), name, 0); _ = file.Close() }()
	n, err := file.Write(data)
	if err != nil {
		return fmt.Errorf("access: write temporary policy: %w", err)
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	if err := s.syncFile(file); err != nil {
		return fmt.Errorf("access: sync temporary policy: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("access: close temporary policy: %w", err)
	}
	if err := s.rename(int(dir.Fd()), name, int(dir.Fd()), "scopes.json"); err != nil {
		return fmt.Errorf("access: publish policy: %w", err)
	}
	if err := s.syncDir(dir); err != nil {
		return &VisibleUnconfirmed{Revision: policy.Revision, Cause: err}
	}
	if err := syncScopeParent(dir, s.syncDir); err != nil {
		return &VisibleUnconfirmed{Revision: policy.Revision, Cause: err}
	}
	return nil
}
