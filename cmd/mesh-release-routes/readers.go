package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/update"
)

type readerCapabilities = update.HostReaderCapabilities

func candidateUpdater(input publicationInput, manifest release.Manifest, selection *sourceSelection) (*release.UpdaterReader, error) {
	if len(input.Readers) != len(proofPlatforms) {
		return nil, errors.New("publisher: every native candidate must report its implemented updater reader")
	}
	seen := map[release.Platform]bool{}
	epoch := input.Readers[0].Epoch
	for _, reader := range input.Readers {
		if seen[reader.Build.Platform] || reader.Epoch != epoch {
			return nil, errors.New("publisher: candidate reader platforms or epochs disagree")
		}
		if err := validateCandidateReader(reader, manifest); err != nil {
			return nil, err
		}
		seen[reader.Build.Platform] = true
	}
	if epoch == input.Epoch {
		return &release.UpdaterReader{Schema: 1, Epoch: epoch, Position: selection.Position}, nil
	}
	if epoch <= input.Epoch || epoch-input.Epoch != 1 {
		return nil, errors.New("publisher: candidate reader must match the publication epoch or its next waypoint epoch")
	}
	if !selection.Position.Anchor {
		selection.Position.Anchor = true
		selection.Position.Ordinal++
	}
	return &release.UpdaterReader{Schema: 1, Epoch: epoch, Position: release.Position{Anchor: true, Ordinal: 0}}, nil
}

func validateCandidateReader(reader readerCapabilities, manifest release.Manifest) error {
	artifact, err := manifest.Artifact(reader.Build.Platform)
	if err != nil {
		return fmt.Errorf("publisher: candidate reader platform: %w", err)
	}
	build := reader.Build
	if reader.Schema != 1 || reader.Epoch == 0 || build.Modified || build.Version != manifest.Version || build.Commit != manifest.Commit || build.Digest != artifact.BinarySHA256 {
		return errors.New("publisher: candidate reader is not bound to its exact release artifact")
	}
	if build.StateVersion != manifest.Compatibility.StateWrite || build.WorkerProtocol != manifest.Compatibility.WorkerWrite || build.UpdateProtocol != release.CurrentUpdateProtocol {
		return errors.New("publisher: candidate reader capabilities differ from its manifest")
	}
	return nil
}

type readerInvocation struct {
	Source   string           `json:"source"`
	Commit   string           `json:"commit"`
	Epoch    uint32           `json:"epoch"`
	Artifact release.Artifact `json:"artifact"`
	Packet   string           `json:"packet"`
	SHA256   string           `json:"sha256"`
	Position release.Position `json:"position"`
	Metadata string           `json:"metadata"`
	Binary   string           `json:"binary"`
}

type readerIndex struct {
	Schema      int                `json:"schema"`
	Target      string             `json:"target"`
	Platform    release.Platform   `json:"platform"`
	Invocations []readerInvocation `json:"invocations"`
}

func readerPacketsOperation(data []byte, directory, destination, native string, output io.Writer) error {
	var input publicationInput
	if err := json.Unmarshal(data, &input); err != nil {
		return fmt.Errorf("publisher: decode packet publication input: %w", err)
	}
	osName, architecture, _ := strings.Cut(native, "/")
	platform := release.Platform{OS: osName, Arch: architecture}
	history, err := loadHistory(directory, input.History)
	if err != nil {
		return err
	}
	selection, err := selectSources(input.Policy, history, input.Version, input.PublishedAt, input.Epoch, input.Waypoint)
	if err != nil {
		return err
	}
	return produceReaderPackets(input, selection, history, directory, destination, platform, output)
}

func produceReaderPackets(input publicationInput, selection sourceSelection, history []publishedEvidence, directory, destination string, platform release.Platform, output io.Writer) error {
	if destination == "" || !slices.Contains(proofPlatforms, platform) {
		return errors.New("publisher: packets requires -output-dir and an actual supported -platform")
	}
	target, err := signedCandidate(directory, input)
	if err != nil {
		return err
	}
	if _, err := readerDocuments(directory, target); err != nil {
		return err
	}
	index := readerIndex{Schema: 1, Target: input.Version, Platform: platform, Invocations: []readerInvocation{}}
	for _, source := range history {
		if source.Metadata == nil || source.Metadata.Updater == nil || !slices.Contains(selection.Sources, source.Tag) {
			continue
		}
		invocation, err := writeReaderPacket(directory, destination, source, target, platform, history)
		if err != nil {
			return err
		}
		index.Invocations = append(index.Invocations, invocation)
	}
	data, err := json.Marshal(index)
	if err != nil {
		return fmt.Errorf("publisher: encode reader index: %w", err)
	}
	if err := writePublisherFile(destination, "index.json", data); err != nil {
		return err
	}
	return encodePublisherOutput(output, map[string]any{"target": input.Version, "platform": platform, "originalReaders": len(index.Invocations)})
}

