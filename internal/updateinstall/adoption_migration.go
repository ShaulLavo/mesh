package updateinstall

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/updategate"
)

type AdoptionMigration struct {
	Operation    string
	Pin          string
	Generation   uint64
	Prior        Settings
	Next         Settings
	Original     Health
	Definition   string
	Before       string
	After        string
	Content      []byte
	ImageExisted bool
	Probe        Probe
	ManagedProbe Probe
	NativeSource func(context.Context) (bool, error)
	Authorize    func(context.Context, func() error) error
}

func PrepareManagedAdoption(ctx context.Context, migration AdoptionMigration) error {
	lock, err := lockInstallation(ctx, migration.Prior.StateDir)
	if err != nil {
		return err
	}
	defer unlock(lock)
	if err := validateAdoptionMigration(ctx, migration); err != nil {
		return err
	}
	if err := release.VerifyDigest(migration.Content, migration.After); err != nil {
		return fmt.Errorf("verify approved migration definition: %w", err)
	}
	err = migration.Authorize(ctx, func() error {
		if err := ownMigrationGate(migration); err != nil {
			return err
		}
		return stageManagedAdoption(ctx, migration)
	})
	if err != nil {
		return errors.Join(err, updategate.Clear(migration.Prior.StateDir, migration.Operation))
	}
	return nil
}

func validateAdoptionMigration(ctx context.Context, migration AdoptionMigration) error {
	if !validAdoptionHelperID(migration.Operation) || !helperDigestPattern.MatchString(migration.Pin) || migration.Authorize == nil || migration.NativeSource == nil || migration.Probe == nil || migration.ManagedProbe == nil {
		return errors.New("managed migration requires canonical adoption and native process approval")
	}
	if !filepath.IsAbs(migration.Definition) || !filepath.IsAbs(migration.Next.Executable) || migration.Next.StateDir != migration.Prior.StateDir || migration.Prior.Service != migration.Next.Service {
		return errors.New("managed migration must preserve state and native service identity")
	}
	expected := migration.Prior
	expected.Executable = migration.Next.Executable
	if migration.Next != expected || !helperDigestPattern.MatchString(migration.After) {
		return errors.New("managed migration changes settings beyond the executable")
	}
	if err := validateUnmanagedPrefix(migration.Next.Executable); err != nil {
		return err
	}
	if migration.Prior.HealthTimeout <= 0 || migration.Prior.HealthTimeout > 5*time.Minute {
		return errors.New("managed migration requires the existing bounded health timeout")
	}
	prior, err := ReadContext(ctx, migration.Prior.StateDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil && (!finished(prior.Phase) || prior.Phase == RollbackFailed || prior.Settings != migration.Prior) {
		return errors.New("another installation journal owns managed migration")
	}
	if err := release.VerifyExecutable(ctx, migration.Prior.Executable, migration.Original.Build.Digest); err != nil {
		return fmt.Errorf("verify original package image: %w", err)
	}
	return nil
}

func migrationAssetPath(migration AdoptionMigration, name string) string {
	return filepath.Join(migration.Prior.StateDir, "updates", "adoption", migration.Operation, name)
}

func stageManagedAdoption(ctx context.Context, migration AdoptionMigration) error {
	if running, err := migration.NativeSource(ctx); err != nil || !running {
		return errors.Join(errors.New("managed preparation requires its actual running old daemon"), err)
	}
	health, err := migration.Probe(ctx)
	if err != nil || len(health.Workers) != len(migration.Original.Workers) {
		return errors.Join(errors.New("worker set changed before managed migration gate"), err)
	}
	if err := preservedMigrationHealth(migration.Original, health); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(migration.Next.Executable), 0700); err != nil {
		return fmt.Errorf("create managed executable directory: %w", err)
	}
	if err := prepareManagedImage(ctx, migration); err != nil {
		return err
	}
	if migration.Before != "absent" {
		data, err := migrationDefinition(migration.Definition)
		if err != nil {
			return err
		}
		if err := release.VerifyDigest(data, migration.Before); err != nil {
			return fmt.Errorf("verify original migration service: %w", err)
		}
		if err := writeMigrationAsset(migration, "original-service", data, migration.Before); err != nil {
			return err
		}
	}
	return writeMigrationAsset(migration, "managed-service", migration.Content, migration.After)
}

func prepareManagedImage(ctx context.Context, migration AdoptionMigration) error {
	info, err := os.Lstat(migration.Next.Executable)
	if err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("managed destination changed to a nonregular file")
		}
		if err := release.VerifyExecutable(ctx, migration.Next.Executable, migration.Original.Build.Digest); err != nil {
			return fmt.Errorf("verify existing managed image: %w", err)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect managed executable destination: %w", err)
	}
	return durableCopy(ctx, migration.Prior.Executable, migration.Next.Executable, migration.Original.Build.Digest)
}

