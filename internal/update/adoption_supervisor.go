package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"time"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/updatebootstrap"
	"github.com/shaul/mesh/internal/updateinstall"
)

type AdoptionStart struct {
	ID       string
	Version  string
	Expected string
	Pin      string
	Policy   HostPolicy
	Require  []Host
	Notice   *AdoptionNotice
	Remotes  []AdoptionRemote
	Files    []string
}

type AdoptionSupervisor struct {
	Store     *Store
	Documents HostDocuments
	Side      string
	Build     release.Build
	Inspect   func(context.Context, string) (updatebootstrap.Observation, error)
	Caller    Caller
	Promote   func(context.Context, Adoption, AdoptionRelease) (updateinstall.HelperInstallation, error)
	Helper    func(context.Context, Adoption) (int, error)
	Engine    func(Adoption) (*updateinstall.Engine, error)
	Evidence  func(context.Context, Host) (HostAdoption, error)
	Deploy    func(context.Context, Adoption, AdoptionRemote) error
	Now       func() time.Time
}

func (u *AdoptionSupervisor) now() time.Time {
	if u.Now != nil {
		return u.Now().UTC()
	}
	return time.Now().UTC()
}

func NewLocalAdoptionSupervisor(stateDir, side string, documents HostDocuments) (*AdoptionSupervisor, error) {
	store, err := OpenStore(stateDir)
	if err != nil {
		return nil, err
	}
	host, key, err := identity.LoadPrivate(stateDir)
	if err != nil {
		return nil, fmt.Errorf("update: adoption requires the daemon account identity: %w", err)
	}
	client := Client{ID: host.ID, Key: key}
	u := &AdoptionSupervisor{Store: store, Documents: documents, Side: side, Build: release.Current(), Inspect: updatebootstrap.Inspect, Caller: client}
	u.Promote = func(ctx context.Context, operation Adoption, pinned AdoptionRelease) (updateinstall.HelperInstallation, error) {
		cfg := adoptionHelperConfig(operation, side)
		artifact, err := pinned.Manifest.Artifact(u.Build.Platform)
		if err != nil {
			return updateinstall.HelperInstallation{}, hostError(err)
		}
		return updateinstall.PromoteHelper(ctx, cfg, pinned.Manifest, artifact.BinarySHA256)
	}
	u.Helper = inspectAdoptionHelper
	u.Engine = func(operation Adoption) (*updateinstall.Engine, error) {
		cfg := operation.Settings.Config()
		cfg.Client, cfg.Probe = documents.Client, updatebootstrap.Probe(stateDir)
		return updateinstall.New(cfg)
	}
	return u, nil
}

func adoptionHelperConfig(operation Adoption, side string) updateinstall.HelperConfig {
	cfg := updateinstall.HelperConfig{StateDir: operation.Settings.StateDir, Executable: side, Kind: operation.Settings.Service.Kind, Domain: operation.Settings.Service.Domain}
	cfg.Probe = updatebootstrap.HelperProcessProbe(cfg)
	return cfg
}

func inspectAdoptionHelper(ctx context.Context, operation Adoption) (int, error) {
	if operation.Helper == nil {
		return 0, errors.New("update: adoption has no promoted helper receipt")
	}
	cfg := adoptionHelperConfig(operation, operation.Helper.Executable)
	installed, err := updateinstall.InspectHelper(ctx, cfg)
	if err != nil {
		return 0, fmt.Errorf("update: inspect promoted adoption helper: %w", err)
	}
	if installed != *operation.Helper {
		return 0, errors.New("update: promoted helper receipt or link changed during adoption")
	}
	pid, err := cfg.Probe(ctx, installed)
	return pid, hostError(err)
}

func (u *AdoptionSupervisor) Start(ctx context.Context, start AdoptionStart) error {
	if u.Store == nil || u.Inspect == nil || u.Promote == nil {
		return errors.New("update: adoption supervisor is incomplete")
	}
	if found, err := u.existingStart(ctx, start); found || err != nil {
		return err
	}
	pinned, err := ReadAdoptionRelease(ctx, u.Documents, start.Version)
	if err != nil {
		return err
	}
	if start.Pin != "" && start.Pin != pinned.Pin {
		return errors.New("update: requested adoption metadata pin differs from the signed release")
	}
	if err := pinned.verifySide(ctx, u.Side, u.Build); err != nil {
		return err
	}
	operation, revision, err := u.prepare(ctx, start, pinned)
	if err != nil {
		return err
	}
	if err := u.Store.beginAdoption(ctx, operation, revision); err != nil {
		return err
	}
	return u.Resume(ctx)
}