func signedCandidate(directory string, input publicationInput) (publishedEvidence, error) {
	item, err := readCandidate(directory, input)
	if err != nil {
		return item, err
	}
	data, err := readLocal(directory+"/"+input.Version+"/mesh-update.json", release.MaxMetadataBytes)
	if err != nil {
		return item, err
	}
	metadata, err := release.DecodeUpdateMetadata(data)
	if err != nil {
		return item, fmt.Errorf("publisher: read signed candidate metadata: %w", err)
	}
	if metadata.Version != input.Version || metadata.Manifest != sum(item.ManifestBytes) {
		return item, errors.New("publisher: signed candidate manifest binding differs")
	}
	envelope, err := readLocal(directory+"/"+input.Version+"/mesh-update.sig.json", release.MaxSignatureBytes)
	if err != nil {
		return item, err
	}
	item.Metadata, item.MetadataBytes = &metadata, data
	item.Assets = append(item.Assets, retainedAsset{Name: "mesh-update.sig.json", SHA256: sum(envelope)})
	return item, nil
}

func readerDocuments(directory string, item publishedEvidence) (update.HostReaderDocuments, error) {
	if item.Metadata == nil {
		return update.HostReaderDocuments{}, errors.New("publisher: reader packet lacks signed metadata")
	}
	envelopeRef, exists := item.asset("mesh-update.sig.json")
	if !exists {
		return update.HostReaderDocuments{}, errors.New("publisher: reader packet lacks the original signature envelope")
	}
	envelope, err := readAsset(directory, envelopeRef, release.MaxSignatureBytes)
	if err != nil {
		return update.HostReaderDocuments{}, err
	}
	page, err := readAsset(directory, item.Metadata.Page, release.MaxAnchorPageBytes)
	if err != nil {
		return update.HostReaderDocuments{}, err
	}
	trust, err := release.LoadTrustRoot(os.Getenv("MESH_RELEASE_TRUST_CACHE"))
	if err != nil {
		return update.HostReaderDocuments{}, fmt.Errorf("publisher: load reader trust: %w", err)
	}
	if _, err := release.VerifyMetadata(trust, item.MetadataBytes, envelope, item.ManifestBytes, page); err != nil {
		return update.HostReaderDocuments{}, fmt.Errorf("publisher: authenticate reader packet: %w", err)
	}
	return update.HostReaderDocuments{Metadata: item.MetadataBytes, Envelope: envelope, Manifest: item.ManifestBytes, Page: page}, nil
}

func writeReaderPacket(directory, destination string, source, target publishedEvidence, platform release.Platform, history []publishedEvidence) (readerInvocation, error) {
	packet, err := buildReaderPacket(directory, source, target, platform, history)
	if err != nil {
		return readerInvocation{}, err
	}
	data, err := json.Marshal(packet)
	if err != nil {
		return readerInvocation{}, fmt.Errorf("publisher: encode original reader packet: %w", err)
	}
	if len(data) > update.MaxHostReaderInputBytes {
		return readerInvocation{}, errors.New("publisher: original reader packet exceeds 8 MiB")
	}
	name := sum(data) + ".reader.json"
	if err := writePublisherFile(destination, name, data); err != nil {
		return readerInvocation{}, err
	}
	artifact, err := source.Manifest.Artifact(platform)
	if err != nil {
		return readerInvocation{}, fmt.Errorf("publisher: original reader artifact: %w", err)
	}
	targetArtifact, err := target.Manifest.Artifact(platform)
	if err != nil {
		return readerInvocation{}, fmt.Errorf("publisher: original reader target artifact: %w", err)
	}
	return readerInvocation{Source: source.Tag, Commit: source.Manifest.Commit, Epoch: source.Metadata.Updater.Epoch, Artifact: artifact, Packet: name, SHA256: sum(data), Position: source.Metadata.Updater.Position, Metadata: sum(target.MetadataBytes), Binary: targetArtifact.BinarySHA256}, nil
}

func buildReaderPacket(directory string, source, target publishedEvidence, platform release.Platform, history []publishedEvidence) (update.HostReaderInput, error) {
	packet := update.HostReaderInput{Schema: update.HostReaderSchema}
	var err error
	packet.Source, err = readerDocuments(directory, source)
	if err != nil {
		return packet, err
	}
	packet.Target, err = readerDocuments(directory, target)
	if err != nil {
		return packet, err
	}
	transition, err := verifyHistoricalEdge(directory, source, target, platform)
	if err != nil {
		return packet, err
	}
	proof, forward, rollback, err := automaticRollback(directory, source, target, transition)
	if err != nil {
		return packet, err
	}
	packet.Forward, packet.Rollback = forward, rollback
	packet.Workers, err = readerWorkers(proof.Workers, platform, history)
	return packet, err
}

func readerWorkers(digests []string, platform release.Platform, history []publishedEvidence) ([]update.HostReaderWorker, error) {
	if len(digests) == 0 || len(digests) > update.MaxHostReaderWorkers {
		return nil, errors.New("publisher: retained worker evidence exceeds its bound")
	}
	implementations := sourceArtifacts(history)
	workers := make([]update.HostReaderWorker, 0, len(digests))
	for _, digest := range digests {
		item, exists := implementations[artifactIdentity{Platform: platform, Digest: digest}]
		if !exists {
			return nil, errors.New("publisher: retained worker has no exact published protocol evidence")
		}
		workers = append(workers, update.HostReaderWorker{Digest: digest, Protocol: item.Manifest.Compatibility.WorkerWrite})
	}
	return workers, nil
}
