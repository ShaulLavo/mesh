package releasedocuments

import (
	"testing"

	"github.com/shaul/mesh/internal/release"
)

func TestDecodeJSONExactFields(t *testing.T) {
	for _, input := range []string{
		`{"Schema":1}`,
		`{"schema":0,"Schema":1}`,
		`{"binding":{"Version":"v0.1.1"}}`,
		`{"binding":{"platforms":[{"OS":"linux"}]}}`,
		`{"documents":[{"Size":1}]}`,
		`{"documents":[{"size":1,"size":2}]}`,
	} {
		t.Run(input, func(t *testing.T) {
			var value catalog
			if err := DecodeJSON([]byte(input), &value); err == nil {
				t.Fatal("noncanonical or repeated field accepted")
			}
		})
	}
	var value catalog
	if err := DecodeJSON([]byte(`{"schema":1,"binding":{"version":"v0.1.1","platforms":[{"os":"linux","arch":"amd64"}]},"documents":[{"name":"a.json","sha256":"abc","size":1}]}`), &value); err != nil {
		t.Fatal(err)
	}
}

func TestManifestLimitMatchesReleaseReader(t *testing.T) {
	if got, want := documentLimit(release.ManifestAssetName), int64(release.DocumentLimit(release.ManifestDocument)); got != want {
		t.Fatalf("manifest limit %d differs from reader %d", got, want)
	}
}
