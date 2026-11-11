package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/shaul/mesh/internal/release"
)

func CurrentHostReaderCapabilities() HostReaderCapabilities {
	return HostReaderCapabilities{Schema: HostReaderSchema, Epoch: HostReaderEpoch, Build: release.Current()}
}

func DecodeHostReaderInput(data []byte) (HostReaderInput, error) {
	var input HostReaderInput
	if len(data) == 0 || len(data) > MaxHostReaderInputBytes {
		return input, errors.New("update: host reader packet exceeds 8 MiB or is empty")
	}
	if err := decodeAuthority(data, &input); err != nil {
		return input, err
	}
	return input, input.validate()
}

// VerifyHostReaderInput is offline and read-only. Packet contents can identify
// subjects, but only the executing image supplies the source build identity.
func VerifyHostReaderInput(ctx context.Context, input HostReaderInput, trustCache string) (HostReaderResult, error) {
	trust, err := release.LoadTrustRoot(trustCache)
	if err != nil {
		return HostReaderResult{}, hostError(err)
	}
	return verifyHostReaderInput(ctx, release.Current(), HostReaderEpoch, input, trust)
}

func (input HostReaderInput) validate() error {
	if input.Schema != HostReaderSchema || len(input.Workers) > MaxHostReaderWorkers {
		return errors.New("update: unsupported host reader packet schema or worker count")
	}
	if err := input.Source.validate(); err != nil {
		return err
	}
	if err := input.Target.validate(); err != nil {
		return err
	}
	if len(input.Forward) == 0 || len(input.Forward) > release.MaxTransitionProofBytes || len(input.Rollback) == 0 || len(input.Rollback) > release.MaxTransitionProofBytes {
		return errors.New("update: host reader forward or rollback proof exceeds its bound or is missing")
	}
	for _, worker := range input.Workers {
		if !hostDigest(worker.Digest) || worker.Protocol <= 0 {
			return errors.New("update: host reader retained worker lacks exact digest or protocol")
		}
	}
	data, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("update: encode host reader packet bound: %w", err)
	}
	if len(data) > MaxHostReaderInputBytes {
		return errors.New("update: host reader packet exceeds aggregate 8 MiB bound")
	}
	return nil
}

func (documents HostReaderDocuments) validate() error {
	if len(documents.Metadata) == 0 || len(documents.Metadata) > release.MaxMetadataBytes || len(documents.Envelope) == 0 || len(documents.Envelope) > release.MaxSignatureBytes {
		return errors.New("update: host reader metadata or envelope exceeds its bound or is missing")
	}
	if len(documents.Manifest) == 0 || len(documents.Manifest) > release.DocumentLimit(release.ManifestDocument) || len(documents.Page) == 0 || len(documents.Page) > release.MaxAnchorPageBytes {
		return errors.New("update: host reader manifest or page exceeds its bound or is missing")
	}
	return nil
}

type hostReaderPublication struct {
	metadata release.UpdateMetadata
	manifest release.Manifest
}

func verifyHostReaderInput(ctx context.Context, current release.Build, epoch uint32, input HostReaderInput, trust release.TrustRoot) (HostReaderResult, error) {
	if err := ctx.Err(); err != nil {
		return HostReaderResult{}, hostError(err)
	}
	if err := input.validate(); err != nil {
		return HostReaderResult{}, err
	}
	source, err := verifyReaderPublication(trust, input.Source, epoch, false)
	if err != nil {
		return HostReaderResult{}, err
	}
	if source.metadata.Updater == nil || source.metadata.Updater.Epoch != epoch {
		return HostReaderResult{}, errors.New("update: executing source lacks its authenticated reader epoch marker")
	}
	position := source.metadata.Updater.Position
	if err := verifyHostSource(current, HostSource{Epoch: epoch, Position: position, Manifest: source.manifest}); err != nil {
		return HostReaderResult{}, err
	}
	target, err := verifyReaderPublication(trust, input.Target, epoch, true)
	if err != nil {
		return HostReaderResult{}, err
	}
	if err := readerDirectEdge(current, position, target.metadata); err != nil {
		return HostReaderResult{}, err
	}
	next, err := readerPlannedRelease(current, target)
	if err != nil {
		return HostReaderResult{}, err
	}
	documents := readerPacketDocuments(input, trust, next)
	proof, err := verifyHostHop(ctx, current, nil, next, input.Target.Metadata, documents)
	if err != nil {
		return HostReaderResult{}, err
	}
	if err := verifyReaderWorkers(input.Workers, next.Manifest.Compatibility, proof.Workers); err != nil {
		return HostReaderResult{}, err
	}
	artifact, err := target.manifest.Artifact(current.Platform)
	if err != nil {
		return HostReaderResult{}, hostError(err)
	}
	return HostReaderResult{Schema: HostReaderSchema, Source: current, Epoch: epoch, Position: position, Target: target.manifest.Version, Metadata: proof.Metadata, Binary: artifact.BinarySHA256}, nil
}

