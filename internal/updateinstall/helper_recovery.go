package updateinstall

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/updategate"
)

type JournalExpectation struct {
	Operation      string
	Generation     uint64
	Phase          Phase
	OriginalDigest string
}

func (expected JournalExpectation) Validate() error {
	if !operationPattern.MatchString(expected.Operation) || expected.Generation == 0 ||
		len(expected.OriginalDigest) != 64 || strings.Trim(expected.OriginalDigest, "0123456789abcdef") != "" {
		return errors.New("recovery requires an exact journal tuple")
	}
	if expected.Phase != RolledBack && expected.Phase != Failed && expected.Phase != RollbackFailed {
		return errors.New("helper recovery requires a rollback or pre-activation failure")
	}
	return nil
}

type HelperRecovery struct {
	Helper    HelperConfig
	Expected  JournalExpectation
	Manifest  release.Manifest
	Digest    string
	Probe     func(context.Context, HelperInstallation) (int, error)
	CheckIdle func(context.Context, Status) error
}

type HelperRecoveryResult struct {
	Installation HelperInstallation `json:"installation"`
	PID          int                `json:"pid"`
	Phase        Phase              `json:"phase"`
}

type helperRecoverySnapshot struct {
	installation HelperInstallation
	service      []byte
	receipt      []byte
}

// RecoverHelper repairs only the executor. Daemon activation still needs a
// separate approval through the ordinary update transaction.
func (e *Engine) RecoverHelper(ctx context.Context, request HelperRecovery) (HelperRecoveryResult, error) {
	if err := request.Expected.Validate(); err != nil {
		return HelperRecoveryResult{}, err
	}
	if request.Helper.StateDir != e.cfg.StateDir || !filepath.IsAbs(request.Helper.Executable) || !filepath.IsAbs(request.Helper.ServiceDir) ||
		e.cfg.ClientOnly || request.Probe == nil || request.CheckIdle == nil {
		return HelperRecoveryResult{}, errors.New("helper recovery requires daemon, helper-image, and pending-approval probes")
	}
	// Image verification and preparation use the bounded recovery envelope.
	// Helper readiness gets its own health budget after preparation.
	ctx, cancel := context.WithTimeout(ctx, e.cfg.HealthTimeout+15*time.Second)
	defer cancel()
	initial, err := ReadContext(ctx, e.cfg.StateDir)
	if err != nil {
		return HelperRecoveryResult{}, err
	}
	if err = expectRecoveryJournal(initial, request.Expected); err != nil {
		return HelperRecoveryResult{}, err
	}
	lock, err := lockInstallation(ctx, e.cfg.StateDir)
	if err != nil {
		return HelperRecoveryResult{}, err
	}
	defer unlock(lock)
	if err = e.unchangedRecoveryJournal(ctx, initial); err != nil {
		return HelperRecoveryResult{}, err
	}
	prior, err := e.recoverySnapshot(ctx, request, initial)
	if err != nil {
		return HelperRecoveryResult{}, err
	}
	initial, err = e.restoreRecoveryRollback(ctx, initial)
	if err != nil {
		return HelperRecoveryResult{}, err
	}
	if prior.installation.Digest == request.Digest {
		return e.verifyRecoveredHelper(ctx, request, initial, prior.installation)
	}
	if err = ctx.Err(); err != nil {
		return HelperRecoveryResult{}, fmt.Errorf("helper recovery cancelled: %w", err)
	}
	return e.promoteRecoveryHelper(ctx, request, initial, prior)
}

