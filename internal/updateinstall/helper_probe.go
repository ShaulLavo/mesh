package updateinstall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/shaul/mesh/internal/release"
	"golang.org/x/sys/unix"
)

const helperMetadataLimit = 64 << 10

const helperReadinessTimeout = 30 * time.Second

const (
	promotionSystemd = "systemd"
	promotionLaunchd = "launchd"
)

// PromoteHelper takes a caller-verified release descriptor and verifies the
// local image again before changing the helper. It never activates the daemon.
func PromoteHelper(ctx context.Context, cfg HelperConfig, source release.Manifest, digest string) (HelperInstallation, error) {
	return ensureHelper(ctx, cfg, source, digest, false)
}

// EnsureCurrentHelper preserves a newer helper and returns only after the
// caller's process probe binds a stable running PID to the retained image.
func EnsureCurrentHelper(ctx context.Context, cfg HelperConfig, source release.Manifest) (HelperInstallation, error) {
	artifact, err := source.Artifact(release.CurrentPlatform())
	if err != nil {
		return HelperInstallation{}, fmt.Errorf("read current helper artifact: %w", err)
	}
	promote, err := helperNeedsPromotion(ctx, cfg, source, artifact.BinarySHA256)
	if err != nil {
		return HelperInstallation{}, err
	}
	if promote {
		return PromoteHelper(ctx, cfg, source, artifact.BinarySHA256)
	}
	return ensureHelper(ctx, cfg, source, artifact.BinarySHA256, true)
}

func helperNeedsPromotion(ctx context.Context, cfg HelperConfig, source release.Manifest, digest string) (bool, error) {
	if cfg.Probe == nil {
		return false, errors.New("helper readiness requires an executing-image process probe")
	}
	if cfg.ServiceDir == "" {
		var err error
		cfg.ServiceDir, err = serviceDirectory(cfg.Kind)
		if err != nil {
			return false, err
		}
	}
	lock, err := lockInstallation(ctx, cfg.StateDir)
	if err != nil {
		return false, err
	}
	defer unlock(lock)
	if err = helperPromotionJournal(ctx, cfg); err != nil {
		return false, err
	}
	if err = resumeHelperPromotion(ctx, cfg); err != nil {
		return false, err
	}
	var installed HelperInstallation
	if err = readHelperInstallation(ctx, cfg.StateDir, &installed); err != nil {
		return false, err
	}
	if installed.Digest == digest {
		return true, nil
	}
	if _, err = verifyHelperReceipt(ctx, cfg.StateDir, installed, digest); err != nil {
		return false, err
	}
	current, err := helperBuild(ctx, installed.Executable, installed.Digest)
	if err != nil {
		return false, err
	}
	order, err := release.CompareVersions(source.Version, current.Version)
	if err != nil {
		return false, fmt.Errorf("compare installed helper currency: %w", err)
	}
	return order >= 0, nil
}

// InspectHelper verifies the existing installation without starting, replacing,
// or pruning an image that an unfinished transaction still owns.
func InspectHelper(ctx context.Context, cfg HelperConfig) (HelperInstallation, error) {
	if !filepath.IsAbs(cfg.StateDir) {
		return HelperInstallation{}, errors.New("helper inspection requires an absolute state path")
	}
	if cfg.ServiceDir == "" {
		var err error
		cfg.ServiceDir, err = serviceDirectory(cfg.Kind)
		if err != nil {
			return HelperInstallation{}, err
		}
	}
	lock, err := lockInstallation(ctx, cfg.StateDir)
	if err != nil {
		return HelperInstallation{}, err
	}
	defer unlock(lock)
	snapshot, err := recoveryHelperSnapshot(ctx, cfg)
	return snapshot.installation, err
}

type helperPromotion struct {
	Prior   HelperInstallation
	Receipt []byte
	Service []byte
	Target  string
}

func helperPromotionPath(stateDir string) string {
	return filepath.Join(transactionDir(stateDir), "helper", "promotion.json")
}