func verifyReaderPublication(trust release.TrustRoot, documents HostReaderDocuments, epoch uint32, target bool) (hostReaderPublication, error) {
	var hint struct {
		Epoch uint32 `json:"epoch"`
	}
	if err := json.Unmarshal(documents.Metadata, &hint); err != nil {
		return hostReaderPublication{}, fmt.Errorf("update: read publication epoch: %w", err)
	}
	if hint.Epoch == 0 || hint.Epoch > epoch || target && hint.Epoch != epoch {
		return hostReaderPublication{}, errors.New("update: publication epoch is unsupported by the executing reader")
	}
	metadata, err := release.VerifyMetadata(trust, documents.Metadata, documents.Envelope, documents.Manifest, documents.Page)
	if err != nil {
		return hostReaderPublication{}, hostError(err)
	}
	var manifest release.Manifest
	if err := decodeAuthority(documents.Manifest, &manifest); err != nil {
		return hostReaderPublication{}, err
	}
	return hostReaderPublication{metadata: metadata, manifest: manifest}, nil
}

func readerDirectEdge(current release.Build, position release.Position, target release.UpdateMetadata) error {
	if target.Updater == nil {
		return errors.New("update: autonomous target lacks authenticated updater capability")
	}
	if position.Ordinal > target.Position.Ordinal {
		return errors.New("update: target precedes the authenticated source position")
	}
	if position.Anchor && position.Ordinal >= target.Direct.AnchorsFrom || slices.Contains(target.Direct.Since, current.Version) {
		return nil
	}
	return errors.New("update: signed target does not admit the authenticated source position")
}

func readerPlannedRelease(current release.Build, target hostReaderPublication) (release.PlannedRelease, error) {
	artifact, err := target.manifest.Artifact(current.Platform)
	if err != nil {
		return release.PlannedRelease{}, hostError(err)
	}
	for _, transition := range target.manifest.Compatibility.Transitions {
		if transition.Platform == current.Platform && transition.FromDigest == current.Digest && transition.ToDigest == artifact.BinarySHA256 {
			return release.PlannedRelease{Metadata: &target.metadata, Manifest: target.manifest, Position: target.metadata.Position, Transition: transition}, nil
		}
	}
	return release.PlannedRelease{}, errors.New("update: signed target lacks the executing source's exact forward transition")
}

func readerPacketDocuments(input HostReaderInput, trust release.TrustRoot, next release.PlannedRelease) release.DocumentSource {
	assets := map[string][]byte{release.UpdateMetadataAssetName: input.Target.Metadata, release.ManifestAssetName: input.Target.Manifest,
		next.Transition.Proof + ".json": input.Forward, discoveryDigest(input.Rollback) + ".rollback.json": input.Rollback}
	return release.DocumentSource{Read: func(ctx context.Context, ref release.AssetRef, _ release.DocumentKind) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, hostError(err)
		}
		data, found := assets[ref.Name]
		if !found || ref.Tag != next.Manifest.Version {
			return nil, errors.New("update: requested proof asset is absent from offline packet")
		}
		return data, nil
	}, VerifyMetadata: func(_ context.Context, _ release.AssetRef, data []byte) error {
		return hostError(release.VerifySignature(trust, data, input.Target.Envelope))
	}}
}

func verifyReaderWorkers(workers []HostReaderWorker, compatibility release.Compatibility, proved []string) error {
	for _, worker := range workers {
		if worker.Protocol < compatibility.WorkerMin || worker.Protocol > compatibility.WorkerMax || !slices.Contains(proved, worker.Digest) {
			return fmt.Errorf("update: retained worker %s lacks exact protocol and rollback coverage", worker.Digest)
		}
	}
	return nil
}