func (e *Engine) recoverySnapshot(ctx context.Context, request HelperRecovery, initial Status) (helperRecoverySnapshot, error) {
	if initial.Settings != e.settings() || request.Helper.Kind != initial.Settings.Service.Kind || request.Helper.Domain != initial.Settings.Service.Domain {
		return helperRecoverySnapshot{}, errors.New("recovery configuration differs from the installation journal")
	}
	if err := validateRequest(initial.Request); err != nil {
		return helperRecoverySnapshot{}, err
	}
	if err := e.recoveryIdle(ctx, request, initial); err != nil {
		return helperRecoverySnapshot{}, err
	}
	health, err := e.recoveryOriginalHealth(ctx, initial)
	if err != nil {
		return helperRecoverySnapshot{}, err
	}
	prior, err := recoveryHelperSnapshot(ctx, request.Helper)
	if err != nil {
		return helperRecoverySnapshot{}, err
	}
	priorPID, err := request.Probe(ctx, prior.installation)
	if err != nil || priorPID <= 0 {
		return helperRecoverySnapshot{}, errors.Join(errors.New("verify running prior helper process"), err)
	}
	if err = e.verifyRecoveryCandidate(ctx, request, prior.installation, health); err != nil {
		return helperRecoverySnapshot{}, err
	}
	if err = e.unchangedRecoveryJournal(ctx, initial); err != nil {
		return helperRecoverySnapshot{}, err
	}
	if err = e.recoveryIdle(ctx, request, initial); err != nil {
		return helperRecoverySnapshot{}, err
	}
	return prior, nil
}

func (e *Engine) restoreRecoveryRollback(ctx context.Context, initial Status) (Status, error) {
	if initial.Phase != RollbackFailed {
		return initial, nil
	}
	// The existing restore path owns daemon stop/start and the real receipt.
	restorer := *e
	restorer.cfg.Probe = func(ctx context.Context) (Health, error) {
		observed, probeErr := e.cfg.Probe(ctx)
		if probeErr == nil {
			probeErr = verifyRecoveryHealth(observed, initial)
		}
		return observed, probeErr
	}
	settled, err := restorer.restoreRollback(ctx, initial)
	if err != nil {
		return settled, err
	}
	if err = verifyRecoveryHealth(*settled.Verified, settled); err != nil {
		return settled, err
	}
	return settled, nil
}

func (e *Engine) promoteRecoveryHelper(ctx context.Context, request HelperRecovery, initial Status, prior helperRecoverySnapshot) (HelperRecoveryResult, error) {
	if err := retainRecoveryHelperReceipt(ctx, e.cfg.StateDir, prior); err != nil {
		return HelperRecoveryResult{}, err
	}
	promotionStarted := false
	request.Helper.beforePromotion = func(ctx context.Context) error {
		if err := e.recoveryPromotionGuard(ctx, request, initial, prior); err != nil {
			return err
		}
		promotionStarted = true
		return nil
	}
	installed, err := prepareHelper(ctx, request.Helper, true, request.Digest)
	if err == nil {
		_, err = activateHelper(ctx, request.Helper, installed)
	}
	var result HelperRecoveryResult
	if err == nil {
		result, err = e.verifyRecoveredHelper(ctx, request, initial, installed)
	}
	if err == nil {
		return result, nil
	}
	cause := fmt.Errorf("replacement helper did not become ready: %w", err)
	if !promotionStarted || recoveryHelperUnchanged(ctx, e.cfg.StateDir, prior) {
		return HelperRecoveryResult{}, cause
	}
	return HelperRecoveryResult{}, errors.Join(cause, e.restoreRecoveryHelper(ctx, request, prior))
}

func (e *Engine) verifyRecoveredHelper(ctx context.Context, request HelperRecovery, initial Status, installed HelperInstallation) (HelperRecoveryResult, error) {
	readyCtx, cancel := context.WithTimeout(ctx, e.cfg.HealthTimeout)
	readyPID, err := awaitRecoveryHelper(readyCtx, request.Probe, installed)
	cancel()
	if err != nil {
		return HelperRecoveryResult{}, err
	}
	if err := e.unchangedRecoveryJournal(ctx, initial); err != nil {
		return HelperRecoveryResult{}, err
	}
	if err := e.recoveryIdle(ctx, request, initial); err != nil {
		return HelperRecoveryResult{}, err
	}
	if _, err := e.recoveryOriginalHealth(ctx, initial); err != nil {
		return HelperRecoveryResult{}, err
	}
	pid, err := request.Probe(ctx, installed)
	if err != nil || pid <= 0 || pid != readyPID {
		return HelperRecoveryResult{}, errors.Join(errors.New("recovered helper process changed before readiness"), err)
	}
	return HelperRecoveryResult{Installation: installed, PID: pid, Phase: initial.Phase}, nil
}

