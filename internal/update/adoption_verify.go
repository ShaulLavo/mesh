package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"syscall"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/updatebootstrap"
	"github.com/shaul/mesh/internal/updateinstall"
)

type AdoptionRelease struct {
	Pin       string
	Signature string
	Metadata  []byte
	Manifest  release.Manifest
	Published release.UpdateMetadata
}

func ReadAdoptionRelease(ctx context.Context, documents HostDocuments, version string) (AdoptionRelease, error) {
	if _, err := release.CompareVersions(version, version); err != nil {
		return AdoptionRelease{}, hostError(err)
	}
	metadata, err := documents.read(ctx, version, release.UpdateMetadataAssetName, release.MaxMetadataBytes)
	if err != nil {
		return AdoptionRelease{}, err
	}
	envelope, err := documents.read(ctx, version, "mesh-update.sig.json", release.MaxSignatureBytes)
	if err != nil {
		return AdoptionRelease{}, err
	}
	// Only authenticated metadata can select the page or bind executable bytes.
	if err := release.VerifySignature(documents.Trust, metadata, envelope); err != nil {
		return AdoptionRelease{}, hostError(err)
	}
	hint, err := release.DecodeUpdateMetadata(metadata)
	if err != nil {
		return AdoptionRelease{}, hostError(err)
	}
	manifest, err := documents.read(ctx, version, release.ManifestAssetName, release.DocumentLimit(release.ManifestDocument))
	if err != nil {
		return AdoptionRelease{}, err
	}
	page, err := documents.read(ctx, hint.Page.Tag, hint.Page.Name, release.MaxAnchorPageBytes)
	if err != nil {
		return AdoptionRelease{}, err
	}
	verified, err := release.VerifyMetadata(documents.Trust, metadata, envelope, manifest, page)
	if err != nil {
		return AdoptionRelease{}, hostError(err)
	}
	if verified.Version != version || verified.Updater == nil || verified.Updater.Epoch != HostReaderEpoch {
		return AdoptionRelease{}, errors.New("update: adoption release does not provide this updater reader epoch")
	}
	var frozen release.Manifest
	if err := decodeAuthority(manifest, &frozen); err != nil {
		return AdoptionRelease{}, err
	}
	if err := frozen.Validate(); err != nil {
		return AdoptionRelease{}, hostError(err)
	}
	return AdoptionRelease{Pin: discoveryDigest(metadata), Signature: discoveryDigest(envelope), Metadata: metadata, Manifest: frozen, Published: verified}, nil
}

func (r AdoptionRelease) verifySide(ctx context.Context, side string, build release.Build) error {
	artifact, err := r.Manifest.Artifact(build.Platform)
	if err != nil {
		return hostError(err)
	}
	if build.Modified || build.Version != r.Manifest.Version || build.Commit != r.Manifest.Commit || build.Digest != artifact.BinarySHA256 {
		return errors.New("update: adoption must execute the exact pinned side image")
	}
	if err := release.VerifyExecutable(ctx, side, artifact.BinarySHA256); err != nil {
		return hostError(err)
	}
	return nil
}

func (r AdoptionRelease) verifySource(ctx context.Context, documents HostDocuments, health updateinstall.Health) error {
	if health.Build.Modified || health.Build.Platform != release.CurrentPlatform() {
		return errors.New("update: adoption source has a modified or foreign-platform image")
	}
	artifact, err := r.Manifest.Artifact(health.Build.Platform)
	if err != nil {
		return hostError(err)
	}
	for _, transition := range r.Manifest.Compatibility.Transitions {
		if transition.FromDigest != health.Build.Digest || transition.ToDigest != artifact.BinarySHA256 || transition.Platform != health.Build.Platform {
			continue
		}
		next := release.PlannedRelease{Metadata: &r.Published, Position: r.Published.Position, Manifest: r.Manifest, Transition: transition}
		_, err := verifyHostHop(ctx, health.Build, health.Workers, next, r.Metadata, documents.Source())
		return err
	}
	return errors.New("update: adoption has no full forward and rollback proof for the observed source")
}

