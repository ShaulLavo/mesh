package release

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestAnchorIndexConservativeSignedRange(t *testing.T) {
	g := graphFixture(t, 50)
	metadata := g.metadata[25]
	metadata.Direct.AnchorsFrom = 0
	data, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	metadataRef := g.put(metadata.Version, UpdateMetadataAssetName, data)
	target := &g.metadata[49]
	page, err := DecodeAnchorPage(g.documents[target.Page], target.Page)
	if err != nil {
		t.Fatal(err)
	}
	page.Entries[25].Metadata = metadataRef.SHA256
	pageBytes, err := EncodeAnchorPage(page, target.Version)
	if err != nil {
		t.Fatal(err)
	}
	target.Page = g.put(target.Version, "anchors.json", pageBytes)
	step, passes := graphNext(t, g, 1, RouteCursor{})
	if step.Next == nil || step.Next.Position.Ordinal != 25 || passes != 1 || step.Next.Metadata.Direct.AnchorsFrom != 0 {
		t.Fatalf("conservative route: %+v in %d passes", step.Next, passes)
	}
	if got := catchupOrdinals(t, g); !slices.Equal(got, []uint64{24, 48, 49}) {
		t.Fatalf("older source lost its route: %v", got)
	}
	entry := page.Entries[25]
	for _, scenario := range []string{"wider range", "empty range", "ordinal", "time", "digest", "signature"} {
		t.Run(scenario, func(t *testing.T) {
			testInvalidAnchorMetadata(t, g, metadata, entry, scenario)
		})
	}
}

func testInvalidAnchorMetadata(t *testing.T, g *routeGraph, metadata UpdateMetadata, entry AnchorEntry, scenario string) {
	t.Helper()
	read := g.source()
	switch scenario {
	case "wider range":
		metadata.Direct.AnchorsFrom = 2
		data, _ := json.Marshal(metadata)
		entry.Metadata = g.put(metadata.Version, UpdateMetadataAssetName, data).SHA256
	case "empty range":
		entry.AnchorsFrom = entry.Ordinal + 1
	case "ordinal":
		entry.Ordinal++
	case "time":
		entry.PublishedAt = entry.PublishedAt.Add(time.Second)
	case "digest":
		entry.Metadata = strings.Repeat("f", 64)
	case "signature":
		read.VerifyMetadata = nil
	}
	budget := NewPassBudget()
	if _, _, err := candidateMetadata(t.Context(), metadata, &RoutePending{Entry: entry}, read, &budget); err == nil {
		t.Fatal("invalid anchor metadata accepted")
	}
}

func TestAnchorOrderDiagnostics(t *testing.T) {
	previous := AnchorEntry{Ordinal: 1, Version: "v0.1.306", Manifest: strings.Repeat("a", 64), AnchorsFrom: 1, PublishedAt: time.Date(2026, 11, 8, 12, 32, 21, 0, time.UTC)}
	next := AnchorEntry{Ordinal: 2, Version: "v0.1.329", Metadata: strings.Repeat("b", 64), AnchorsFrom: 1, PublishedAt: previous.PublishedAt.Add(time.Hour)}
	first := previous
	first.Ordinal, first.Version, first.AnchorsFrom = 0, "v0.1.295", 0
	page := AnchorPage{Schema: 1, Epoch: 1, Entries: []AnchorEntry{first, previous, next}}
	if _, err := EncodeAnchorPage(page, "v0.1.330"); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"versions", "anchorsFrom", "publishedAt"} {
		invalid := next
		switch field {
		case "versions":
			invalid.Version = previous.Version
		case "anchorsFrom":
			invalid.AnchorsFrom = 0
		case "publishedAt":
			invalid.PublishedAt = previous.PublishedAt.Add(-time.Second)
		}
		page.Entries[2] = invalid
		_, err := EncodeAnchorPage(page, "v0.1.330")
		assertAnchorOrderError(t, previous, invalid, field, err)
		data, _ := json.Marshal(page)
		_, err = DecodeAnchorPage(data, AssetRef{Tag: "v0.1.330", Name: "anchors.json", SHA256: digestBytes(data)})
		assertAnchorOrderError(t, previous, invalid, field, err)
	}
}

func assertAnchorOrderError(t *testing.T, previous, next AnchorEntry, field string, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("invalid order accepted")
	}
	if !strings.Contains(err.Error(), previous.Version+" -> "+next.Version) || !strings.Contains(err.Error(), field) {
		t.Fatalf("missing adjacent versions or field: %v", err)
	}
}