func (e *Engine) recoveryPromotionGuard(ctx context.Context, request HelperRecovery, initial Status, prior helperRecoverySnapshot) error {
	if err := e.unchangedRecoveryJournal(ctx, initial); err != nil {
		return err
	}
	if err := e.recoveryIdle(ctx, request, initial); err != nil {
		return err
	}
	if !recoveryHelperUnchanged(ctx, request.Helper.StateDir, prior) {
		return errors.New("prior helper changed during recovery")
	}
	_, err := e.recoveryOriginalHealth(ctx, initial)
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return fmt.Errorf("helper promotion cancelled: %w", err)
	}
	return nil
}

func (e *Engine) recoveryOriginalHealth(ctx context.Context, initial Status) (Health, error) {
	if err := release.VerifyExecutable(ctx, e.cfg.Executable, initial.Request.Current.Digest); err != nil {
		return Health{}, fmt.Errorf("verify original daemon executable: %w", err)
	}
	health, err := e.cfg.Probe(ctx)
	if err != nil {
		return Health{}, err
	}
	if err = verifyRecoveryHealth(health, initial); err != nil {
		return Health{}, err
	}
	return health, nil
}

func retainRecoveryHelperReceipt(ctx context.Context, stateDir string, prior helperRecoverySnapshot) error {
	name := filepath.Join("update", "helper", prior.installation.Digest, "installed.json")
	path := filepath.Join(stateDir, name)
	retained, err := readMetadata(ctx, stateDir, name, true)
	if err == nil {
		if !bytes.Equal(retained, prior.receipt) {
			return errors.New("retained prior helper receipt differs from the verified installation")
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read retained prior helper receipt: %w", err)
	}
	if err = atomicWrite(path, prior.receipt, 0600); err != nil {
		return fmt.Errorf("retain prior helper receipt: %w", err)
	}
	return nil
}

func recoveryHelperUnchanged(ctx context.Context, stateDir string, prior helperRecoverySnapshot) bool {
	receipt, receiptErr := readMetadata(ctx, stateDir, filepath.Join("update", "helper", "installed.json"), true)
	service, serviceErr := readMetadata(ctx, filepath.Dir(prior.installation.ServicePath), filepath.Base(prior.installation.ServicePath), false)
	link, linkErr := os.Readlink(filepath.Join(transactionDir(stateDir), "helper", "current"))
	return receiptErr == nil && serviceErr == nil && linkErr == nil &&
		bytes.Equal(receipt, prior.receipt) && bytes.Equal(service, prior.service) && link == prior.installation.Executable
}

func expectRecoveryJournal(status Status, expected JournalExpectation) error {
	if status.Request.ID != expected.Operation || status.Request.Generation != expected.Generation ||
		status.Phase != expected.Phase || status.Request.Current.Digest != expected.OriginalDigest {
		return errors.New("installation journal changed; review it before helper recovery")
	}
	return nil
}

func (e *Engine) unchangedRecoveryJournal(ctx context.Context, expected Status) error {
	current, err := ReadContext(ctx, e.cfg.StateDir)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, expected) {
		return errors.New("installation journal changed during helper recovery")
	}
	return nil
}

