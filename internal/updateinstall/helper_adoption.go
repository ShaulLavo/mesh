package updateinstall

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/shaul/mesh/internal/release"
)

type AdoptionHelperApproval struct {
	Operation string
	Pin       string
	Authorize func(context.Context, func() error) error
}

type adoptionHelperPreparation struct {
	Schema       int                `json:"schema"`
	Operation    string             `json:"operation"`
	Pin          string             `json:"pin"`
	Digest       string             `json:"digest"`
	Prior        HelperInstallation `json:"prior"`
	Receipt      []byte             `json:"receipt,omitempty"`
	Service      []byte             `json:"service,omitempty"`
	PriorRunning bool               `json:"priorRunning"`
	ImageExisted bool               `json:"imageExisted"`
	Complete     bool               `json:"complete"`
}

// PrepareAdoptionHelper is restricted to an explicitly approved unjournaled
// adoption. Ordinary promotion still requires its finished-journal guard.
func PrepareAdoptionHelper(ctx context.Context, cfg HelperConfig, source release.Manifest, digest string, approval AdoptionHelperApproval) (HelperInstallation, error) {
	if approval.Authorize == nil || !validAdoptionHelperID(approval.Operation) || !helperDigestPattern.MatchString(approval.Pin) || cfg.Probe == nil || cfg.State == nil {
		return HelperInstallation{}, errors.New("initial adoption helper requires canonical approval and a mapped-process probe")
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
	if err := noFreshHelperPreparation(ctx, cfg.StateDir); err != nil {
		return HelperInstallation{}, err
	}
	if err := noAdoptionJournal(ctx, cfg.StateDir); err != nil {
		return HelperInstallation{}, err
	}
	if err := verifyInitialAdoptionImage(ctx, cfg, source, digest); err != nil {
		return HelperInstallation{}, err
	}
	if err := approval.Authorize(ctx, func() error { return nil }); err != nil {
		return HelperInstallation{}, fmt.Errorf("authorize initial adoption helper: %w", err)
	}
	record, err := readAdoptionHelperPreparation(ctx, cfg, approval, digest)
	if err != nil {
		return HelperInstallation{}, err
	}
	if err := checkInitialAdoptionOwnership(ctx, cfg, record); err != nil {
		return HelperInstallation{}, err
	}
	if err := adoptionHelperForward(ctx, cfg, record); err != nil {
		return HelperInstallation{}, err
	}
	return runAdoptionHelperPreparation(ctx, cfg, approval, record)
}

func validAdoptionHelperID(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' && character < 'a' || character > 'f' {
			return false
		}
	}
	return true
}

func noAdoptionJournal(ctx context.Context, stateDir string) error {
	_, err := ReadContext(ctx, stateDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return errors.Join(errors.New("initial adoption helper requires no competing installation journal"), err)
}

func verifyInitialAdoptionImage(ctx context.Context, cfg HelperConfig, source release.Manifest, digest string) error {
	if !filepath.IsAbs(cfg.StateDir) || !filepath.IsAbs(cfg.ServiceDir) || !filepath.IsAbs(cfg.Executable) {
		return errors.New("initial adoption helper requires absolute owned paths")
	}
	if err := source.Validate(); err != nil {
		return fmt.Errorf("verify initial helper manifest: %w", err)
	}
	artifact, err := source.Artifact(release.CurrentPlatform())
	if err != nil || artifact.BinarySHA256 != digest {
		return errors.Join(errors.New("initial adoption helper differs from the approved artifact"), err)
	}
	if err := release.VerifyExecutable(ctx, cfg.Executable, digest); err != nil {
		return fmt.Errorf("verify initial adoption helper executable: %w", err)
	}
	build, err := helperBuild(ctx, cfg.Executable, digest)
	if err != nil {
		return err
	}
	if build.Version != source.Version || build.Commit != source.Commit || build.Modified || build.StateVersion != source.Compatibility.StateWrite || build.WorkerProtocol != source.Compatibility.WorkerWrite || source.Compatibility.JournalVersion != release.CurrentJournalVersion {
		return errors.New("initial helper build differs from the verified release")
	}
	return nil
}

func adoptionHelperPreparationPath(stateDir string) string {
	return filepath.Join(transactionDir(stateDir), "helper", "adoption-preparation.json")
}

func readAdoptionHelperPreparation(ctx context.Context, cfg HelperConfig, approval AdoptionHelperApproval, digest string) (adoptionHelperPreparation, error) {
	var record adoptionHelperPreparation
	data, err := readMetadata(ctx, cfg.StateDir, filepath.Join("update", "helper", "adoption-preparation.json"), true)
	if err == nil {
		if err := readJSON(data, &record); err != nil {
			return record, err
		}
		if record.Schema != 1 || record.Operation != approval.Operation || record.Pin != approval.Pin || record.Digest != digest {
			return record, errors.New("another adoption owns initial helper preparation")
		}
		return record, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return record, err
	}
	record = adoptionHelperPreparation{Schema: 1, Operation: approval.Operation, Pin: approval.Pin, Digest: digest}
	if err := captureAdoptionHelperPrior(ctx, cfg, &record); err != nil {
		return record, err
	}
	image := filepath.Join(transactionDir(cfg.StateDir), "helper", digest, "mesh")
	_, err = os.Lstat(image)
	record.ImageExisted = err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return record, fmt.Errorf("inspect initial helper image destination: %w", err)
	}
	data, err = json.Marshal(record)
	if err != nil {
		return record, fmt.Errorf("encode initial helper preparation: %w", err)
	}
	err = approval.Authorize(ctx, func() error { return atomicWrite(adoptionHelperPreparationPath(cfg.StateDir), data, 0600) })
	return record, err
}