func (u *AdoptionSupervisor) existingStart(ctx context.Context, start AdoptionStart) (bool, error) {
	_, prior, err := u.Store.AdoptionStatus(ctx)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	if start.ID != "" && start.ID != prior.ID || start.Pin != "" && start.Pin != prior.Pin || start.Version != prior.Version {
		return true, errors.New("update: adoption is already pinned to another operation or release")
	}
	return true, u.Resume(ctx)
}

func (u *AdoptionSupervisor) prepare(ctx context.Context, start AdoptionStart, pinned AdoptionRelease) (Adoption, uint64, error) {
	stateDir := u.Store.stateDir()
	observed, err := u.Inspect(ctx, stateDir)
	if err != nil {
		return Adoption{}, 0, hostError(err)
	}
	if start.Expected != "" && observed.Health.HostID != start.Expected {
		return Adoption{}, 0, errors.New("update: SSH account does not contain the expected daemon identity")
	}
	if err := checkAdoptionIdentity(stateDir, observed.Health.HostID); err != nil {
		return Adoption{}, 0, err
	}
	if err := pinned.verifySource(ctx, u.Documents, observed.Health); err != nil {
		return Adoption{}, 0, err
	}
	settings, generation, err := adoptionSettings(ctx, stateDir, observed)
	if err != nil {
		return Adoption{}, 0, err
	}
	files, err := SnapshotAdoptionFiles(start.Files)
	if err != nil {
		return Adoption{}, 0, err
	}
	policy, err := u.Store.HostPolicy(ctx)
	if err != nil {
		return Adoption{}, 0, err
	}
	if start.ID == "" {
		start.ID, err = newAutomaticID()
		if err != nil {
			return Adoption{}, 0, err
		}
	}
	start.Policy.Schema, start.Policy.Revision = 1, policy.Revision+1
	operation := Adoption{Schema: 1, ID: start.ID, Pin: pinned.Pin, Signature: pinned.Signature, Version: start.Version, Host: AdoptionHost{Health: observed.Health, Preserved: files}, Policy: start.Policy, Require: start.Require, Notice: start.Notice, Remotes: start.Remotes, Phase: AdoptionRecorded, Settings: settings,
		Request: updateinstall.Request{ID: start.ID, TargetID: observed.Health.HostID, Generation: generation, Manifest: pinned.Manifest, Current: observed.Health.Build}}
	return operation, policy.Revision, operation.validate()
}

func (u *AdoptionSupervisor) Resume(ctx context.Context) error {
	envelope, operation, err := u.Store.AdoptionStatus(ctx)
	if err != nil {
		return err
	}
	if envelope.Confirmed || operation.Phase == AdoptionRolledBack || operation.Phase == AdoptionAttention {
		return nil
	}
	if u.now().Before(operation.NextAttempt) {
		return nil
	}
	pinned, err := u.verifyResume(ctx, operation)
	if err != nil {
		return u.hold(ctx, operation, err)
	}
	switch operation.Phase {
	case AdoptionRecorded:
		return u.promote(ctx, operation, pinned)
	case AdoptionWaiting:
		return u.wait(ctx, operation)
	case AdoptionActivating:
		return u.activate(ctx, operation)
	case AdoptionConfirming:
		return u.confirm(ctx, operation)
	case AdoptionComplete, AdoptionRolledBack, AdoptionAttention:
		return nil
	default:
		return errors.New("update: unknown adoption phase")
	}
}

func (u *AdoptionSupervisor) verifyResume(ctx context.Context, operation Adoption) (AdoptionRelease, error) {
	pinned, err := ReadAdoptionRelease(ctx, u.Documents, operation.Version)
	if err != nil {
		return pinned, err
	}
	if pinned.Pin != operation.Pin || pinned.Signature != operation.Signature || !reflect.DeepEqual(pinned.Manifest, operation.Request.Manifest) {
		return pinned, errors.New("update: adoption release changed from its exact document pins")
	}
	if err := pinned.verifySide(ctx, u.Side, u.Build); err != nil {
		return pinned, err
	}
	if err := pinned.verifySource(ctx, u.Documents, operation.Host.Health); err != nil {
		return pinned, err
	}
	return pinned, checkAdoptionFiles(operation.Host.Preserved)
}

