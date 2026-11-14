package main

import (
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/release"
)

func fixtureCapabilities(manifest release.Manifest, epoch uint32) []readerCapabilities {
	readers := make([]readerCapabilities, 0, len(manifest.Artifacts))
	for _, artifact := range manifest.Artifacts {
		readers = append(readers, readerCapabilities{Schema: 1, Epoch: epoch, Build: release.Build{Version: manifest.Version, Commit: manifest.Commit, Digest: artifact.BinarySHA256, Platform: artifact.Platform, StateVersion: manifest.Compatibility.StateWrite, WorkerProtocol: manifest.Compatibility.WorkerWrite, UpdateProtocol: release.CurrentUpdateProtocol}})
	}
	return readers
}

func TestPublisherMarkerRequiresActualPlatformArtifactCapabilities(t *testing.T) {
	manifest := fixtureRelease(t, t.TempDir(), 300, nil).Manifest
	input := publicationInput{Epoch: 1, Readers: fixtureCapabilities(manifest, 1)}
	selection := sourceSelection{Position: release.Position{Anchor: true, Ordinal: 19}}
	marker, err := candidateUpdater(input, manifest, &selection)
	if err != nil || marker.Epoch != 1 || marker.Position != selection.Position {
		t.Fatalf("normal marker: %+v, %v", marker, err)
	}
	input.Readers[0].Build.Digest = strings.Repeat("b", 64)
	if _, err := candidateUpdater(input, manifest, &selection); err == nil {
		t.Fatal("capability for another executable accepted")
	}
	input.Readers = nil
	if _, err := candidateUpdater(input, manifest, &selection); err == nil {
		t.Fatal("missing real command granted a marker")
	}
}

func TestPublisherWaypointKeepsOldPublicationAndNewReaderPosition(t *testing.T) {
	manifest := fixtureRelease(t, t.TempDir(), 300, nil).Manifest
	input := publicationInput{Epoch: 1, Readers: fixtureCapabilities(manifest, 2)}
	selection := sourceSelection{Position: release.Position{Ordinal: 19}}
	marker, err := candidateUpdater(input, manifest, &selection)
	if err != nil || marker.Epoch != 2 || marker.Position != (release.Position{Anchor: true, Ordinal: 0}) || selection.Position != (release.Position{Anchor: true, Ordinal: 20}) || input.Epoch != 1 {
		t.Fatalf("old-format waypoint: %+v, %+v, %v", marker, selection, err)
	}
	input.Readers = fixtureCapabilities(manifest, 3)
	if _, err := candidateUpdater(input, manifest, &selection); err == nil {
		t.Fatal("skipped reader epoch accepted")
	}
}

func TestPublisherAutomaticAdmissionUsesSignedMarkerAndEpochCoordinates(t *testing.T) {
	source := publishedEvidence{Tag: "v0.1.300", Metadata: &release.UpdateMetadata{Epoch: 1}}
	target := publishedEvidence{Tag: "v0.1.301", Metadata: &release.UpdateMetadata{Epoch: 1, Position: release.Position{Ordinal: 10}, Direct: release.DirectSources{AnchorsFrom: 0}}}
	if automaticAdmission(source, target) {
		t.Fatal("metadata presence granted an automatic reader")
	}
	source.Metadata.Updater = &release.UpdaterReader{Schema: 1, Epoch: 1, Position: release.Position{Anchor: true, Ordinal: 9}}
	if !automaticAdmission(source, target) {
		t.Fatal("compatible signed source marker was ignored")
	}
	target.Metadata.Epoch = 2
	if automaticAdmission(source, target) {
		t.Fatal("old reader skipped its format waypoint")
	}
	source.Metadata.Updater.Epoch = 2
	source.Metadata.Updater.Position = release.Position{Anchor: true, Ordinal: 0}
	target.Metadata.Position.Ordinal = 1
	if !automaticAdmission(source, target) {
		t.Fatal("new reader did not use its explicit new-epoch waypoint source position")
	}
}

func TestPublisherReaderWorkerEvidenceRequiresExactPublishedImplementation(t *testing.T) {
	source := fixtureRelease(t, t.TempDir(), 300, nil)
	artifact, err := source.Manifest.Artifact(proofPlatforms[0])
	if err != nil {
		t.Fatal(err)
	}
	workers, err := readerWorkers([]string{artifact.BinarySHA256}, proofPlatforms[0], []publishedEvidence{source})
	if err != nil || len(workers) != 1 || workers[0].Protocol != source.Manifest.Compatibility.WorkerWrite {
		t.Fatalf("retained workers: %+v, %v", workers, err)
	}
	if _, err := readerWorkers([]string{strings.Repeat("c", 64)}, proofPlatforms[0], []publishedEvidence{source}); err == nil {
		t.Fatal("unknown worker implementation received guessed protocol evidence")
	}
}
