package update

import (
	"encoding/json"
	"testing"

	"github.com/shaul/mesh/internal/release"
)

func TestHostSourceRequiresAuthenticatedReaderMarker(t *testing.T) {
	for _, kind := range []string{"missing", "wrong epoch", "altered marker"} {
		t.Run(kind, func(t *testing.T) { testHostSourceMarker(t, kind) })
	}
}

func testHostSourceMarker(t *testing.T, kind string) {
	f := newHostFixture(t)
	version := f.current.Version
	data := mustHostJSON(f.metadata[version])
	if kind != "missing" {
		metadata := f.metadata[version]
		metadata.Updater = &release.UpdaterReader{Schema: 1, Epoch: 2, Position: release.Position{Anchor: true, Ordinal: 0}}
		data = mustHostJSON(metadata)
	}
	f.files[version+"/"+release.UpdateMetadataAssetName] = data
	if kind != "altered marker" {
		f.files[version+"/mesh-update.sig.json"] = f.signer.envelope(t, data)
	}
	if _, err := f.documents.Current(t.Context(), f.current); err == nil {
		t.Fatal("unavailable reader marker authorized source")
	}
}

func TestHostWaypointUsesSignedNewEpochPosition(t *testing.T) {
	f := newHostFixture(t)
	version := f.current.Version
	metadata := f.metadata[version]
	metadata.Position.Ordinal = 7
	metadata.Updater = &release.UpdaterReader{Schema: 1, Epoch: 2, Position: release.Position{Anchor: true, Ordinal: 0}}
	data := mustHostJSON(metadata)
	f.files[version+"/"+release.UpdateMetadataAssetName] = data
	f.files[version+"/mesh-update.sig.json"] = f.signer.envelope(t, data)
	source, err := f.documents.current(t.Context(), f.current, 2)
	if err != nil {
		t.Fatal(err)
	}
	if source.Epoch != 2 || source.Position != (release.Position{Anchor: true, Ordinal: 0}) || source.Position == metadata.Position {
		t.Fatalf("waypoint position inferred from publication: %+v", source)
	}
	var publication release.UpdateMetadata
	if err := json.Unmarshal(data, &publication); err != nil || publication.Epoch != 1 {
		t.Fatalf("fixture changed publication epoch: %d %v", publication.Epoch, err)
	}
}