func (u *AdoptionSupervisor) promote(ctx context.Context, operation Adoption, pinned AdoptionRelease) error {
	if err := u.Store.seedAdoptionPolicy(ctx, operation); err != nil {
		return u.hold(ctx, operation, err)
	}
	installed, err := u.Promote(ctx, operation, pinned)
	if err != nil {
		return u.hold(ctx, operation, err)
	}
	next := operation
	next.Helper, next.Phase, next.Problem = &installed, AdoptionWaiting, ""
	return u.Store.saveAdoption(ctx, operation, next)
}

func (u *AdoptionSupervisor) hold(ctx context.Context, operation Adoption, cause error) error {
	next := operation
	next.Failures = min(operation.Failures+1, 6)
	next.NextAttempt = u.now().Add(min(time.Minute*time.Duration(1<<(next.Failures-1)), 30*time.Minute))
	next.Problem = boundedProblem(cause.Error())
	return errors.Join(cause, u.Store.saveAdoption(ctx, operation, next))
}

func (u *AdoptionSupervisor) wait(ctx context.Context, operation Adoption) error {
	if changed, err := u.deployRemotes(ctx, operation); changed || err != nil {
		return err
	}
	if err := u.checkPeers(ctx, operation); err != nil {
		return u.hold(ctx, operation, err)
	}
	if operation.Notice != nil && operation.Notice.DeliveredAt.IsZero() {
		return u.deliverNotice(ctx, operation)
	}
	if operation.Notice != nil && u.now().Before(operation.Notice.DeliveredAt.Add(operation.Notice.Lead)) {
		return nil
	}
	if u.Helper == nil {
		return u.hold(ctx, operation, errors.New("update: adoption helper process probe unavailable"))
	}
	if _, err := u.Helper(ctx, operation); err != nil {
		return u.hold(ctx, operation, err)
	}
	next := operation
	next.Phase, next.Problem, next.NextAttempt = AdoptionActivating, "", time.Time{}
	return u.Store.saveAdoption(ctx, operation, next)
}

func (u *AdoptionSupervisor) checkPeers(ctx context.Context, operation Adoption) error {
	for _, peer := range operation.Require {
		if u.Evidence == nil {
			return fmt.Errorf("update: predecessor %s evidence reader unavailable", peer.ID)
		}
		evidence, err := u.Evidence(ctx, peer)
		if err != nil {
			return fmt.Errorf("update: predecessor %s: %w", peer.ID, err)
		}
		if err := AdoptionMilestones(evidence, operation.ID, operation.Pin); err != nil {
			return fmt.Errorf("update: predecessor %s: %w", peer.ID, err)
		}
	}
	return nil
}

func (u *AdoptionSupervisor) deliverNotice(ctx context.Context, operation Adoption) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	command := operation.Notice.Command
	if err := runAdoptionNotice(ctx, command); err != nil {
		return u.hold(ctx, operation, fmt.Errorf("update: adoption notice failed: %w", err))
	}
	next, notice := operation, *operation.Notice
	notice.DeliveredAt = u.now()
	next.Notice, next.Problem, next.Failures = &notice, "", 0
	return u.Store.saveAdoption(ctx, operation, next)
}

func runAdoptionNotice(ctx context.Context, argv []string) error {
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return fmt.Errorf("locate notice executable: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("start adoption notice: %w", err)
	}
	command := &exec.Cmd{Path: path, Args: slices.Clone(argv), WaitDelay: time.Second}
	if err := command.Start(); err != nil {
		return fmt.Errorf("start adoption notice: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { _ = command.Process.Kill() })
	defer stop()
	if err := command.Wait(); err != nil {
		return fmt.Errorf("wait for adoption notice: %w", err)
	}
	return nil
}

func (u *AdoptionSupervisor) deployRemotes(ctx context.Context, operation Adoption) (bool, error) {
	for index, remote := range operation.Remotes {
		if remote.State == "started" || u.now().Before(remote.NextAttempt) {
			continue
		}
		return true, u.deployRemote(ctx, operation, index)
	}
	return false, nil
}

func (u *AdoptionSupervisor) deployRemote(ctx context.Context, operation Adoption, index int) error {
	if u.Deploy == nil {
		return u.hold(ctx, operation, errors.New("update: configured system SSH deployment unavailable"))
	}
	err := u.Deploy(ctx, operation, operation.Remotes[index])
	next := operation
	next.Remotes = append([]AdoptionRemote(nil), operation.Remotes...)
	remote := &next.Remotes[index]
	remote.State, remote.Problem = "started", ""
	if err != nil {
		remote.State, remote.Problem, remote.NextAttempt = "blocked", boundedProblem(err.Error()), u.now().Add(time.Minute)
	}
	return errors.Join(err, u.Store.saveAdoption(ctx, operation, next))
}