func adoptionSettings(ctx context.Context, stateDir string, observed updatebootstrap.Observation) (updateinstall.Settings, uint64, error) {
	status, err := updateinstall.ReadContext(ctx, stateDir)
	if err != nil {
		return updateinstall.Settings{}, 0, fmt.Errorf("update: adoption requires inspected installation settings: %w", err)
	}
	if !hostFinished(status.Phase) || status.Phase == updateinstall.RollbackFailed || status.Settings.ClientOnly {
		return status.Settings, 0, errors.New("update: adoption requires a finished daemon journal without failed rollback")
	}
	if status.Settings.StateDir != stateDir || status.Settings.Executable != observed.Executable || status.Request.Generation == ^uint64(0) {
		return status.Settings, 0, errors.New("update: adoption installation settings differ from the running daemon")
	}
	if err := updateinstall.ValidateInstallationPath(observed.Executable); err != nil {
		return status.Settings, 0, fmt.Errorf("update: adoption installation path: %w", err)
	}
	return status.Settings, status.Request.Generation + 1, nil
}

// SnapshotAdoptionFiles hashes presence as well as contents. It never reads or
// serializes a private identity, credential, or the mutable session database.
func SnapshotAdoptionFiles(files []string) (map[string]string, error) {
	if len(files) > 64 {
		return nil, errors.New("update: adoption preserved-file set exceeds 64 files")
	}
	result := make(map[string]string, len(files))
	for _, path := range files {
		if !filepath.IsAbs(path) || filepath.Base(path) == "identity.key" {
			return nil, errors.New("update: adoption preservation requires absolute noncredential paths")
		}
		digest, err := adoptionFileDigest(path)
		if err != nil {
			return nil, err
		}
		result[path] = digest
	}
	return result, nil
}