func captureAdoptionHelperPrior(ctx context.Context, cfg HelperConfig, record *adoptionHelperPreparation) error {
	state, err := cfg.State(ctx)
	if err != nil {
		return err
	}
	prior, err := recoveryHelperSnapshot(ctx, cfg)
	if err == nil {
		record.Prior, record.Receipt, record.Service = prior.installation, prior.receipt, prior.service
		record.PriorRunning, err = priorAdoptionHelperRunning(ctx, cfg, prior.installation, state)
		return err
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var existing HelperInstallation
	if receiptErr := readHelperInstallation(ctx, cfg.StateDir, &existing); !errors.Is(receiptErr, os.ErrNotExist) {
		return errors.Join(errors.New("existing unjournaled helper is incomplete; refusing an unrelated replacement"), receiptErr)
	}
	if state.Loaded || state.PID != 0 {
		return errors.New("existing helper manager unit has no managed receipt")
	}
	service, name, err := helperService(cfg, filepath.Join(transactionDir(cfg.StateDir), "helper", "current"))
	if err != nil {
		return err
	}
	_, readErr := readMetadata(ctx, cfg.ServiceDir, name, false)
	if !errors.Is(readErr, os.ErrNotExist) {
		return errors.Join(errors.New("unjournaled helper service has no managed receipt"), readErr)
	}
	if _, err := os.Lstat(filepath.Join(transactionDir(cfg.StateDir), "helper", "current")); !errors.Is(err, os.ErrNotExist) {
		return errors.Join(errors.New("unjournaled helper launcher has no managed receipt"), err)
	}
	record.Service = []byte(service)
	return nil
}

func priorAdoptionHelperRunning(ctx context.Context, cfg HelperConfig, installed HelperInstallation, state HelperProcessState) (bool, error) {
	if state.PID == 0 {
		return false, nil
	}
	pid, err := cfg.Probe(ctx, installed)
	if err != nil || pid != state.PID {
		return false, errors.Join(errors.New("existing helper manager process differs from its mapped image"), err)
	}
	return true, nil
}

func runAdoptionHelperPreparation(ctx context.Context, cfg HelperConfig, approval AdoptionHelperApproval, record adoptionHelperPreparation) (HelperInstallation, error) {
	installed, currentErr := currentAdoptionHelper(ctx, cfg, record)
	if currentErr == nil {
		if pid, err := cfg.Probe(ctx, installed); err == nil && pid > 0 {
			return completeAdoptionHelperPreparation(cfg, record, installed)
		}
	}
	if record.Complete && currentErr != nil {
		return installed, currentErr
	}
	cfg.adoptionInitial = true
	cfg.beforePromotion = func(ctx context.Context) error {
		return approval.Authorize(ctx, func() error { return noAdoptionJournal(ctx, cfg.StateDir) })
	}
	installed, err := prepareHelper(ctx, cfg, true, record.Digest)
	if err == nil {
		err = startInitialAdoptionHelper(ctx, cfg, installed, record.Prior.Digest != "")
	}
	if err != nil {
		return HelperInstallation{}, errors.Join(err, restoreInitialAdoptionHelper(ctx, cfg, record))
	}
	return completeAdoptionHelperPreparation(cfg, record, installed)
}

func completeAdoptionHelperPreparation(cfg HelperConfig, record adoptionHelperPreparation, installed HelperInstallation) (HelperInstallation, error) {
	if record.Complete {
		return installed, nil
	}
	record.Complete = true
	data, err := json.Marshal(record)
	if err != nil {
		return installed, fmt.Errorf("encode completed initial helper preparation: %w", err)
	}
	if err := atomicWrite(adoptionHelperPreparationPath(cfg.StateDir), data, 0600); err != nil {
		return installed, err
	}
	return installed, nil
}

func adoptionHelperForward(ctx context.Context, cfg HelperConfig, record adoptionHelperPreparation) error {
	if record.Prior.Digest == "" {
		return nil
	}
	prior, err := helperBuild(ctx, record.Prior.Executable, record.Prior.Digest)
	if err != nil {
		return err
	}
	candidate, err := helperBuild(ctx, cfg.Executable, record.Digest)
	if err != nil {
		return err
	}
	_, err = helperPromotionOrder(candidate, prior, false)
	return err
}

func currentAdoptionHelper(ctx context.Context, cfg HelperConfig, record adoptionHelperPreparation) (HelperInstallation, error) {
	snapshot, err := recoveryHelperSnapshot(ctx, cfg)
	if err != nil {
		return HelperInstallation{}, err
	}
	if snapshot.installation.Digest != record.Digest {
		return HelperInstallation{}, errors.New("initial adoption helper receipt was changed")
	}
	return snapshot.installation, nil
}

func startInitialAdoptionHelper(ctx context.Context, cfg HelperConfig, installed HelperInstallation, existing bool) error {
	if existing {
		return startPromotedHelper(ctx, cfg, installed)
	}
	if cfg.Kind == promotionLaunchd {
		if _, err := runCommand(ctx, "launchctl", "bootstrap", cfg.Domain, installed.ServicePath); err != nil {
			return err
		}
		return awaitStableHelper(ctx, cfg, installed)
	}
	if _, err := runCommand(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	if _, err := runCommand(ctx, "systemctl", "--user", "enable", "--now", "mesh-update-helper.service"); err != nil {
		return err
	}
	return awaitStableHelper(ctx, cfg, installed)
}

func restoreInitialAdoptionHelper(ctx context.Context, cfg HelperConfig, record adoptionHelperPreparation) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), helperReadinessTimeout)
	defer cancel()
	if err := checkInitialAdoptionOwnership(ctx, cfg, record); err != nil {
		return err
	}
	if err := stopOwnedInitialAdoptionHelper(ctx, cfg, record); err != nil {
		return err
	}
	if record.Prior.Digest != "" {
		return restoreInitialAdoptionPrior(ctx, cfg, record)
	}
	return removeInitialAdoptionHelper(ctx, cfg, record)
}

