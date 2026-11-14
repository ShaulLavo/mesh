package update

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
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
	ConsentRevision        uint64                      `json:"consentRevision,omitempty"`
	StageAttempt           uint64                      `json:"stageAttempt,omitempty"`
	RetryAttempt           uint64                      `json:"retryAttempt,omitempty"`
	StageUnsettled         bool                        `json:"stageUnsettled,omitempty"`
	RetryUnsettled         bool                        `json:"retryUnsettled,omitempty"`
	GrantAccepted          bool                        `json:"grantAccepted,omitempty"`
	JoinedUnsettled        bool                        `json:"joinedUnsettled,omitempty"`
	Commands               []updateinstall.CommandCopy `json:"commands,omitempty"`
	RetryPending           bool                        `json:"retryPending,omitempty"`
	RetryToken             uint64                      `json:"retryToken,omitempty"`
	BootstrapRetryToken    uint64                      `json:"bootstrapRetryToken,omitempty"`
	BootstrapAttempt       uint64                      `json:"bootstrapAttempt,omitempty"`
	BootstrapRetry         bool                        `json:"bootstrapRetry,omitempty"`
	InstallationID         string                      `json:"installationId,omitempty"`
	BootstrapStatusSession string                      `json:"bootstrapStatusSession,omitempty"`
	BootstrapStatusAttempt uint64                      `json:"bootstrapStatusAttempt,omitempty"`
	BootstrapSession       string                      `json:"bootstrapSession,omitempty"`
	Host                   Host                        `json:"host"`
	State                  State                       `json:"state"`
	Generation             uint64                      `json:"generation,omitempty"`
	Build                  *release.Build              `json:"build,omitempty"`
	Workers                []updateinstall.Worker      `json:"workers,omitempty"`
	InterruptedWorkers     []updateinstall.Worker      `json:"interruptedWorkers,omitempty"`
	Problem                string                      `json:"problem,omitempty"`
	Grant                  bool                        `json:"grant,omitempty"`
	RetryAt                time.Time                   `json:"retryAt,omitempty"`
}