func adoptionFileDigest(path string) (string, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if errors.Is(err, os.ErrNotExist) {
		return "absent", nil
	}
	if err != nil {
		return "", fmt.Errorf("update: open preserved file: %w", err)
	}
	defer func() { _ = root.Close() }()
	file, err := root.OpenFile(filepath.Base(path), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return "absent", nil
	}
	if err != nil {
		return "", fmt.Errorf("update: open preserved file: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("update: inspect preserved file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Size() > 1<<20 {
		return "", errors.New("update: preserved file must be a bounded regular file without other writers")
	}
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return "", errors.Join(err, errors.New("update: read preserved file within 1 MiB bound"))
	}
	return discoveryDigest(data), nil
}

func checkAdoptionFiles(expected map[string]string) error {
	for path, digest := range expected {
		actual, err := adoptionFileDigest(path)
		if err != nil {
			return err
		}
		if actual != digest {
			return fmt.Errorf("update: preserved file %s changed during adoption", path)
		}
	}
	return nil
}

func checkAdoptionIdentity(stateDir, id string) error {
	actual, err := identity.LoadOwned(stateDir)
	if err != nil {
		return fmt.Errorf("update: adoption daemon account identity: %w", err)
	}
	if actual.ID != id {
		return errors.New("update: adoption account differs from the daemon account identity")
	}
	return nil
}

func checkAdoptionHealth(operation Adoption, observed updateinstall.Health, installed bool) error {
	if observed.HostID != operation.Host.Health.HostID || observed.BootID != operation.Host.Health.BootID {
		return errors.New("update: adoption daemon identity or boot changed")
	}
	want := operation.Host.Health.Build.Digest
	if installed {
		artifact, err := operation.Request.Manifest.Artifact(operation.Request.Current.Platform)
		if err != nil {
			return hostError(err)
		}
		want = artifact.BinarySHA256
	}
	if observed.Build.Digest != want {
		return errors.New("update: adoption running daemon differs from its exact expected image")
	}
	for _, original := range operation.Host.Health.Workers {
		if !hostWorkerPreserved(original, observed.Workers) {
			return fmt.Errorf("update: adoption worker %s was not preserved", original.ID)
		}
	}
	return nil
}

func (s *Store) confirmAdoption(ctx context.Context, operation Adoption, status updateinstall.Status, probe updateinstall.Probe, service func(context.Context, updateinstall.Settings) (string, error)) error {
	unlock, err := s.lockContext(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	envelope, actual, err := s.adoption()
	if err != nil {
		return err
	}
	if envelope.Confirmed {
		if operation.ID == actual.ID && operation.Pin == actual.Pin {
			return nil
		}
		return errors.New("update: confirmed adoption belongs to another operation")
	}
	if !reflect.DeepEqual(operation, actual) || status.Phase != updateinstall.Committed || !reflect.DeepEqual(status.Request, operation.Request) || status.Settings != operation.Settings {
		return errors.New("update: adoption confirmation differs from the committed transaction")
	}
	health, err := s.checkAdoptionConfirmation(ctx, operation, status, probe, service)
	if err != nil {
		return err
	}
	entry := LedgerEntry{Request: operation.ID, Generation: operation.Request.Generation, Version: operation.Version, Metadata: operation.Pin, Binary: health.Build.Digest, From: operation.Host.Health.Build.Digest, Origin: "adoption", CommittedAt: status.UpdatedAt, Workers: len(status.Original.Workers), WorkersPreserved: len(status.Original.Workers)}
	return s.publishAdoptionCommit(envelope, operation, entry)
}

func (s *Store) checkAdoptionConfirmation(ctx context.Context, operation Adoption, status updateinstall.Status, probe updateinstall.Probe, service func(context.Context, updateinstall.Settings) (string, error)) (updateinstall.Health, error) {
	policy, err := s.hostPolicy()
	if err != nil {
		return updateinstall.Health{}, err
	}
	if !reflect.DeepEqual(policy, operation.Policy) {
		return updateinstall.Health{}, errors.New("update: adoption seeded policy changed before confirmation")
	}
	if err := checkAdoptionFiles(operation.Host.Preserved); err != nil {
		return updateinstall.Health{}, err
	}
	if err := checkAdoptionIdentity(operation.Settings.StateDir, operation.Host.Health.HostID); err != nil {
		return updateinstall.Health{}, err
	}
	definition, err := service(ctx, operation.Settings)
	if err != nil {
		return updateinstall.Health{}, err
	}
	if definition != operation.Host.Service {
		return updateinstall.Health{}, errors.New("update: adopted daemon service configuration changed")
	}
	health, err := probe(ctx)
	if err != nil {
		return health, hostError(err)
	}
	if err := checkAdoptionHealth(operation, health, true); err != nil {
		return health, err
	}
	return health, checkAdoptionStageWorkers(status.Original.Workers, health.Workers)
}

func (s *Store) publishAdoptionCommit(envelope HostAdoption, operation Adoption, entry LedgerEntry) error {
	if err := s.appendLedger(entry); err != nil {
		return err
	}
	var saved hostLedgerIdentity
	if err := s.readAuthority(ledgerIdentityName(entry), &saved); err != nil {
		return err
	}
	entry.Seq = saved.Seq
	var actual LedgerEntry
	if err := s.readAuthority(ledgerName(entry.Seq), &actual); err != nil {
		return err
	}
	if !reflect.DeepEqual(actual, entry) {
		return errors.New("update: adoption ledger entry belongs to another origin or commit")
	}
	envelope.Adopted, envelope.Confirmed = &entry, true
	operation.Phase, operation.Problem = AdoptionComplete, ""
	if err := encodeAdoption(&envelope, operation); err != nil {
		return err
	}
	return s.writeAuthority("host-adoption.json", envelope)
}

func checkAdoptionStageWorkers(original, current []updateinstall.Worker) error {
	for _, worker := range original {
		if !hostWorkerPreserved(worker, current) {
			return fmt.Errorf("update: staged adoption worker %s was not preserved", worker.ID)
		}
	}
	return nil
}