func ensureHelper(ctx context.Context, cfg HelperConfig, source release.Manifest, digest string, preserveNewer bool) (HelperInstallation, error) {
	if cfg.Probe == nil {
		return HelperInstallation{}, errors.New("helper readiness requires an executing-image process probe")
	}
	if cfg.ServiceDir == "" {
		var err error
		cfg.ServiceDir, err = serviceDirectory(cfg.Kind)
		if err != nil {
			return HelperInstallation{}, err
		}
	}
	lock, err := lockInstallation(ctx, cfg.StateDir)
	if err != nil {
		return HelperInstallation{}, err
	}
	defer unlock(lock)
	if err = helperPromotionJournal(ctx, cfg); err != nil {
		return HelperInstallation{}, err
	}
	if err = resumeHelperPromotion(ctx, cfg); err != nil {
		return HelperInstallation{}, err
	}
	candidate, err := verifiedHelperCandidate(ctx, cfg, source, digest)
	if err != nil {
		return HelperInstallation{}, err
	}
	prior, err := recoveryHelperSnapshot(ctx, cfg)
	if err != nil {
		return HelperInstallation{}, err
	}
	current, err := helperBuild(ctx, prior.installation.Executable, prior.installation.Digest)
	if err != nil {
		return prior.installation, err
	}
	order, err := helperPromotionOrder(candidate, current, preserveNewer)
	if err != nil {
		return prior.installation, err
	}
	if order <= 0 {
		return ensureRetainedHelper(ctx, cfg, prior.installation)
	}
	return promoteVerifiedHelper(ctx, cfg, prior, digest)
}

func helperPromotionOrder(candidate, current release.Build, preserveNewer bool) (int, error) {
	order, err := release.CompareVersions(candidate.Version, current.Version)
	if err != nil {
		return 0, fmt.Errorf("compare helper promotion releases: %w", err)
	}
	if order == 0 && candidate.Digest != current.Digest || order < 0 && !preserveNewer {
		return 0, errors.New("helper promotion must preserve or advance the verified release")
	}
	return order, nil
}

func helperPromotionJournal(ctx context.Context, cfg HelperConfig) error {
	if !filepath.IsAbs(cfg.StateDir) || !filepath.IsAbs(cfg.Executable) || !filepath.IsAbs(cfg.ServiceDir) {
		return errors.New("helper promotion requires absolute state, executable, and service paths")
	}
	if cfg.Kind != promotionSystemd && cfg.Kind != promotionLaunchd || cfg.Kind == promotionLaunchd && !launchDomainPattern.MatchString(cfg.Domain) {
		return errors.New("helper promotion requires a supported user service manager")
	}
	status, err := ReadContext(ctx, cfg.StateDir)
	if err != nil {
		return err
	}
	if !finished(status.Phase) || status.Phase == RollbackFailed {
		return errors.New("helper promotion requires a finished installation journal")
	}
	if status.Settings.StateDir != cfg.StateDir || !status.Settings.ClientOnly &&
		(status.Settings.Service.Kind != cfg.Kind || status.Settings.Service.Domain != cfg.Domain) {
		return errors.New("helper service settings differ from the installation journal")
	}
	return nil
}

func verifiedHelperCandidate(ctx context.Context, cfg HelperConfig, source release.Manifest, digest string) (release.Build, error) {
	artifact, err := source.Artifact(release.CurrentPlatform())
	if err != nil {
		return release.Build{}, fmt.Errorf("read promotion helper artifact: %w", err)
	}
	if artifact.BinarySHA256 != digest {
		return release.Build{}, errors.New("helper digest differs from the verified release artifact")
	}
	if err = release.VerifyExecutable(ctx, cfg.Executable, digest); err != nil {
		return release.Build{}, fmt.Errorf("verify promotion helper image: %w", err)
	}
	installed, err := stageHelperImage(ctx, cfg, digest)
	if err != nil {
		return release.Build{}, err
	}
	build, err := helperBuild(ctx, installed.Executable, digest)
	if err != nil {
		return build, err
	}
	if build.Version != source.Version || build.Commit != source.Commit ||
		build.StateVersion != source.Compatibility.StateWrite || build.WorkerProtocol != source.Compatibility.WorkerWrite ||
		source.Compatibility.JournalVersion != release.CurrentJournalVersion {
		return build, errors.New("helper image differs from the verified release build")
	}
	return build, checkHelperJournal(ctx, cfg.StateDir, installed.Executable)
}

func checkHelperJournal(ctx context.Context, stateDir, executable string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := helperCommandOutput(ctx, executable, "update-helper", "--state-dir", stateDir, "--check-journal")
	return err
}

