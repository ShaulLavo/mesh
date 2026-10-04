package update

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/updateinstall"
)

type State string

const (
	Pending   State = "pending"
	Offline   State = "pending offline"
	Staged    State = "staged"
	Granted   State = "authorized"
	Updated   State = "updated"
	Newer     State = "already newer"
	Failed    State = "failed"
	Bootstrap State = "bootstrap required"
	Cancelled State = "cancelled"
)

type Target struct {
	RetryPending           bool                   `json:"retryPending,omitempty"`
	RetryToken             uint64                 `json:"retryToken,omitempty"`
	BootstrapRetryToken    uint64                 `json:"bootstrapRetryToken,omitempty"`
	BootstrapAttempt       uint64                 `json:"bootstrapAttempt,omitempty"`
	BootstrapRetry         bool                   `json:"bootstrapRetry,omitempty"`
	InstallationID         string                 `json:"installationId,omitempty"`
	BootstrapStatusSession string                 `json:"bootstrapStatusSession,omitempty"`
	BootstrapStatusAttempt uint64                 `json:"bootstrapStatusAttempt,omitempty"`
	BootstrapSession       string                 `json:"bootstrapSession,omitempty"`
	Host                   Host                   `json:"host"`
	State                  State                  `json:"state"`
	Generation             uint64                 `json:"generation,omitempty"`
	Build                  *release.Build         `json:"build,omitempty"`
	Workers                []updateinstall.Worker `json:"workers,omitempty"`
	InterruptedWorkers     []updateinstall.Worker `json:"interruptedWorkers,omitempty"`
	Problem                string                 `json:"problem,omitempty"`
	Grant                  bool                   `json:"grant,omitempty"`
	RetryAt                time.Time              `json:"retryAt,omitempty"`
}

type Run struct {
	archive              *operationArchive
	CoordinatorSetup     bool             `json:"coordinatorSetup,omitempty"`
	SetupExecutable      string           `json:"setupExecutable,omitempty"`
	SetupBuild           *release.Build   `json:"setupBuild,omitempty"`
	SetupCacheDir        string           `json:"setupCacheDir,omitempty"`
	SetupRequiredMount   string           `json:"setupRequiredMount,omitempty"`
	CoordinatorBootstrap bool             `json:"coordinatorBootstrap,omitempty"`
	ID                   string           `json:"id"`
	Coordinator          string           `json:"coordinator"`
	Fleet                Fleet            `json:"fleet"`
	Membership           string           `json:"membership"`
	Release              release.Manifest `json:"release"`
	ReleaseDigest        string           `json:"releaseDigest"`
	Targets              []Target         `json:"targets"`
	CreatedAt            time.Time        `json:"createdAt"`
	UpdatedAt            time.Time        `json:"updatedAt"`
	Stopped              bool             `json:"stopped"`
	Cancel               bool             `json:"cancel"`
	Cached               bool             `json:"cached"`
	Problem              string           `json:"problem,omitempty"`
}

func (r Run) Done() bool {
	for _, target := range r.Targets {
		if target.State != Updated && target.State != Newer && target.State != Cancelled {
			return false
		}
	}
	return true
}

// Unfinished returns the runs that still need attention. A run stays
// unfinished only while a machine it left behind has not been verified by a
// later run; otherwise one failure from weeks ago would be reported forever
// after the fleet has moved on.
func Unfinished(runs []Run) []Run {
	var pending []Run
	for _, run := range runs {
		if run.Done() {
			continue
		}
		for _, target := range run.Targets {
			if target.State == Updated || target.State == Newer || target.State == Cancelled {
				continue
			}
			if !verifiedAfter(runs, target.Host.ID, run.CreatedAt) {
				pending = append(pending, run)
				break
			}
		}
	}
	return pending
}

func verifiedAfter(runs []Run, hostID string, after time.Time) bool {
	for _, later := range runs {
		if !later.CreatedAt.After(after) {
			continue
		}
		for _, target := range later.Targets {
			if target.Host.ID == hostID && (target.State == Updated || target.State == Newer) {
				return true
			}
		}
	}
	return false
}

func (r Run) ExitCode() int {
	for _, target := range r.Targets {
		if target.State == Failed || target.State == Bootstrap {
			return 1
		}
	}
	if r.Problem != "" {
		return 1
	}
	if !r.Done() {
		return 2
	}
	return 0
}

type Store struct{ directory string }

