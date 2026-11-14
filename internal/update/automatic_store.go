package update

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/shaul/mesh/internal/hostprofile"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/updatenotice"
	"golang.org/x/sys/unix"
)

func (s *Store) ReadAutomatic(ctx context.Context) (AutomaticStatus, error) {
	unlock, err := s.lockContext(ctx)
	if err != nil {
		return AutomaticStatus{}, err
	}
	defer unlock()
	record, err := s.readAutomatic()
	if err != nil {
		return AutomaticStatus{State: "state unavailable", Problem: err.Error()}, err
	}
	return automaticStatus(record), nil
}
func (s *Store) readAutomatic() (*AutomaticRecord, error) {
	var record AutomaticRecord
	err := s.readAuthority("automatic.json", &record)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, unavailableAutomaticState(err)
	}
	if err = validateAutomatic(record); err != nil {
		return nil, unavailableAutomaticState(err)
	}
	return &record, nil
}
func validateAutomatic(r AutomaticRecord) error {
	if err := validateAutomaticPolicy(r); err != nil {
		return err
	}
	if err := validateContinuation(r); err != nil {
		return err
	}
	if r.Current == nil {
		return nil
	}
	if err := validateAutomaticOrigin(r); err != nil {
		return err
	}
	if err := validateAutomaticProgress(r); err != nil {
		return err
	}
	return validateRunMembership(*r.Current)
}
func validateAutomaticPolicy(r AutomaticRecord) error {
	p := r.Policy
	if r.Schema != 1 || !ValidRunID(p.ID) || p.Revision == 0 || p.Epoch == 0 || p.Membership != p.Fleet.Digest() {
		return errors.New("update: invalid automatic authority identity")
	}
	if _, err := PublicKey(p.OwnerID); err != nil {
		return err
	}
	if _, err := PublicKey(p.CoordinatorID); err != nil {
		return err
	}
	return p.Fleet.Validate()
}
func validateAutomaticOrigin(r AutomaticRecord) error {
	p, run := r.Policy, r.Current
	if !ValidRunID(run.ID) || run.Origin == nil || run.Origin.PolicyID != p.ID || run.Origin.Epoch != p.Epoch || run.Origin.Membership != p.Membership || run.Membership != p.Membership || run.Fleet.Digest() != p.Membership || run.Coordinator != p.CoordinatorID || run.Release.Digest() != run.ReleaseDigest || r.Attempt == 0 {
		return errors.New("update: automatic run differs from its authority")
	}
	return nil
}
func validateAutomaticProgress(r AutomaticRecord) error {
	run := r.Current
	if r.Stop != nil && (!run.Stopped && r.Stop.Kind != stopRecovering || r.Stop.RunID != run.ID || r.Stop.Attempt != r.Attempt) {
		return errors.New("update: invalid automatic stop")
	}
	if r.Canary != nil && (r.Canary.RunID != run.ID || r.Canary.Attempt != r.Attempt || !slices.Contains([]string{"selected", "issued", canaryVerified, "failed"}, r.Canary.State)) {
		return errors.New("update: invalid automatic canary")
	}
	if r.Continuation != nil && !slices.Contains([]string{continuationActive, continuationSuperseded, continuationCompleted}, r.Continuation.State) {
		return errors.New("update: invalid automatic continuation")
	}
	return nil
}
func (s *Store) writeAutomatic(record AutomaticRecord) error {
	if err := validateAutomatic(record); err != nil {
		return err
	}
	status := automaticStatus(&record)
	status.SavedFleet = &record.Policy.Fleet
	status.SavedMembership = record.Policy.Membership
	data, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("encode automatic status bound: %w", err)
	}
	if len(data) > maximumMessage-2048 {
		return errors.New("update: automatic status exceeds signed carrier limit")
	}
	return s.writeAuthority("automatic.json", record)
}
func (s *Store) authorityDirectory() (*os.File, error) {
	directory := filepath.Dir(s.directory)
	fd, err := unix.Open(directory, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("update: open authority directory: %w", err)
	}
	file := os.NewFile(uintptr(fd), directory)
	if err = privateAuthority(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}
func privateAuthority(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("update: inspect authority file: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int64(stat.Uid) != int64(os.Geteuid()) || info.Mode().Perm()&0077 != 0 || !info.IsDir() && (!info.Mode().IsRegular() || stat.Nlink != 1) {
		return errors.New("update: authority state must be private, owned and regular")
	}
	return nil
}
func (s *Store) readAuthority(name string, value any) error {
	dir, err := s.authorityDirectory()
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("update: open authority record: %w", err)
	}
	file := os.NewFile(uintptr(fd), name)
	defer func() { _ = file.Close() }()
	if err = privateAuthority(file); err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(file, maximumMessage+1))
	if err != nil {
		return fmt.Errorf("update: read authority: %w", err)
	}
	if len(data) > maximumMessage {
		return errors.New("update: authority exceeds size limit")
	}
	if err = decodeAuthority(data, value); err != nil {
		return err
	}
	return s.settlePublication(s.directory)
}
func decodeAuthority(data []byte, value any) error {
	if err := hostprofile.CheckJSON(json.NewDecoder(bytes.NewReader(data)), 0, 32); err != nil {
		return fmt.Errorf("update: check authority JSON: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("update: decode authority: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("update: trailing authority JSON")
	}
	return nil
}
func (s *Store) writeAuthority(name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("update: encode authority: %w", err)
	}
	if len(data) > maximumMessage {
		return errors.New("update: authority exceeds size limit")
	}
	dir, err := s.authorityDirectory()
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	id, err := newAutomaticID()
	if err != nil {
		return err
	}
	temp := ".authority-" + id
	fd, err := unix.Openat(int(dir.Fd()), temp, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return fmt.Errorf("update: create authority: %w", err)
	}
	defer func() { _ = unix.Unlinkat(int(dir.Fd()), temp, 0) }()
	file := os.NewFile(uintptr(fd), temp)
	err = publishAuthority(file, dir, temp, name, data)
	if err != nil {
		return err
	}
	return s.settlePublication(filepath.Dir(s.directory))
}
func publishAuthority(file, dir *os.File, temp, name string, data []byte) error {
	defer func() { _ = file.Close() }()
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("update: write authority: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("update: sync authority: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("update: close authority: %w", err)
	}
	if err := unix.Renameat(int(dir.Fd()), temp, int(dir.Fd()), name); err != nil {
		return fmt.Errorf("update: publish authority: %w", err)
	}
	return nil
}
func (s *Store) lockContext(ctx context.Context) (func(), error) {
	ctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	file, err := s.openAuthorityLock()
	if err != nil {
		return nil, err
	}
	if err = waitAuthority(ctx, file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return func() { _ = file.Close() }, nil
}
func (s *Store) openAuthorityLock() (*os.File, error) {
	authority, err := s.authorityDirectory()
	if err != nil {
		return nil, err
	}
	defer func() { _ = authority.Close() }()
	fd, err := unix.Openat(int(authority.Fd()), "runs", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("update: open writer directory: %w", err)
	}
	dir := os.NewFile(uintptr(fd), s.directory)
	defer func() { _ = dir.Close() }()
	if err = privateAuthority(dir); err != nil {
		return nil, err
	}
	fd, err = openLockAt(int(dir.Fd()), ".lock")
	if err != nil {
		return nil, fmt.Errorf("update: open authority lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), ".lock")
	if err = privateAuthority(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func waitAuthority(ctx context.Context, file *os.File) error {
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("update: acquire authority lock: %w", err)
		}
		err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return fmt.Errorf("update: lock authority: %w", err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("update: acquire authority lock: %w", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}
func (s *Store) AcceptCheckedRelease(ctx context.Context, checked updatenotice.CheckedRelease, scope AdmissionScope) (AutomaticStatus, error) {
	unlock, err := s.lockContext(ctx)
	if err != nil {
		return AutomaticStatus{}, err
	}
	defer unlock()
	r, err := s.readAutomatic()
	if err != nil {
		return AutomaticStatus{}, err
	}
	if r == nil {
		return automaticStatus(nil), nil
	}
	if err = s.fenceAutomatic(r, scope); err != nil {
		return automaticStatus(r), err
	}
	if !r.Policy.Enabled || r.Stop != nil || !enrollmentReady(*r) {
		return automaticStatus(r), nil
	}
	if err = checked.Validate(); err != nil {
		return automaticStatus(r), err
	}
	if r.Current != nil && !automaticSuccessful(*r) {
		return automaticStatus(r), nil
	}
	eligible, err := s.automaticAdmissionReady(*r, checked)
	if err != nil {
		return automaticStatus(r), err
	}
	if !eligible {
		status := automaticStatus(r)
		status.State = "waiting for manual operation or a newer release"
		return status, nil
	}

	if r.Current != nil {
		if err = s.write(*r.Current); err != nil {
			return automaticStatus(r), err
		}
	}
	run, err := newRun(scope.CoordinatorID, r.Policy.Fleet, checked.Manifest)
	if err != nil {
		return automaticStatus(r), err
	}
	run.Origin = &AutomaticOrigin{r.Policy.ID, r.Policy.Epoch, r.Policy.Membership}
	r.Current = &run
	r.Attempt = 1
	r.Canary = nil
	r.Continuation = nil
	return automaticStatus(r), s.writeAutomatic(*r)
}
func automaticSuccessful(r AutomaticRecord) bool {
	return r.Current != nil && !r.Current.Cancel && !r.Current.Stopped && r.Stop == nil && automaticSettled(*r.Current)
}
func fleetsOverlap(a, b Fleet) bool {
	members := make(map[string]bool, len(a.Members))
	for _, host := range a.Members {
		members[host.ID] = true
	}
	for _, host := range b.Members {
		if members[host.ID] {
			return true
		}
	}
	return false
}
func (s *Store) fenceAutomatic(r *AutomaticRecord, scope AdmissionScope) error {
	kind, text := "", ""
	if scope.CoordinatorID != r.Policy.CoordinatorID || scope.Trusted == nil || !scope.Trusted(r.Policy.OwnerID) {
		kind, text = "authority_withdrawn", "Automatic policy owner is no longer an update administrator. Restore authority and explicitly recover the pinned run."
	}
	fleet, err := readAutomaticFleet(scope)
	if kind == "" && (err != nil || fleet.Digest() != r.Policy.Membership) {
		kind, text = "membership_changed", "Saved fleet changed. Cancel, settle and acknowledge-stop before enrolling the changed fleet."
	}
	if kind == "" {
		return nil
	}
	if r.Stop != nil && r.Stop.Kind != stopRecovering && r.EnrollmentResetRequired {
		return problem("automatic.stopped", text)
	}
	r.EnrollmentResetRequired = true
	stopAutomatic(r, kind, "", text)
	if err = s.writeAutomatic(*r); err != nil {
		return err
	}

	return problem("automatic.stopped", text)
}

var errAutomaticHistory = errors.New("update: automatic run is not current; history is status-only")

func (s *Store) changeAutomaticRun(id string, apply func(*Run, *AutomaticRecord) error) (Run, error) {
	r, err := s.readAutomatic()
	if err != nil {
		return Run{}, err
	}
	if r == nil || r.Current == nil || r.Current.ID != id {
		return Run{}, errAutomaticHistory
	}
	approval := r.Current.approvalIdentity()
	wasStopped := r.Current.Stopped
	if err = apply(r.Current, r); err != nil {
		return Run{}, err
	}
	if approval != r.Current.approvalIdentity() || wasStopped && !r.Current.Stopped {
		return Run{}, errors.New("update: progress cannot change automatic approval or clear its stop")
	}
	r.Current.Problem = boundedProblem(r.Current.Problem)
	for i := range r.Current.Targets {
		r.Current.Targets[i].Problem = boundedProblem(r.Current.Targets[i].Problem)
	}
	synchronizeAutomatic(r)
	r.Current.UpdatedAt = time.Now().UTC()
	return *r.Current, s.writeAutomatic(*r)
}
func synchronizeAutomatic(r *AutomaticRecord) {
	run := r.Current
	if run.Stopped && (r.Stop == nil || r.Stop.Kind == stopRecovering) {
		host, text := automaticFailure(*run)
		stopAutomatic(r, "failed", host, text)
	}
	if run.Cancel && (r.Stop == nil || r.Stop.Kind != "cancelled") {
		r.Policy.Enabled = false
		stopAutomatic(r, "cancelled", "", "Automatic operation cancelled; acknowledge-stop after settlement.")
	}
	if r.Canary != nil {
		synchronizeCanary(r)
	}
	if run.Cancel || run.Stopped || !automaticSettled(*run) {
		return
	}
	if r.Stop != nil && (r.Stop.Kind != stopRecovering || !r.Policy.Enabled && !activeContinuation(*r)) {
		return
	}
	r.Stop = nil
	r.LastCompleted = &CompletedFleetRelease{run.Release.Version, run.ReleaseDigest, time.Now().UTC()}
	if r.Continuation != nil && r.Continuation.State == continuationActive {
		r.Continuation.State = continuationCompleted
	}
}
func synchronizeCanary(r *AutomaticRecord) {
	for _, t := range r.Current.Targets {
		if t.Host.ID != r.Canary.HostID {
			continue
		}
		if t.State == Failed {
			r.Canary.State = "failed"
		}
		if t.State == Updated && t.InstallationID == "" && t.GrantAccepted && t.Build != nil && t.Build.Digest == r.Canary.ExpectedBinary {
			r.Canary.State = canaryVerified
		}
	}
}

func automaticFailure(run Run) (string, string) {
	for _, t := range run.Targets {
		if t.State == Failed {
			return t.Host.ID, t.Problem
		}
	}
	return "", run.Problem
}

func (s *Store) automaticAdmissionReady(r AutomaticRecord, checked updatenotice.CheckedRelease) (bool, error) {
	if r.LastCompleted != nil {
		order, err := release.CompareVersions(checked.Manifest.Version, r.LastCompleted.Version)
		if err != nil {
			return false, fmt.Errorf("compare automatic release: %w", err)
		}
		if order <= 0 {
			return false, nil
		}
	}
	manual, err := s.overlappingManual(r.Policy.Fleet)
	return manual == "", err
}

func releaseAuthority(file *os.File) func() { return func() { _ = file.Close() } }

func validateContinuation(r AutomaticRecord) error {
	continuation := r.Continuation
	if continuation == nil {
		return nil
	}
	if !slices.Contains([]string{continuationActive, continuationSuperseded, continuationCompleted}, continuation.State) || !ValidRunID(continuation.CommandID) || continuation.Attempt == 0 || continuation.PolicyRevision == 0 || continuation.PolicyRevision > r.Policy.Revision {
		return errors.New("update: invalid automatic continuation receipt")
	}
	if continuation.State == continuationActive && !activeContinuation(r) {
		return errors.New("update: automatic continuation differs from its pinned run")
	}
	return nil
}

func (s *Store) overlappingManual(fleet Fleet) (string, error) {
	runs, err := s.List()
	if err != nil {
		return "", err
	}
	for _, run := range runs {
		if run.Origin == nil && !automaticSettled(run) && fleetsOverlap(run.Fleet, fleet) {
			return run.ID, nil
		}
	}
	return "", nil
}