func ensureRetainedHelper(ctx context.Context, cfg HelperConfig, installed HelperInstallation) (HelperInstallation, error) {
	if err := checkHelperJournal(ctx, cfg.StateDir, installed.Executable); err != nil {
		return installed, err
	}
	pending, err := readMetadata(ctx, cfg.StateDir, filepath.Join("update", "helper", "activation-pending"), true)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return installed, err
	}
	if err == nil {
		if string(pending) != installed.Digest {
			return installed, errors.New("pending activation differs from the retained helper")
		}
		return installed, startPromotedHelper(ctx, cfg, installed)
	}
	if err := stableHelperProcess(ctx, cfg, installed); err == nil {
		return installed, nil
	}
	if err := restartHelper(ctx, cfg); err != nil {
		return installed, err
	}
	return installed, awaitStableHelper(ctx, cfg, installed)
}

func stableHelperProcess(ctx context.Context, cfg HelperConfig, installed HelperInstallation) error {
	pid, err := cfg.Probe(ctx, installed)
	if err != nil || pid <= 0 {
		return errors.Join(errors.New("helper has no verified running process"), err)
	}
	confirmed, err := cfg.Probe(ctx, installed)
	if err != nil || confirmed != pid {
		return errors.Join(errors.New("helper process changed before readiness"), err)
	}
	return nil
}

func awaitStableHelper(ctx context.Context, cfg HelperConfig, installed HelperInstallation) error {
	ctx, cancel := context.WithTimeout(ctx, helperReadinessTimeout)
	defer cancel()
	_, err := awaitRecoveryHelper(ctx, func(ctx context.Context, installed HelperInstallation) (int, error) {
		if err := stableHelperProcess(ctx, cfg, installed); err != nil {
			return 0, err
		}
		return 1, nil
	}, installed)
	return err
}

func promoteVerifiedHelper(ctx context.Context, cfg HelperConfig, prior helperRecoverySnapshot, digest string) (HelperInstallation, error) {
	promotion := helperPromotion{Prior: prior.installation, Receipt: prior.receipt, Service: prior.service, Target: digest}
	data, err := json.Marshal(promotion)
	if err != nil {
		return prior.installation, fmt.Errorf("encode helper promotion: %w", err)
	}
	if err = atomicWrite(helperPromotionPath(cfg.StateDir), data, 0600); err != nil {
		return prior.installation, err
	}
	installed, err := prepareHelper(ctx, cfg, true, digest)
	if err == nil {
		err = startPromotedHelper(ctx, cfg, installed)
	}
	if err != nil {
		return prior.installation, errors.Join(err, restorePromotedHelper(ctx, cfg, promotion))
	}
	return installed, clearHelperPromotion(cfg.StateDir)
}