func (e *Engine) recoveryIdle(ctx context.Context, request HelperRecovery, status Status) error {
	if _, err := os.Lstat(filepath.Join(e.cfg.StateDir, "activation.pending")); !errors.Is(err, os.ErrNotExist) {
		return errors.Join(errors.New("service activation is pending; stop helper recovery"), err)
	}
	if _, err := os.Lstat(updategate.Path(e.cfg.StateDir)); !errors.Is(err, os.ErrNotExist) {
		return errors.Join(errors.New("update activation gate is pending; stop helper recovery"), err)
	}
	return request.CheckIdle(ctx, status)
}

func verifyRecoveryHealth(health Health, status Status) error {
	if health.Build.Modified || health.Build.StateVersion != status.Original.Build.StateVersion ||
		health.BootID == "" || health.BootID != status.Original.BootID {
		return errors.New("recovery requires the original boot and unchanged daemon state")
	}
	if err := verifyHealth(health, status.Request.Current, status); err != nil {
		return err
	}
	if err := compatibleWorkers(health, status.Request.Manifest.Compatibility); err != nil {
		return err
	}
	for _, prior := range status.Original.Workers {
		for _, current := range health.Workers {
			if current.ID == prior.ID && !reflect.DeepEqual(current.Build, prior.Build) {
				return errors.New("original worker executing image changed")
			}
		}
	}
	return nil
}

func recoveryHelperSnapshot(ctx context.Context, cfg HelperConfig) (helperRecoverySnapshot, error) {
	var snapshot helperRecoverySnapshot
	receipt, err := readMetadata(ctx, cfg.StateDir, filepath.Join("update", "helper", "installed.json"), true)
	if err != nil {
		return snapshot, err
	}
	if err = readJSON(receipt, &snapshot.installation); err != nil {
		return snapshot, err
	}
	snapshot.receipt = receipt
	prior := snapshot.installation
	if prior.Executable != filepath.Join(transactionDir(cfg.StateDir), "helper", prior.Digest, "mesh") {
		return snapshot, errors.New("prior helper receipt requires its immutable executable")
	}
	if err := release.VerifyExecutable(ctx, prior.Executable, prior.Digest); err != nil {
		return snapshot, fmt.Errorf("verify retained prior helper: %w", err)
	}
	link, err := os.Readlink(filepath.Join(transactionDir(cfg.StateDir), "helper", "current"))
	if err != nil || link != prior.Executable {
		return snapshot, errors.Join(errors.New("prior helper promotion is unfinished; stop recovery"), err)
	}
	service, name, err := helperService(cfg, filepath.Join(transactionDir(cfg.StateDir), "helper", "current"))
	if err != nil {
		return snapshot, err
	}
	if prior.ServicePath != filepath.Join(cfg.ServiceDir, name) {
		return snapshot, errors.New("helper service differs from its managed receipt")
	}
	snapshot.service, err = readMetadata(ctx, cfg.ServiceDir, name, false)
	if err != nil {
		return snapshot, fmt.Errorf("read prior helper service: %w", err)
	}
	if !bytes.Equal(snapshot.service, []byte(service)) {
		return snapshot, errors.New("helper service configuration differs from its managed definition")
	}
	return snapshot, nil
}

