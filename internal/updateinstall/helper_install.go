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

const freshHelperCheckpoint = "fresh-installation.json"

// EnsureInstalledHelper accepts a caller-authenticated release for a new managed
// installation. Existing transaction journals retain the ordinary promotion guard.
func EnsureInstalledHelper(ctx context.Context, cfg HelperConfig, source release.Manifest) (HelperInstallation, error) {
	if err := ValidateInstallationPath(cfg.Executable); err != nil {
		return HelperInstallation{}, err
	}
	_, err := ReadContext(ctx, cfg.StateDir)
	if err == nil {
		return EnsureCurrentHelper(ctx, cfg, source)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return HelperInstallation{}, err
	}
	if cfg.Probe == nil || cfg.State == nil {
		return HelperInstallation{}, errors.New("initial helper installation requires native process and manager probes")
	}
	if cfg.ServiceDir == "" {
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
	if err := noFreshHelperConflict(ctx, cfg); err != nil {
		return HelperInstallation{}, err
	}
	artifact, err := source.Artifact(release.CurrentPlatform())
	if err != nil {
		return HelperInstallation{}, fmt.Errorf("read initial helper artifact: %w", err)
	}
	if err := verifyInitialAdoptionImage(ctx, cfg, source, artifact.BinarySHA256); err != nil {
		return HelperInstallation{}, err
	}
	return installFreshHelper(ctx, cfg, source, artifact.BinarySHA256)
}

func noFreshHelperConflict(ctx context.Context, cfg HelperConfig) error {
	if err := noAdoptionJournal(ctx, cfg.StateDir); err != nil {
		return err
	}
	for _, name := range []string{filepath.Join("updates", "host-adoption.json"), filepath.Join("update", "helper", "adoption-preparation.json")} {
		_, err := readMetadata(ctx, cfg.StateDir, name, true)
		if !errors.Is(err, os.ErrNotExist) {
			return errors.Join(errors.New("adoption owns helper installation"), err)
		}
	}
	return nil
}

func freshHelperPath(stateDir string) string {
	return filepath.Join(transactionDir(stateDir), "helper", freshHelperCheckpoint)
}

func noFreshHelperPreparation(ctx context.Context, stateDir string) error {
	_, err := readMetadata(ctx, stateDir, filepath.Join("update", "helper", freshHelperCheckpoint), true)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return errors.Join(errors.New("unfinished fresh installation owns helper preparation"), err)
}

func installFreshHelper(ctx context.Context, cfg HelperConfig, source release.Manifest, digest string) (HelperInstallation, error) {
	record, err := freshHelperPreparation(ctx, cfg, digest)
	if err != nil {
		return HelperInstallation{}, err
	}
	if record.Prior.Digest != "" {
		return retainInitialHelper(ctx, cfg, source, record.Prior)
	}
	cfg.freshInstall = true
	cfg.beforePromotion = func(ctx context.Context) error { return noFreshHelperConflict(ctx, cfg) }
	installed, err := prepareHelper(ctx, cfg, true, digest)
	if err == nil {
		err = startFreshHelper(ctx, cfg, installed)
	}
	if err != nil {
		return installed, errors.Join(err, rollbackFreshHelper(ctx, cfg, record))
	}
	if err := os.Remove(helperActivationRecord(cfg.StateDir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return installed, fmt.Errorf("clear initial helper activation: %w", err)
	}
	return installed, removeFreshHelperCheckpoint(cfg.StateDir)
}

// The snapshot type also describes an initial adoption's owned helper resources.
// This checkpoint has its own path and never creates adoption authority.
func freshHelperPreparation(ctx context.Context, cfg HelperConfig, digest string) (adoptionHelperPreparation, error) {
	var record adoptionHelperPreparation
	data, err := readMetadata(ctx, cfg.StateDir, filepath.Join("update", "helper", freshHelperCheckpoint), true)
	if err == nil {
		if err := readJSON(data, &record); err != nil {
			return record, err
		}
		if record.Schema != 1 || record.Digest != digest || record.Prior.Digest != "" {
			return record, errors.New("initial helper checkpoint differs from this installation")
		}
		service, _, err := helperService(cfg, filepath.Join(transactionDir(cfg.StateDir), "helper", "current"))
		if err != nil || !bytes.Equal(record.Service, []byte(service)) {
			return record, errors.Join(errors.New("initial helper checkpoint service changed"), err)
		}
		return record, checkInitialAdoptionOwnership(ctx, cfg, record)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return record, err
	}
	record.Schema, record.Digest = 1, digest
	if err := captureAdoptionHelperPrior(ctx, cfg, &record); err != nil {
		return record, err
	}
	if record.Prior.Digest != "" {
		return record, nil
	}
	image := filepath.Join(transactionDir(cfg.StateDir), "helper", digest, "mesh")
	_, err = os.Lstat(image)
	record.ImageExisted = err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return record, fmt.Errorf("inspect initial helper image: %w", err)
	}
	data, err = json.Marshal(record)
	if err != nil {
		return record, fmt.Errorf("encode initial helper checkpoint: %w", err)
	}
	return record, atomicWrite(freshHelperPath(cfg.StateDir), data, 0600)
}

func retainInitialHelper(ctx context.Context, cfg HelperConfig, source release.Manifest, installed HelperInstallation) (HelperInstallation, error) {
	build, err := helperBuild(ctx, installed.Executable, installed.Digest)
	if err != nil {
		return installed, err
	}
	order, err := release.CompareVersions(build.Version, source.Version)
	if err != nil || order < 0 {
		return installed, errors.Join(errors.New("older unjournaled helper requires adoption"), err)
	}
	if order == 0 {
		artifact, err := source.Artifact(release.CurrentPlatform())
		if err != nil || installed.Digest != artifact.BinarySHA256 {
			return installed, errors.Join(errors.New("initial helper differs from its release"), err)
		}
	}
	if err := stableHelperProcess(ctx, cfg, installed); err == nil {
		return installed, nil
	}
	state, err := cfg.State(ctx)
	if err != nil || state.PID != 0 {
		return installed, errors.Join(errors.New("existing helper process has an unverified image"), err)
	}
	if err := startInitialAdoptionHelper(ctx, cfg, installed, state.Loaded); err != nil {
		return installed, errors.Join(err, restoreStoppedInitialHelper(ctx, cfg, installed))
	}
	return installed, nil
}

func restoreStoppedInitialHelper(ctx context.Context, cfg HelperConfig, installed HelperInstallation) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), helperReadinessTimeout)
	defer cancel()
	state, err := cfg.State(ctx)
	if err != nil || state.PID == 0 {
		return err
	}
	pid, err := cfg.Probe(ctx, installed)
	if err != nil || pid != state.PID {
		return errors.Join(errors.New("existing helper process changed during failed start"), err)
	}
	if err := stopInitialAdoptionHelper(ctx, cfg); err != nil {
		return err
	}
	return verifyStoppedRecoveryHelper(ctx, cfg)
}

