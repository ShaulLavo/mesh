package main

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/release"
)

func TestPublisherFirstMetadataBoundary(t *testing.T) {
	var captured struct {
		Input   publicationInput      `json:"input"`
		Anchors []release.AnchorEntry `json:"anchors"`
	}
	if err := json.Unmarshal(boundaryFile(t, "boundary.json"), &captured); err != nil {
		t.Fatal(err)
	}
	metadataBytes := boundaryFile(t, "mesh-update.json")
	metadata, err := release.DecodeUpdateMetadata(metadataBytes)
	if err != nil {
		t.Fatal(err)
	}
	prior, err := release.DecodeAnchorPage(boundaryFile(t, "page.json"), metadata.Page)
	if err != nil {
		t.Fatal(err)
	}
	last := captured.Anchors[len(captured.Anchors)-1]
	item := publishedEvidence{Tag: metadata.Version, Metadata: &metadata, MetadataBytes: metadataBytes}
	entry, ok := historicalAnchor(item, nil, 1, "")
	if !ok || entry != last || last.AnchorsFrom != 3 || prior.Entries[len(prior.Entries)-1].AnchorsFrom != 17 {
		t.Fatalf("captured boundary changed: %+v", entry)
	}
	graph := boundaryGraph(captured.Anchors)
	_, err = release.BuildAnchorPage(1, 0, captured.Anchors, nil, captured.Input.Version)
	if err == nil || !strings.Contains(err.Error(), "v0.1.306 -> v0.1.329: anchorsFrom decreases from 17 to 3") {
		t.Fatalf("raw captured conflict: %v", err)
	}
	setAnchorRanges(captured.Anchors, graph)
	if !slices.Equal(prior.Entries, captured.Anchors[:len(prior.Entries)]) {
		t.Fatal("immutable seed prefix changed")
	}
	if captured.Anchors[len(captured.Anchors)-1].AnchorsFrom != 17 || metadata.Direct.AnchorsFrom != 3 || sum(metadataBytes) != last.Metadata {
		t.Fatal("index must narrow its search range and preserve signed metadata")
	}
	if first := windowStart(captured.Anchors, captured.Input.PublishedAt, captured.Input.Policy); first != 4 {
		t.Fatalf("B direct window starts at %d, want 4", first)
	}
	directory := t.TempDir()
	retained := map[uint64]retainedPage{0: {Page: prior, Ref: metadata.Page}}
	ref, err := publishPages(captured.Input, captured.Anchors, directory, retained)
	if err != nil {
		t.Fatal(err)
	}
	data, err := readLocal(filepath.Join(directory, ref.Name), release.MaxAnchorPageBytes)
	if err != nil {
		t.Fatal(err)
	}
	page, err := release.DecodeAnchorPage(data, ref)
	if err != nil || !slices.Equal(page.Entries, captured.Anchors) {
		t.Fatalf("new page: %v", err)
	}
	if _, _, err := reusePage(retained[0], page.Entries); err != nil {
		t.Fatal(err)
	}
	setAnchorRanges(captured.Anchors, graph)
	if !slices.Equal(page.Entries, captured.Anchors) {
		t.Fatal("range normalization is not stable")
	}
}

func boundaryFile(t *testing.T, name string) []byte {
	t.Helper()
	data, err := readLocal(filepath.Join("testdata", "first-metadata", name), release.MaxAnchorPageBytes)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func boundaryGraph(anchors []release.AnchorEntry) verifiedGraph {
	graph := verifiedGraph{}
	for _, target := range anchors {
		for _, source := range anchors[target.AnchorsFrom:target.Ordinal] {
			addBoundaryEdge(graph, source.Version, target.Version)
		}
	}
	return graph
}

func addBoundaryEdge(graph verifiedGraph, from, to string) {
	for _, platform := range proofPlatforms {
		graph[edgeKey{From: from, To: to, Platform: platform}] = release.Transition{}
	}
}