func (e *Engine) verifyRecoveryCandidate(ctx context.Context, request HelperRecovery, prior HelperInstallation, health Health) error {
	build, err := e.recoveryCandidateBuild(ctx, request)
	if err != nil {
		return err
	}
	manifest := request.Manifest
	if err = helperCapabilities(health.Build, manifest); err != nil {
		return err
	}
	if err = compatibleWorkers(health, manifest.Compatibility); err != nil {
		return err
	}
	current, err := helperBuild(ctx, prior.Executable, prior.Digest)
	if err != nil {
		return err
	}
	if err = helperCapabilities(current, manifest); err != nil {
		return err
	}
	order, err := release.CompareVersions(build.Version, current.Version)
	if err != nil {
		return fmt.Errorf("compare replacement helper release: %w", err)
	}
	if order < 0 || order == 0 && request.Digest != prior.Digest {
		return errors.New("replacement helper must preserve or advance the verified release")
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err = helperCommandOutput(probeCtx, filepath.Join(transactionDir(request.Helper.StateDir), "helper", request.Digest, "mesh"), "update-helper", "--state-dir", request.Helper.StateDir, "--check-journal")
	return err
}

func (e *Engine) recoveryCandidateBuild(ctx context.Context, request HelperRecovery) (release.Build, error) {
	manifest, err := e.cfg.Client.Manifest(ctx, request.Manifest.Version)
	if err != nil {
		return release.Build{}, fmt.Errorf("read replacement helper descriptor: %w", err)
	}
	if manifest.Digest() != request.Manifest.Digest() {
		return release.Build{}, errors.New("replacement helper release differs from its published descriptor")
	}
	artifact, err := manifest.Artifact(release.CurrentPlatform())
	if err != nil {
		return release.Build{}, fmt.Errorf("read replacement helper artifact: %w", err)
	}
	if artifact.BinarySHA256 != request.Digest {
		return release.Build{}, errors.New("replacement helper digest differs from its published artifact")
	}
	if err = release.VerifyExecutable(ctx, request.Helper.Executable, request.Digest); err != nil {
		return release.Build{}, fmt.Errorf("verify replacement helper source: %w", err)
	}
	staged, err := stageHelperImage(ctx, request.Helper, request.Digest)
	if err != nil {
		return release.Build{}, err
	}
	build, err := helperBuild(ctx, staged.Executable, request.Digest)
	if err != nil {
		return release.Build{}, err
	}
	if build.Version != manifest.Version || build.Commit != manifest.Commit ||
		build.StateVersion != manifest.Compatibility.StateWrite || build.WorkerProtocol != manifest.Compatibility.WorkerWrite {
		return release.Build{}, errors.New("replacement helper build differs from its published release")
	}
	return build, nil
}

func awaitRecoveryHelper(ctx context.Context, probe func(context.Context, HelperInstallation) (int, error), installed HelperInstallation) (int, error) {
	for {
		pid, err := probe(ctx, installed)
		if err == nil && pid > 0 {
			return pid, nil
		}
		if waitErr := waitContext(ctx, 100*time.Millisecond); waitErr != nil {
			return 0, errors.Join(errors.New("running helper image did not become ready"), err, waitErr)
		}
	}
}

func (e *Engine) restoreRecoveryHelper(ctx context.Context, request HelperRecovery, prior helperRecoverySnapshot) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), e.cfg.HealthTimeout+15*time.Second)
	defer cancel()
	if err := release.VerifyExecutable(ctx, prior.installation.Executable, prior.installation.Digest); err != nil {
		return fmt.Errorf("restore prior helper executable: %w", err)
	}
	if err := atomicWrite(prior.installation.ServicePath, prior.service, 0644); err != nil {
		return fmt.Errorf("restore prior helper service: %w", err)
	}
	if err := atomicWrite(helperRecord(e.cfg.StateDir), prior.receipt, 0600); err != nil {
		return fmt.Errorf("restore prior helper receipt: %w", err)
	}
	if err := replaceHelperLink(filepath.Join(transactionDir(e.cfg.StateDir), "helper", "current"), prior.installation.Executable); err != nil {
		return fmt.Errorf("restore prior helper launcher: %w", err)
	}
	if _, err := activateHelper(ctx, request.Helper, prior.installation); err != nil {
		return fmt.Errorf("restart prior helper: %w", err)
	}
	readyCtx, readyCancel := context.WithTimeout(ctx, e.cfg.HealthTimeout)
	defer readyCancel()
	_, err := awaitRecoveryHelper(readyCtx, request.Probe, prior.installation)
	if err != nil {
		return fmt.Errorf("verify restored prior helper process: %w", err)
	}
	return nil
}