func writeMigrationAsset(migration AdoptionMigration, name string, data []byte, digest string) error {
	path := migrationAssetPath(migration, name)
	prior, err := migrationDefinition(path)
	if err == nil {
		if err := release.VerifyDigest(prior, digest); err != nil {
			return fmt.Errorf("migration backup belongs to another writer: %w", err)
		}
		return nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return atomicWrite(path, data, 0600)
}

func migrationDefinition(path string) ([]byte, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("open migration definition directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile(filepath.Base(path))
	if err != nil || len(data) > 1<<20 {
		return nil, errors.Join(errors.New("read bounded migration definition"), err)
	}
	return data, nil
}

func ApplyManagedAdoption(ctx context.Context, migration AdoptionMigration) error {
	lock, err := lockInstallation(ctx, migration.Prior.StateDir)
	if err != nil {
		return err
	}
	defer unlock(lock)
	if err := validateAdoptionMigration(ctx, migration); err != nil {
		return err
	}
	if err := release.VerifyExecutable(ctx, migration.Next.Executable, migration.Original.Build.Digest); err != nil {
		return fmt.Errorf("verify preserved managed old image: %w", err)
	}
	if err := migration.Authorize(ctx, func() error {
		if err := ownMigrationGate(migration); err != nil {
			return err
		}
		return publishManagedDefinition(ctx, migration, true)
	}); err != nil {
		return err
	}
	if err := restartManagedAdoption(ctx, migration, true); err != nil {
		return errors.Join(err, restoreManagedAdoption(ctx, migration))
	}
	return nil
}

func ownMigrationGate(migration AdoptionMigration) error {
	var updating *updategate.UpdatingError
	err := updategate.Check(migration.Prior.StateDir)
	if err != nil && (!errors.As(err, &updating) || updating.Operation != migration.Operation) {
		return fmt.Errorf("another update creation gate owns migration: %w", err)
	}
	if err := updategate.Set(migration.Prior.StateDir, migration.Operation); err != nil {
		return fmt.Errorf("fence managed migration worker creation: %w", err)
	}
	return nil
}

func CompleteManagedAdoption(ctx context.Context, migration AdoptionMigration) error {
	lock, err := lockInstallation(ctx, migration.Prior.StateDir)
	if err != nil {
		return err
	}
	defer unlock(lock)
	journal, err := ReadContext(ctx, migration.Prior.StateDir)
	if err == nil && journal.Request.ID == migration.Operation && journal.Request.Generation == migration.Generation {
		return nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return migration.Authorize(ctx, func() error {
		if err := updategate.Clear(migration.Prior.StateDir, migration.Operation); err != nil {
			return fmt.Errorf("clear completed managed migration gate: %w", err)
		}
		return nil
	})
}

func publishManagedDefinition(ctx context.Context, migration AdoptionMigration, forward bool) error {
	if _, err := migration.NativeSource(ctx); err != nil {
		return err
	}
	name, digest := "managed-service", migration.After
	if !forward {
		name, digest = "original-service", migration.Before
	}
	if !forward && migration.Before == "absent" {
		if err := os.Remove(migration.Definition); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove owned migration drop-in: %w", err)
		}
		return syncDirectory(filepath.Dir(migration.Definition))
	}
	data, err := migrationDefinition(migrationAssetPath(migration, name))
	if err != nil {
		return err
	}
	if err := release.VerifyDigest(data, digest); err != nil {
		return fmt.Errorf("verify migration service bytes: %w", err)
	}
	return atomicWrite(migration.Definition, data, 0600)
}

func restartManagedAdoption(ctx context.Context, migration AdoptionMigration, forward bool) error {
	if migration.Next.Service.Kind == promotionSystemd {
		if _, err := runCommand(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
			return err
		}
	}
	running, err := migration.NativeSource(ctx)
	if err != nil {
		return err
	}
	service, err := NewSystemService(migration.Next.Service)
	if err != nil {
		return err
	}
	if running {
		if err := service.Stop(ctx); err != nil {
			return err
		}
	}
	if err := service.Start(ctx); err != nil {
		return err
	}
	return awaitMigrationSource(ctx, migration, forward)
}

func awaitMigrationSource(ctx context.Context, migration AdoptionMigration, forward bool) error {
	ctx, cancel := context.WithTimeout(ctx, migration.Prior.HealthTimeout)
	defer cancel()
	probe := migration.Probe
	if forward {
		probe = migration.ManagedProbe
	}
	for ctx.Err() == nil {
		health, err := probe(ctx)
		if err == nil && preservedMigrationHealth(migration.Original, health) == nil {
			return nil
		}
		if err := waitContext(ctx, 50*time.Millisecond); err != nil {
			return err
		}
	}
	return fmt.Errorf("verify same old image and workers after service migration: %w", ctx.Err())
}

func preservedMigrationHealth(original, actual Health) error {
	if original.HostID != actual.HostID || original.Build.Digest != actual.Build.Digest || original.BootID != actual.BootID {
		return errors.New("managed migration changed the daemon identity, old image or boot")
	}
	for _, worker := range original.Workers {
		if !migrationWorkerPresent(worker, actual.Workers) {
			return errors.New("managed migration lost a worker or shell process")
		}
	}
	return nil
}

func migrationWorkerPresent(original Worker, workers []Worker) bool {
	for _, worker := range workers {
		if original.ID == worker.ID && original.PID == worker.PID && original.ShellPID == worker.ShellPID {
			return true
		}
	}
	return false
}

func restoreManagedAdoption(ctx context.Context, migration AdoptionMigration) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), migration.Prior.HealthTimeout)
	defer cancel()
	if err := publishManagedDefinition(ctx, migration, false); err != nil {
		return fmt.Errorf("restore original daemon service definition: %w", err)
	}
	if err := restartManagedAdoption(ctx, migration, false); err != nil {
		return fmt.Errorf("restore original daemon service process: %w", err)
	}
	if !migration.ImageExisted {
		if err := release.VerifyExecutable(ctx, migration.Next.Executable, migration.Original.Build.Digest); err != nil {
			return fmt.Errorf("verify owned managed image before cleanup: %w", err)
		}
		if err := os.Remove(migration.Next.Executable); err != nil {
			return fmt.Errorf("remove owned managed image after restoration: %w", err)
		}
	}
	if err := updategate.Clear(migration.Prior.StateDir, migration.Operation); err != nil {
		return fmt.Errorf("clear restored managed migration gate: %w", err)
	}
	return nil
}