func stopOwnedInitialAdoptionHelper(ctx context.Context, cfg HelperConfig, record adoptionHelperPreparation) error {
	state, err := cfg.State(ctx)
	if err != nil || state.PID == 0 {
		return err
	}
	installed := HelperInstallation{Executable: filepath.Join(transactionDir(cfg.StateDir), "helper", record.Digest, "mesh"), Digest: record.Digest}
	pid, probeErr := cfg.Probe(ctx, installed)
	if probeErr != nil && record.Prior.Digest != "" {
		pid, probeErr = cfg.Probe(ctx, record.Prior)
	}
	if probeErr != nil || pid != state.PID {
		return errors.Join(errors.New("helper manager process changed; refusing unrelated cleanup"), probeErr)
	}
	return stopInitialAdoptionHelper(ctx, cfg)
}

func checkInitialAdoptionOwnership(ctx context.Context, cfg HelperConfig, record adoptionHelperPreparation) error {
	if err := checkInitialAdoptionService(ctx, cfg, record); err != nil {
		return err
	}
	if err := checkInitialAdoptionReceipt(ctx, cfg, record); err != nil {
		return err
	}
	link, err := os.Readlink(filepath.Join(transactionDir(cfg.StateDir), "helper", "current"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect initial helper launcher ownership: %w", err)
	}
	want := filepath.Join(transactionDir(cfg.StateDir), "helper", record.Digest, "mesh")
	if err == nil && link != want && link != record.Prior.Executable {
		return errors.New("initial helper launcher changed; refusing unrelated cleanup")
	}
	if _, err := os.Lstat(want); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := release.VerifyExecutable(ctx, want, record.Digest); err != nil {
		return fmt.Errorf("verify owned initial helper image: %w", err)
	}
	return nil
}

func checkInitialAdoptionService(ctx context.Context, cfg HelperConfig, record adoptionHelperPreparation) error {
	_, name, err := helperService(cfg, filepath.Join(transactionDir(cfg.StateDir), "helper", "current"))
	if err != nil {
		return err
	}
	service, err := readMetadata(ctx, cfg.ServiceDir, name, false)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil && !bytes.Equal(service, record.Service) {
		return errors.New("initial helper service changed; refusing to remove another writer's service")
	}
	return nil
}

func checkInitialAdoptionReceipt(ctx context.Context, cfg HelperConfig, record adoptionHelperPreparation) error {
	var installed HelperInstallation
	err := readHelperInstallation(ctx, cfg.StateDir, &installed)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil && installed.Digest != record.Digest && installed != record.Prior {
		return errors.New("initial helper receipt changed; refusing unrelated cleanup")
	}
	return nil
}

func stopInitialAdoptionHelper(ctx context.Context, cfg HelperConfig) error {
	if cfg.Kind == promotionLaunchd {
		_, err := runCommand(ctx, "launchctl", "bootout", cfg.Domain+"/dev.shaulavo.mesh-update-helper")
		return err
	}
	_, err := runCommand(ctx, "systemctl", "--user", "stop", "mesh-update-helper.service")
	return err
}

func restoreInitialAdoptionPrior(ctx context.Context, cfg HelperConfig, record adoptionHelperPreparation) error {
	if err := replaceHelperLink(filepath.Join(transactionDir(cfg.StateDir), "helper", "current"), record.Prior.Executable); err != nil {
		return err
	}
	if err := atomicWrite(record.Prior.ServicePath, record.Service, 0644); err != nil {
		return err
	}
	if err := atomicWrite(helperRecord(cfg.StateDir), record.Receipt, 0600); err != nil {
		return err
	}
	if record.PriorRunning {
		if err := restartHelper(ctx, cfg); err != nil {
			return err
		}
		if err := awaitStableHelper(ctx, cfg, record.Prior); err != nil {
			return err
		}
	}
	return removeInitialAdoptionCheckpoint(cfg, record)
}

func removeInitialAdoptionHelper(ctx context.Context, cfg HelperConfig, record adoptionHelperPreparation) error {
	_, name, err := helperService(cfg, filepath.Join(transactionDir(cfg.StateDir), "helper", "current"))
	if err != nil {
		return err
	}
	if err := deactivateInitialAdoptionHelper(ctx, cfg); err != nil {
		return err
	}
	for _, path := range []string{filepath.Join(cfg.ServiceDir, name), helperRecord(cfg.StateDir), filepath.Join(transactionDir(cfg.StateDir), "helper", "current")} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove owned initial helper resource: %w", err)
		}
	}
	return removeInitialAdoptionCheckpoint(cfg, record)
}

func deactivateInitialAdoptionHelper(ctx context.Context, cfg HelperConfig) error {
	if cfg.Kind == promotionSystemd {
		_, err := runCommand(ctx, "systemctl", "--user", "disable", "--now", "mesh-update-helper.service")
		return err
	}
	state, err := cfg.State(ctx)
	if err != nil || !state.Loaded {
		return err
	}
	return stopInitialAdoptionHelper(ctx, cfg)
}

func removeInitialAdoptionCheckpoint(cfg HelperConfig, record adoptionHelperPreparation) error {
	if !record.ImageExisted {
		path := filepath.Join(transactionDir(cfg.StateDir), "helper", record.Digest, "mesh")
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove owned initial helper image: %w", err)
		}
		if err := os.Remove(filepath.Dir(path)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove owned initial helper image directory: %w", err)
		}
	}
	for _, path := range []string{adoptionHelperPreparationPath(cfg.StateDir), helperActivationRecord(cfg.StateDir)} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove owned initial helper checkpoint: %w", err)
		}
	}
	return syncDirectory(filepath.Join(transactionDir(cfg.StateDir), "helper"))
}
