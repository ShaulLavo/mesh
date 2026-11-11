package release

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestDiscoverySerializedBoundsAndAcyclicPages(t *testing.T) {
	version := "v18446744073709551615.18446744073709551615.18446744073709551615"
	previous := make(map[uint64]AssetRef)
	pageNumber := ^uint64(0) / 64
	for distance := uint64(1); distance <= pageNumber; distance *= 2 {
		previous[pageNumber-distance] = AssetRef{Tag: "v18446744073709551613.18446744073709551615.18446744073709551615", Name: strings.Repeat("n", MaxAssetNameBytes), SHA256: strings.Repeat("a", 64)}
	}
	entries := make([]AnchorEntry, AnchorsPerPage)
	for i := range entries {
		entries[i] = AnchorEntry{Ordinal: pageNumber*64 + uint64(i), Version: fmt.Sprintf("v18446744073709551614.18446744073709551615.%d", ^uint64(0)-63+uint64(i)), Metadata: strings.Repeat("b", 64), Manifest: strings.Repeat("c", 64), PublishedAt: time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)}
	}
	page, err := BuildAnchorPage(1, pageNumber, entries, previous, version)
	if err != nil {
		t.Fatal(err)
	}
	data, err := EncodeAnchorPage(page, version)
	if err != nil || len(data) > MaxAnchorPageBytes {
		t.Fatalf("maximum page: %d bytes, %v", len(data), err)
	}
	ref := AssetRef{Tag: version, Name: strings.Repeat("p", 128), SHA256: digestBytes(data)}
	if _, err := DecodeAnchorPage(data, ref); err != nil {
		t.Fatal(err)
	}
	root := DiscoveryRoot{Format: 1, Epoch: 2, Head: AssetRef{Tag: version, Name: UpdateMetadataAssetName, SHA256: strings.Repeat("d", 64)}, Previous: &AssetRef{Tag: "v1.0.0", Name: DiscoveryRootAssetName, SHA256: strings.Repeat("e", 64)}}
	encoded, err := EncodeDiscoveryRoot(root)
	if err != nil || len(encoded) > MaxDiscoveryRootBytes {
		t.Fatalf("maximum root: %d bytes, %v", len(encoded), err)
	}
	if _, err := DecodeDiscoveryRoot(encoded); err != nil {
		t.Fatal(err)
	}
	// The asset is under the current tag, but contains only the prior entry.
	prior := AnchorEntry{Ordinal: 0, Version: "v0.1.1", Metadata: strings.Repeat("a", 64), PublishedAt: time.Now().UTC()}
	page, err = BuildAnchorPage(1, 0, []AnchorEntry{prior}, nil, "v0.1.2")
	if err != nil {
		t.Fatal(err)
	}
	page.Entries[0].Version = "v0.1.2"
	if _, err := EncodeAnchorPage(page, "v0.1.2"); err == nil {
		t.Fatal("accepted current anchor self-hash")
	}
}

func TestDiscoveryRejectsCorruptionAndFieldBounds(t *testing.T) {
	entry := AnchorEntry{Ordinal: 0, Version: "v0.1.1", Metadata: strings.Repeat("a", 64), PublishedAt: time.Now().UTC()}
	base := AnchorPage{Schema: 1, Epoch: 1, Entries: []AnchorEntry{entry}}
	tests := []struct {
		name   string
		change func(*AnchorPage)
	}{
		{"long version", func(p *AnchorPage) { p.Entries[0].Version = "v" + strings.Repeat("1", 65) }},
		{"non ascii", func(p *AnchorPage) { p.Entries[0].Version = "v0.1.é" }},
		{"digest", func(p *AnchorPage) { p.Entries[0].Metadata = strings.Repeat("a", 63) }},
		{"entries", func(p *AnchorPage) { p.Entries = make([]AnchorEntry, 65) }},
		{"backrefs", func(p *AnchorPage) { p.Back = make([]PageRef, 65) }},
		{"ordinal", func(p *AnchorPage) { p.Entries[0].Ordinal = 1 }},
		{"future source", func(p *AnchorPage) { p.Entries[0].AnchorsFrom = 1 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertInvalidDiscoveryPage(t, base, test.change)
		})
	}
	data, _ := EncodeAnchorPage(base, "v0.1.2")
	ref := AssetRef{Tag: "v0.1.2", Name: "page.json", SHA256: digestBytes(data)}
	if _, err := DecodeAnchorPage(append(data, ' '), ref); err == nil {
		t.Fatal("accepted wrong exact digest")
	}
	oversize := []byte(string(data) + strings.Repeat(" ", MaxAnchorPageBytes))
	ref.SHA256 = digestBytes(oversize)
	if _, err := DecodeAnchorPage(oversize, ref); err == nil {
		t.Fatal("accepted oversized serialized page")
	}
	root := DiscoveryRoot{Format: 1, Epoch: 1, Head: AssetRef{Tag: "v0.1.2", Name: UpdateMetadataAssetName, SHA256: strings.Repeat("a", 64)}}
	encoded, _ := EncodeDiscoveryRoot(root)
	if _, err := DecodeDiscoveryRoot(append(encoded, []byte(strings.Repeat(" ", MaxDiscoveryRootBytes))...)); err == nil {
		t.Fatal("accepted oversized serialized root")
	}
	for _, name := range []string{strings.Repeat("a", 129), "é.json", "../page.json"} {
		ref.Name = name
		if _, err := DecodeAnchorPage(data, ref); err == nil {
			t.Fatalf("accepted name %q", name)
		}
	}
}

func assertInvalidDiscoveryPage(t *testing.T, base AnchorPage, change func(*AnchorPage)) {
	t.Helper()
	page := base
	page.Entries = append([]AnchorEntry(nil), base.Entries...)
	change(&page)
	if _, err := EncodeAnchorPage(page, "v0.1.2"); err == nil {
		t.Fatal("producer accepted invalid page")
	}
	data, _ := json.Marshal(page)
	if _, err := DecodeAnchorPage(data, AssetRef{Tag: "v0.1.2", Name: "p.json", SHA256: digestBytes(data)}); err == nil {
		t.Fatal("decoder accepted invalid page")
	}
}