type Run struct {
	Origin               *AutomaticOrigin `json:"origin,omitempty"`
	archive              *operationArchive
	statusOnly           bool
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
		if run.statusOnly || run.Done() {
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

type Store struct {
	directory        string
	hostInstallation string
}

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
	unlock, err := s.lock()
	if err != nil {
		return Run{}, err
	}
	defer unlock()
	runs, err := s.List()
	if err != nil {
		return Run{}, err
	}
	if existing, found, err := s.existingApproval(runs, fleet, manifest); found || err != nil {
		return existing, err
	}

	run, err := newRun(coordinator, fleet, manifest)
	if err != nil {
		return Run{}, err
	}
	return run, s.write(run)
}
func newRun(coordinator string, fleet Fleet, manifest release.Manifest) (Run, error) {
	ordered, err := fleet.Order(coordinator)
	if err != nil {
		return Run{}, err
	}
	id, err := newAutomaticID()
	if err != nil {
		return Run{}, err
	}
	now := time.Now().UTC()
	run := Run{ID: id, Coordinator: coordinator, Fleet: fleet, Membership: fleet.Digest(), Release: manifest, ReleaseDigest: manifest.Digest(), CreatedAt: now, UpdatedAt: now}
	for _, host := range ordered {
		run.Targets = append(run.Targets, Target{Host: host, State: Pending})
	}
	return run, nil
}

func ValidRunID(id string) bool {
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == 16 && hex.EncodeToString(decoded) == id
}

func (s *Store) Read(id string) (Run, error) {
	automatic, err := s.readAutomatic()
	if err != nil {
		return Run{}, err
	}
	if automatic != nil && automatic.Current != nil && automatic.Current.ID == id {
		return *automatic.Current, nil
	}
	run, err := s.read(id)
	if err != nil {
		return Run{}, err
	}
	return run, s.settlePublication(s.directory)
}

func (s *Store) read(id string) (Run, error) {
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
	run.statusOnly = run.Origin != nil
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
	automatic, err := s.readAutomatic()
	if err != nil {
		return nil, err
	}
	if automatic != nil && automatic.Current != nil {
		runs = append(runs, *automatic.Current)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		run, err := s.read(strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		if automatic != nil && automatic.Current != nil && run.ID == automatic.Current.ID {
			continue
		}
		runs = append(runs, run)
	}
	if len(runs) == 0 {
		return runs, nil
	}
	if err := s.settlePublication(s.directory); err != nil {
		return nil, err
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
	if run.Origin != nil {
		return s.changeAutomaticRun(id, func(r *Run, _ *AutomaticRecord) error { return apply(r) })
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
	return run, s.write(run)
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
	return s.Change(id, func(r *Run) error {
		if r.Origin != nil {
			return problem("automatic.stopped", "automatic retry requires policy-owner approval and --expected-policy-revision")
		}
		return retryRun(r)
	})
}

func retryRun(run *Run) error {
	if run.Cancel {
		return errors.New("update: cancelled operations require a new approval")
	}
	run.Stopped, run.Problem = false, ""
	for i := range run.Targets {
		target := &run.Targets[i]
		if target.State == Failed && target.InstallationID == "" && target.BootstrapSession == "" && !target.RetryPending {
			if target.RetryToken == math.MaxUint64 {
				return errors.New("update: target retry token exhausted")
			}
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

func (s *Store) write(run Run) error {
	if err := writeOperation(s.path(run.ID), run); err != nil {
		return err
	}
	return s.settlePublication(filepath.Dir(s.directory))
}

// A visible rename can outlive a failed barrier. Readers settle it before the
// coordinator or an identical approval can use the run. Include the directories
// that OpenStore may have created for the first operation.
func (s *Store) settlePublication(directory string) error {
	stateDir := filepath.Dir(filepath.Dir(s.directory))
	for {
		if err := syncOperationDirectory(directory); err != nil {
			return fmt.Errorf("update: settle operation publication: %w", err)
		}
		if directory == stateDir {
			return settleOperationParents(stateDir)
		}
		directory = filepath.Dir(directory)
	}
}

// The filesystem root is a pre-existing anchor even after a failed creation's
// in-memory history is lost. Resolve aliases before walking its physical parents.
func settleOperationParents(directory string) error {
	current, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return fmt.Errorf("update: resolve publication directory: %w", err)
	}
	for {
		parent, err := operationPublicationParent(current)
		if err != nil || parent == "" {
			return err
		}
		if err := syncOperationDirectory(parent); err != nil {
			return fmt.Errorf("update: settle operation parent: %w", err)
		}
		current = parent
	}
}

func operationPublicationParent(directory string) (string, error) {
	parent := filepath.Dir(directory)
	if parent == directory {
		return "", nil
	}
	var current, ancestor syscall.Stat_t
	if err := syscall.Stat(directory, &current); err != nil {
		return "", fmt.Errorf("update: inspect publication directory: %w", err)
	}
	if err := syscall.Stat(parent, &ancestor); err != nil {
		return "", fmt.Errorf("update: inspect publication parent: %w", err)
	}
	if current.Dev != ancestor.Dev {
		return "", nil
	}
	return parent, nil
}

func (s *Store) path(id string) string { return filepath.Join(s.directory, id+".json") }

func (s *Store) lock() (func(), error) {
	file, err := s.openAuthorityLock()
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(1500 * time.Millisecond)
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return releaseAuthority(file), nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(deadline) {
			_ = file.Close()
			return nil, fmt.Errorf("update: acquire writer lock: %w", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
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
		return fmt.Errorf("update: sync state snapshot: %w", err)
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	return syncOperationDirectory(filepath.Dir(path))
}

var syncOperationDirectory = func(path string) error {
	dir, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("update: open publication directory: %w", err)
	}
	defer func() { _ = syscall.Close(dir) }()
	if err := syscall.Fsync(dir); err != nil {
		return fmt.Errorf("update: sync publication directory: %w", err)
	}
	return nil
}

func (s *Store) existingApproval(runs []Run, fleet Fleet, manifest release.Manifest) (Run, bool, error) {
	for _, existing := range runs {
		found, err := automaticApproval(existing, fleet, manifest)
		if found || err != nil {
			return existing, found, err
		}
		if existing.Origin != nil || existing.Done() || existing.Cancel || existing.Membership != fleet.Digest() || existing.ReleaseDigest != manifest.Digest() {
			continue
		}
		if !existing.Stopped {
			return existing, true, nil
		}
		if err := retryRun(&existing); err != nil {
			return Run{}, false, err
		}
		existing.UpdatedAt = time.Now().UTC()
		return existing, true, s.write(existing)
	}
	return Run{}, false, nil
}
func automaticApproval(existing Run, fleet Fleet, manifest release.Manifest) (bool, error) {
	if existing.Origin == nil {
		return false, nil
	}
	matching := existing.Membership == fleet.Digest() && existing.ReleaseDigest == manifest.Digest()
	if existing.Stopped && matching {
		return true, nil
	}
	if automaticSettled(existing) || !fleetsOverlap(existing.Fleet, fleet) {
		return false, nil
	}
	if matching {
		return true, nil
	}
	return false, problem("automatic.conflict", "overlapping automatic run "+existing.ID+"; explicitly retry or cancel it")
}