func startFreshHelper(ctx context.Context, cfg HelperConfig, installed HelperInstallation) error {
	state, err := cfg.State(ctx)
	if err != nil {
		return err
	}
	if state.PID != 0 {
		return stableHelperProcess(ctx, cfg, installed)
	}
	return startInitialAdoptionHelper(ctx, cfg, installed, state.Loaded)
}

func rollbackFreshHelper(ctx context.Context, cfg HelperConfig, record adoptionHelperPreparation) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), helperReadinessTimeout)
	defer cancel()
	if err := checkInitialAdoptionOwnership(ctx, cfg, record); err != nil {
		return err
	}
	if err := stopOwnedInitialAdoptionHelper(ctx, cfg, record); err != nil {
		return err
	}
	if err := deactivateInitialAdoptionHelper(ctx, cfg); err != nil {
		return err
	}
	_, name, err := helperService(cfg, filepath.Join(transactionDir(cfg.StateDir), "helper", "current"))
	if err != nil {
		return err
	}
	paths := []string{filepath.Join(cfg.ServiceDir, name), helperRecord(cfg.StateDir), filepath.Join(transactionDir(cfg.StateDir), "helper", "current"), helperActivationRecord(cfg.StateDir)}
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove failed initial helper: %w", err)
		}
	}
	if !record.ImageExisted {
		image := filepath.Join(transactionDir(cfg.StateDir), "helper", record.Digest, "mesh")
		if err := os.Remove(image); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove failed initial helper image: %w", err)
		}
	}
	return removeFreshHelperCheckpoint(cfg.StateDir)
}

func removeFreshHelperCheckpoint(stateDir string) error {
	if err := os.Remove(freshHelperPath(stateDir)); err != nil {
		return fmt.Errorf("clear initial helper checkpoint: %w", err)
	}
	return syncDirectory(filepath.Dir(freshHelperPath(stateDir)))
}
