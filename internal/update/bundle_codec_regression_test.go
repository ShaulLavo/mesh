package update

import (
	"bytes"
	"testing"

	"github.com/shaul/mesh/internal/release"
)

func TestBundleManifestPreservesPublicLimit(t *testing.T) {
	f := newHostFixture(t)
	version := f.current.Version
	manifest := f.files[version+"/"+release.ManifestAssetName]
	manifest = append(manifest, bytes.Repeat([]byte(" "), release.DocumentLimit(release.ManifestDocument)-len(manifest))...)
	f.files[version+"/"+release.ManifestAssetName] = manifest
	metadata := f.metadata[version]
	metadata.Manifest = discoveryDigest(manifest)
	f.metadata[version] = metadata
	f.signMetadata(version)
	archive, envelope, err := tryFixtureDocumentBundle(t.Context(), t, f.files, version, f.signer)
	if err != nil {
		t.Fatal(err)
	}
	if err := readFixtureDocumentBundle(t, f, archive, envelope, version); err != nil {
		t.Fatal(err)
	}
	f.files[version+"/"+release.ManifestAssetName] = append(manifest, ' ')
	if _, _, err := tryFixtureDocumentBundle(t.Context(), t, f.files, version, f.signer); err == nil {
		t.Fatal("Pack accepted a manifest above the public byte bound")
	}
}

func TestBundlePackRejectsEmptyMember(t *testing.T) {
	f := newHostFixture(t)
	version := f.current.Version
	f.files[version+"/empty.json"] = nil
	if _, _, err := tryFixtureDocumentBundle(t.Context(), t, f.files, version, f.signer); err == nil {
		t.Fatal("Pack accepted an empty document")
	}
}
