package main

import (
	"testing"

	"github.com/shaul/mesh/internal/release"
)

func TestPublisherEpochRejectsPreviousRootFromAnotherRelease(t *testing.T) {
	directory := t.TempDir()
	waypoint := fixtureRelease(t, directory, 400, nil)
	waypoint.Metadata = &release.UpdateMetadata{Epoch: 1, Position: release.Position{Anchor: true, Ordinal: 200}}
	waypoint.MetadataBytes = fixtureJSON(t, waypoint.Metadata)
	root := release.DiscoveryRoot{Format: 1, Epoch: 1, Head: release.AssetRef{Tag: waypoint.Tag, Name: release.UpdateMetadataAssetName, SHA256: sum(waypoint.MetadataBytes)}}
	data, err := release.EncodeDiscoveryRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	previous := release.AssetRef{Tag: "v0.1.399", Name: release.DiscoveryRootAssetName, SHA256: sum(data)}
	fixtureWrite(t, directory, previous.Tag, previous.Name, data)
	input := publicationInput{Epoch: 2, Waypoint: waypoint.Tag, PreviousRoot: &previous}
	if err := validateEpoch(directory, input, []publishedEvidence{waypoint}); err == nil {
		t.Fatal("previous root from another release accepted despite its different head identity")
	}
}