func startPromotedHelper(ctx context.Context, cfg HelperConfig, installed HelperInstallation) error {
	if err := restartHelper(ctx, cfg); err != nil {
		return err
	}
	if err := awaitStableHelper(ctx, cfg, installed); err != nil {
		return err
	}
	if err := os.Remove(helperActivationRecord(cfg.StateDir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear verified helper activation: %w", err)
	}
	return syncDirectory(filepath.Dir(helperActivationRecord(cfg.StateDir)))
}

func clearHelperPromotion(stateDir string) error {
	if err := os.Remove(helperPromotionPath(stateDir)); err != nil {
		return fmt.Errorf("clear helper promotion: %w", err)
	}
	return syncDirectory(filepath.Dir(helperPromotionPath(stateDir)))
}

func resumeHelperPromotion(ctx context.Context, cfg HelperConfig) error {
	data, err := readMetadata(ctx, cfg.StateDir, filepath.Join("update", "helper", "promotion.json"), true)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var promotion helperPromotion
	if err = readJSON(data, &promotion); err != nil {
		return err
	}
	if err = validateHelperPromotion(ctx, cfg, promotion); err != nil {
		return err
	}
	var installed HelperInstallation
	if err = readHelperInstallation(ctx, cfg.StateDir, &installed); err != nil {
		return err
	}
	if installed == promotion.Prior {
		return restorePromotedHelper(ctx, cfg, promotion)
	}
	if installed.Digest != promotion.Target || installed.ServicePath != promotion.Prior.ServicePath {
		return errors.New("interrupted helper promotion differs from its retained target")
	}
	if _, err = verifyHelperReceipt(ctx, cfg.StateDir, installed, promotion.Target); err != nil {
		return err
	}
	if err = replaceHelperLink(filepath.Join(transactionDir(cfg.StateDir), "helper", "current"), installed.Executable); err == nil {
		err = checkHelperJournal(ctx, cfg.StateDir, installed.Executable)
	}
	if err == nil {
		err = startPromotedHelper(ctx, cfg, installed)
	}
	if err != nil {
		return errors.Join(err, restorePromotedHelper(ctx, cfg, promotion))
	}
	return clearHelperPromotion(cfg.StateDir)
}

func validateHelperPromotion(ctx context.Context, cfg HelperConfig, promotion helperPromotion) error {
	prior := promotion.Prior
	if !helperDigestPattern.MatchString(promotion.Target) || !helperDigestPattern.MatchString(prior.Digest) ||
		prior.Executable != filepath.Join(transactionDir(cfg.StateDir), "helper", prior.Digest, "mesh") {
		return errors.New("invalid retained helper promotion image")
	}
	var receipt HelperInstallation
	if err := readJSON(promotion.Receipt, &receipt); err != nil || receipt != prior {
		return errors.Join(errors.New("retained helper promotion receipt differs"), err)
	}
	service, name, err := helperService(cfg, filepath.Join(transactionDir(cfg.StateDir), "helper", "current"))
	if err != nil {
		return err
	}
	if prior.ServicePath != filepath.Join(cfg.ServiceDir, name) || string(promotion.Service) != service {
		return errors.New("retained helper promotion service differs")
	}
	if err = release.VerifyExecutable(ctx, prior.Executable, prior.Digest); err != nil {
		return fmt.Errorf("verify prior promotion helper: %w", err)
	}
	return nil
}

func restorePromotedHelper(ctx context.Context, cfg HelperConfig, promotion helperPromotion) error {
	engine := Engine{cfg: Config{StateDir: cfg.StateDir, HealthTimeout: helperReadinessTimeout}}
	request := HelperRecovery{Helper: cfg, Probe: cfg.Probe}
	prior := helperRecoverySnapshot{installation: promotion.Prior, receipt: promotion.Receipt, service: promotion.Service}
	if err := engine.restoreRecoveryHelper(ctx, request, prior); err != nil {
		return fmt.Errorf("restore prior helper after promotion: %w", err)
	}
	return clearHelperPromotion(cfg.StateDir)
}

type helperMetadataCapture struct {
	data     []byte
	cancel   context.CancelFunc
	overflow bool
}

func (c *helperMetadataCapture) Write(data []byte) (int, error) {
	if len(data) > helperMetadataLimit-len(c.data) {
		c.overflow = true
		c.cancel()
		return 0, errors.New("helper build report exceeds size limit")
	}
	c.data = append(c.data, data...)
	return len(data), nil
}

func helperCommandOutput(ctx context.Context, executable string, args ...string) ([]byte, error) {
	probe, cancel := context.WithCancel(ctx)
	defer cancel()
	capture := &helperMetadataCapture{data: make([]byte, 0, helperMetadataLimit), cancel: cancel}
	command := exec.CommandContext(probe, executable, args...) //nolint:gosec // verified local helper image and fixed build/journal probe arguments
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.WaitDelay = 50 * time.Millisecond
	command.Stdout, command.Stderr = capture, capture
	command.Cancel = func() error {
		err := unix.Kill(-command.Process.Pid, unix.SIGKILL)
		if errors.Is(err, unix.ESRCH) {
			return os.ErrProcessDone
		}
		if err != nil {
			return fmt.Errorf("stop helper build probe: %w", err)
		}
		return nil
	}
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start helper build probe: %w", err)
	}
	defer func() { _ = unix.Kill(-command.Process.Pid, unix.SIGKILL) }()
	err := command.Wait()
	if capture.overflow {
		return nil, errors.New("helper build report exceeds size limit")
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("helper build probe: %w", ctx.Err())
	}
	if err != nil {
		return nil, fmt.Errorf("helper build probe: %w", err)
	}
	return capture.data, nil
}