func OpenStore(stateDir string) (*Store, error) {
	if !filepath.IsAbs(stateDir) {
		return nil, errors.New("update: state directory must be absolute")
	}
	dir := filepath.Join(stateDir, "updates", "runs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{directory: dir}, nil
}

func (s *Store) Start(coordinator string, fleet Fleet, manifest release.Manifest) (Run, error) {
	if err := fleet.Validate(); err != nil {
		return Run{}, err
	}
	if err := manifest.Validate(); err != nil {
		return Run{}, err
	}
	ordered, err := fleet.Order(coordinator)
	if err != nil {
		return Run{}, err
	}
	unlock, err := s.lock()
	if err != nil {
		return Run{}, err
	}
	defer unlock()
	runs, err := s.List()
	if err != nil {
		return Run{}, err
	}
	for _, existing := range runs {
		if existing.Done() || existing.Cancel || existing.Membership != fleet.Digest() || existing.ReleaseDigest != manifest.Digest() {
			continue
		}
		if !existing.Stopped {
			return existing, nil
		}
		if err := retryRun(&existing); err != nil {
			return Run{}, err
		}
		existing.UpdatedAt = time.Now().UTC()
		return existing, writeOperation(s.path(existing.ID), existing)
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return Run{}, err
	}
	now := time.Now().UTC()
	run := Run{ID: hex.EncodeToString(raw[:]), Coordinator: coordinator, Fleet: fleet, Membership: fleet.Digest(),
		Release: manifest, ReleaseDigest: manifest.Digest(), CreatedAt: now, UpdatedAt: now}
	for _, h := range ordered {
		run.Targets = append(run.Targets, Target{Host: h, State: Pending})
	}
	return run, writeJSON(s.path(run.ID), run)
}

func ValidRunID(id string) bool {
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == 16 && hex.EncodeToString(decoded) == id
}

func (s *Store) Read(id string) (Run, error) {
	if !ValidRunID(id) {
		return Run{}, errors.New("update: invalid run ID")
	}
	var record operationRecord
	if err := readJSON(s.path(id), &record); err != nil {
		return Run{}, err
	}
	run := record.Run
	if run.ID != id || run.Release.Digest() != run.ReleaseDigest || run.approvedMembership() != run.Membership {
		return Run{}, errors.New("update: saved operation identity or digest does not match")
	}
	if err := validateRunMembership(run); err != nil {
		return Run{}, err
	}
	return run, nil
}

func validateRunMembership(run Run) error {
	if err := run.Fleet.Validate(); err != nil {
		return err
	}
	if err := run.Release.Validate(); err != nil {
		return err
	}
	if len(run.Targets) != len(run.Fleet.Members) {
		return errors.New("update: saved target count differs from approved membership")
	}
	members := make(map[string]Host, len(run.Fleet.Members))
	for _, host := range run.Fleet.Members {
		members[host.ID] = host
	}
	for _, target := range run.Targets {
		host, exists := members[target.Host.ID]
		if !exists || host.Endpoint != target.Host.Endpoint || host.Platform != target.Host.Platform || !slices.Equal(host.DependsOn, target.Host.DependsOn) {
			return errors.New("update: target differs from approved fleet membership")
		}
		delete(members, target.Host.ID)
	}
	return nil
}

func (s *Store) List() ([]Run, error) {
	entries, err := os.ReadDir(s.directory)
	if err != nil {
		return nil, err
	}
	var runs []Run
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		run, err := s.Read(strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].CreatedAt.After(runs[j].CreatedAt) })
	return runs, nil
}

func (s *Store) Change(id string, apply func(*Run) error) (Run, error) {
	unlock, err := s.lock()
	if err != nil {
		return Run{}, err
	}
	defer unlock()
	run, err := s.Read(id)
	if err != nil {
		return Run{}, err
	}
	approval := run.approvalIdentity()
	if err := apply(&run); err != nil {
		return Run{}, err
	}
	if approval != run.approvalIdentity() {
		return Run{}, errors.New("update: progress changed original operation approval")
	}
	if err := validateRunMembership(run); err != nil {
		return Run{}, err
	}
	run.UpdatedAt = time.Now().UTC()
	return run, writeOperation(s.path(id), run)
}

func (s *Store) Cancel(id string) (Run, error) {
	return s.Change(id, func(run *Run) error {
		run.Cancel, run.Stopped = true, true
		for index := range run.Targets {
			run.Targets[index].RetryPending = false
		}
		return nil
	})
}

func (s *Store) Retry(id string) (Run, error) {
	return s.Change(id, retryRun)
}

func retryRun(run *Run) error {
	if run.Cancel {
		return errors.New("update: cancelled operations require a new approval")
	}
	run.Stopped, run.Problem = false, ""
	for i := range run.Targets {
		target := &run.Targets[i]
		if target.State == Failed && target.InstallationID == "" && target.BootstrapSession == "" && !target.RetryPending {
			target.RetryPending = true
			target.RetryToken++
		}
		if target.State == Failed && target.BootstrapSession != "" {
			target.BootstrapRetry = true
		}
		if target.State == Failed || target.State == Bootstrap || target.State == Offline {
			target.State, target.Problem, target.RetryAt = Pending, "", time.Time{}
		}
	}
	return nil
}

func (s *Store) path(id string) string { return filepath.Join(s.directory, id+".json") }

func (s *Store) lock() (func(), error) {
	file, err := os.OpenFile(filepath.Join(s.directory, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN); _ = file.Close() }, nil
}

func readJSON(path string, target any) error {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // policy snapshots must reject symlinks and special files without blocking authentication
	if err != nil {
		return fmt.Errorf("update: open state snapshot: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("update: inspect state snapshot: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 || info.Mode().Perm()&0o444 == 0 {
		return errors.New("update: state snapshot requires a readable regular file with safe permissions")
	}
	if info.Size() > 2<<20 {
		return errors.New("update: state snapshot exceeds the size limit")
	}
	data, err := io.ReadAll(io.LimitReader(file, (2<<20)+1))
	if err != nil || len(data) > 2<<20 {
		return errors.New("update: state exceeds the size limit or cannot be read")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("update: decode %s: %w", path, err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("update: trailing JSON data")
	}
	return nil
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".update-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if _, err := file.Write(append(data, '\n')); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}
