package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

type routeGraph struct {
	documents map[AssetRef][]byte
	metadata  []UpdateMetadata
	builds    []Build
	reads     map[AssetRef]int
}

// This model supplies a fixed 24-anchor admission window independently of the
// planner. Expected paths below are stated explicitly rather than derived by it.
func graphFixture(t *testing.T, count int) *routeGraph {
	t.Helper()
	g := &routeGraph{documents: make(map[AssetRef][]byte), reads: make(map[AssetRef]int)}
	pages := make(map[uint64]AssetRef)
	var entries []AnchorEntry
	for i := 0; i < count; i++ {
		page := uint64(i / 64)
		if i > 0 && i%64 == 0 {
			page--
		}
		start := (i / 64) * 64
		if i > 0 && i%64 == 0 {
			start -= 64
		}
		index, err := BuildAnchorPage(1, page, entries[start:], pages, fmt.Sprintf("v0.1.%d", i+1))
		if err != nil {
			t.Fatal(err)
		}
		data, err := EncodeAnchorPage(index, fmt.Sprintf("v0.1.%d", i+1))
		if err != nil {
			t.Fatal(err)
		}
		pageRef := g.put(fmt.Sprintf("v0.1.%d", i+1), "anchors.json", data)
		if len(index.Entries) == 64 {
			pages[page] = pageRef
		}
		manifest, build := graphManifest(g, i)
		manifestData, _ := json.Marshal(manifest)
		manifestRef := g.put(build.Version, ManifestAssetName, manifestData)
		metadata := UpdateMetadata{Schema: 1, Epoch: 1, Trust: 1, Version: build.Version, Manifest: manifestRef.SHA256, PublishedAt: time.Date(2026, 1, 1, 0, 0, i, 0, time.UTC), Position: Position{Anchor: true, Ordinal: uint64(i)}, Direct: DirectSources{AnchorsFrom: uint64(max(0, i-24))}, Page: pageRef}
		metadataData, _ := json.Marshal(metadata)
		metadataRef := g.put(build.Version, UpdateMetadataAssetName, metadataData)
		entries = append(entries, AnchorEntry{Ordinal: uint64(i), Version: build.Version, Metadata: metadataRef.SHA256, AnchorsFrom: metadata.Direct.AnchorsFrom, PublishedAt: metadata.PublishedAt})
		g.metadata = append(g.metadata, metadata)
		g.builds = append(g.builds, build)
	}
	return g
}

func graphManifest(g *routeGraph, i int) (Manifest, Build) {
	version := fmt.Sprintf("v0.1.%d", i+1)
	to := digestBytes([]byte(version))
	compat := Compatibility{StateReadMin: 1, StateReadMax: 16, StateWrite: 16, WorkerMin: 1, WorkerMax: 1, WorkerWrite: 1, JournalVersion: 1}
	for source := max(0, i-24); source < i; source++ {
		from := digestBytes([]byte(fmt.Sprintf("v0.1.%d", source+1)))
		receipt := TransitionReceipt{Schema: 1, Platform: Platform{OS: "linux", Arch: "amd64"}, FromDigest: from, ToDigest: to, StateReadMin: 1, StateReadMax: 16, StateWrite: 16, WorkerMin: 1, WorkerMax: 1, WorkerWrite: 1, JournalVersion: 1, CandidateOpenedRetainedState: true, SessionsPreserved: true, RecoveryRecordsPreserved: true}
		data, _ := json.Marshal(receipt)
		proof := digestBytes(data)
		g.put(version, proof+".json", data)
		compat.Transitions = append(compat.Transitions, Transition{Platform: receipt.Platform, FromDigest: from, ToDigest: to, Proof: proof})
	}
	var artifacts []Artifact
	for _, platform := range supportedPlatforms {
		artifacts = append(artifacts, Artifact{Platform: platform, Archive: ArchiveName(platform, ArchiveGzip), SHA256: strings.Repeat("a", 64), BinarySHA256: to})
	}
	manifest := Manifest{Schema: 1, Version: version, Commit: strings.Repeat("b", 40), Artifacts: artifacts, Compatibility: compat}
	build := Build{Version: version, Digest: to, Platform: Platform{OS: "linux", Arch: "amd64"}, StateVersion: 16, WorkerProtocol: 1, UpdateProtocol: 1}
	return manifest, build
}

func (g *routeGraph) put(tag, name string, data []byte) AssetRef {
	ref := AssetRef{Tag: tag, Name: name, SHA256: digestBytes(data)}
	g.documents[ref] = data
	return ref
}

func (g *routeGraph) source() DocumentSource {
	return DocumentSource{Read: func(_ context.Context, ref AssetRef, _ DocumentKind) ([]byte, error) {
		g.reads[ref]++
		data, ok := g.documents[ref]
		if !ok {
			return nil, errors.New("missing fixture document")
		}
		return data, nil
	}, VerifyMetadata: func(_ context.Context, ref AssetRef, data []byte) error {
		if !slices.Equal(g.documents[ref], data) {
			return errors.New("fixture authentication failed")
		}
		return nil
	}}
}

func graphNext(t *testing.T, g *routeGraph, from uint64, cursor RouteCursor) (RouteStep, int) {
	t.Helper()
	for pass := 1; pass <= 100; pass++ {
		step, err := NextHop(t.Context(), g.builds[from], Position{Anchor: true, Ordinal: from}, g.metadata[len(g.metadata)-1], cursor, g.source(), NewPassBudget())
		if err != nil {
			t.Fatal(err)
		}
		if step.Budget.Discovery < 0 || step.Budget.Evidence < 0 {
			t.Fatal("exceeded pass budget")
		}
		if step.Next != nil || step.Waiting {
			return step, pass
		}
		data, _ := json.Marshal(step.Cursor)
		if err := json.Unmarshal(data, &cursor); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("route did not progress in 100 passes")
	return RouteStep{}, 0
}

func TestRouteOldAnchorAndThirteenHopCatchup(t *testing.T) {
	for _, test := range []struct {
		count int
		want  []uint64
	}{{53, []uint64{24, 48, 52}}, {301, []uint64{24, 48, 72, 96, 120, 144, 168, 192, 216, 240, 264, 288, 300}}} {
		g := graphFixture(t, test.count)
		got := catchupOrdinals(t, g)
		if !slices.Equal(got, test.want) {
			t.Fatalf("route = %v, want %v", got, test.want)
		}
	}
}

func TestRouteTwoThousandAnchorsAndPassResume(t *testing.T) {
	g := graphFixture(t, 2001)
	step, passes := graphNext(t, g, 0, RouteCursor{})
	if step.Next == nil || step.Next.Position.Ordinal != 24 || passes < 2 {
		t.Fatalf("long route: next %v, passes %d", step.Next, passes)
	}
	if len(step.Cursor.Pages) > 256 {
		t.Fatal("unbounded resident locations")
	}
	for ref := range g.reads {
		if ref.Name == ManifestAssetName && ref.Tag != "v0.1.25" {
			t.Fatalf("search tried sequential/latest manifest %s", ref.Tag)
		}
	}
	// Evidence can resume even when a pass permits only one manifest/proof.
	cursor := RouteCursor{}
	for pass := 0; pass < 30; pass++ {
		budget := NewPassBudget()
		budget.Discovery = 2
		budget.Evidence = 1
		step, err := NextHop(t.Context(), g.builds[0], Position{Anchor: true}, g.metadata[2000], cursor, g.source(), budget)
		if err != nil {
			t.Fatal(err)
		}
		cursor = step.Cursor
		if step.Next != nil && step.Next.Position.Ordinal != 24 {
			t.Fatal("wrong resumed candidate")
		}
		if step.Next != nil {
			return
		}
		if !step.Exhausted {
			t.Fatal("expected bounded exhaustion")
		}
	}
	t.Fatal("pass exhaustion reset progress")
}

func TestRouteNonanchorUsesFirstNextAnchorOrExactSince(t *testing.T) {
	g := graphFixture(t, 53)
	step, err := NextHop(t.Context(), g.builds[0], Position{}, g.metadata[52], RouteCursor{}, g.source(), NewPassBudget())
	if err != nil || step.Next == nil || step.Next.Position.Ordinal != 1 {
		t.Fatalf("nonanchor route = %v, %v", step.Next, err)
	}
	failed := FailedEdge{FromDigest: g.builds[0].Digest, ToDigest: g.builds[1].Digest, Platform: g.builds[0].Platform}
	step, err = NextHop(t.Context(), g.builds[0], Position{}, g.metadata[52], RouteCursor{Failed: []FailedEdge{failed}}, g.source(), NewPassBudget())
	if err != nil || step.Next != nil || !step.Waiting {
		t.Fatalf("failed first anchor must wait: %+v, %v", step, err)
	}
	target := g.metadata[3]
	target.Direct.Since = []string{g.builds[0].Version}
	step, err = NextHop(t.Context(), g.builds[0], Position{}, target, RouteCursor{}, g.source(), NewPassBudget())
	if err != nil || step.Next == nil || step.Next.Position.Ordinal != 3 {
		t.Fatalf("exact Since route = %v, %v", step.Next, err)
	}
}

func TestRouteHeadRefreshReplacesPartialAndKeepsFullPages(t *testing.T) {
	g := graphFixture(t, 132)
	budget := NewPassBudget()
	budget.Discovery = 1
	step, err := NextHop(t.Context(), g.builds[0], Position{Anchor: true}, g.metadata[130], RouteCursor{}, g.source(), budget)
	if err != nil || !step.Exhausted {
		t.Fatalf("initial search: %v, %v", step, err)
	}
	if !step.Cursor.Pages[0].Full || !step.Cursor.Pages[1].Full || step.Cursor.Pages[2].Full {
		t.Fatalf("page fullness = %v", step.Cursor.Pages)
	}
	full := step.Cursor.Pages[0]
	partial := step.Cursor.Pages[2]
	budget = NewPassBudget()
	budget.Discovery = 0
	refreshed, err := NextHop(t.Context(), g.builds[0], Position{Anchor: true}, g.metadata[131], step.Cursor, g.source(), budget)
	if err != nil || !refreshed.Exhausted {
		t.Fatalf("refreshed search: %v, %v", refreshed, err)
	}
	if refreshed.Cursor.Pages[0] != full || refreshed.Cursor.Pages[2] == partial || refreshed.Cursor.Pages[2].Ref != g.metadata[131].Page {
		t.Fatal("partial page did not refresh by head digest")
	}
	if refreshed.Cursor.Search.Low != 1 || refreshed.Cursor.Search.High != 130 {
		t.Fatal("head-dependent binary search did not reset")
	}
	if step.Cursor.Pages[2] != partial {
		t.Fatal("pure search mutated the input cursor")
	}
}

func TestRouteFailedTargetUsesVerifiedPreviousAnchor(t *testing.T) {
	g := graphFixture(t, 5)
	failed := FailedEdge{FromDigest: g.builds[0].Digest, ToDigest: g.builds[4].Digest, Platform: g.builds[0].Platform}
	step, _ := graphNext(t, g, 0, RouteCursor{Failed: []FailedEdge{failed}})
	if step.Next == nil || step.Next.Position.Ordinal != 3 {
		t.Fatalf("failed direct target fallback = %v", step.Next)
	}
}

func TestRouteDeadlineAndRejectedMetadataAdapter(t *testing.T) {
	g := graphFixture(t, 53)
	budget := NewPassBudget()
	budget.Deadline = time.Now().Add(-time.Second)
	step, err := NextHop(t.Context(), g.builds[0], Position{Anchor: true}, g.metadata[52], RouteCursor{}, g.source(), budget)
	if err != nil || !step.Exhausted || len(g.reads) != 0 {
		t.Fatalf("expired budget = %v, %v", step, err)
	}
	source := g.source()
	source.VerifyMetadata = func(context.Context, AssetRef, []byte) error { return errors.New("wrong signer identity") }
	if _, err := NextHop(t.Context(), g.builds[0], Position{Anchor: true}, g.metadata[52], RouteCursor{}, source, NewPassBudget()); err == nil {
		t.Fatal("accepted a rejected metadata signature")
	}
}

func TestRoutePageWalkSurvivesResidentCacheEviction(t *testing.T) {
	g := &routeGraph{documents: make(map[AssetRef][]byte), reads: make(map[AssetRef]int)}
	page0 := addSparsePage(t, g, 0, "v0.2.1000", nil)
	page1 := addSparsePage(t, g, 1, "v0.2.1001", map[uint64]AssetRef{0: page0})
	previous := make(map[uint64]AssetRef)
	for distance := uint64(1); distance <= 257; distance *= 2 {
		previous[257-distance] = AssetRef{Tag: "v0.2.1001", Name: "prior.json", SHA256: strings.Repeat("a", 64)}
	}
	previous[1] = page1
	page257 := addSparsePage(t, g, 257, "v0.2.2000", previous)
	pages := make(map[uint64]PageLocation)
	for page := uint64(257); page <= 512; page++ {
		pages[page] = PageLocation{Ref: page257, Full: true}
	}
	step := RouteStep{Cursor: RouteCursor{Pages: pages, Search: &RouteSearch{}}}
	target := UpdateMetadata{Epoch: 1, Position: Position{Anchor: true, Ordinal: 513 * 64}}
	for pass := 0; pass < 3; pass++ {
		step.Budget = NewPassBudget()
		step.Budget.Discovery = 1
		entry, err := routeEntry(t.Context(), 0, target, &step, g.source())
		if pass == 2 && (err != nil || entry.Ordinal != 0) {
			t.Fatalf("evicted walk failed: %v", err)
		}
		if pass == 2 {
			break
		}
		if !errors.Is(err, ErrPassExhausted) {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if len(step.Cursor.Pages) > 256 {
			t.Fatal("resident cache exceeded 256 locations")
		}
		data, _ := json.Marshal(step.Cursor)
		if err := json.Unmarshal(data, &step.Cursor); err != nil {
			t.Fatal(err)
		}
	}
	for ref, count := range g.reads {
		if count != 1 {
			t.Fatalf("repeated authenticated walk fetch %s: %d", ref.Name, count)
		}
	}
}

func addSparsePage(t *testing.T, g *routeGraph, page uint64, tag string, previous map[uint64]AssetRef) AssetRef {
	t.Helper()
	entries := make([]AnchorEntry, 64)
	for index := range entries {
		ordinal := page*64 + uint64(index)
		entries[index] = AnchorEntry{Ordinal: ordinal, Version: fmt.Sprintf("v0.1.%d", ordinal), Metadata: strings.Repeat("a", 64), PublishedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	}
	document, err := BuildAnchorPage(1, page, entries, previous, tag)
	if err != nil {
		t.Fatal(err)
	}
	data, err := EncodeAnchorPage(document, tag)
	if err != nil {
		t.Fatal(err)
	}
	return g.put(tag, fmt.Sprintf("page-%d.json", page), data)
}

func TestRouteFailedExactEdgeChoosesAlternate(t *testing.T) {
	g := graphFixture(t, 53)
	failed := FailedEdge{FromDigest: g.builds[0].Digest, ToDigest: g.builds[24].Digest, Platform: g.builds[0].Platform}
	step, _ := graphNext(t, g, 0, RouteCursor{Failed: []FailedEdge{failed}})
	if step.Next == nil || step.Next.Position.Ordinal != 23 {
		t.Fatalf("alternate = %v", step.Next)
	}
	failed.Platform = Platform{OS: "darwin", Arch: "arm64"}
	step, _ = graphNext(t, g, 0, RouteCursor{Failed: []FailedEdge{failed}})
	if step.Next == nil || step.Next.Position.Ordinal != 24 {
		t.Fatal("failure escaped exact platform edge")
	}
}

func TestRouteRefusesCorruptReceiptIdentityAndSourceRanges(t *testing.T) {
	for _, scenario := range []string{"receipt bytes", "manifest identity", "receipt identity", "source state", "source worker", "signature"} {
		t.Run(scenario, func(t *testing.T) { testRouteCorruption(t, scenario) })
	}
}

func testRouteCorruption(t *testing.T, scenario string) {
	t.Helper()
	g := graphFixture(t, 3)
	target := g.metadata[2]
	from := g.builds[0]
	read := g.source()
	manifestRef := AssetRef{Tag: target.Version, Name: ManifestAssetName, SHA256: target.Manifest}
	manifest, _ := decodeManifest(g.documents[manifestRef])
	transition := manifest.Compatibility.Transitions[0]
	proofRef := AssetRef{Tag: target.Version, Name: transition.Proof + ".json", SHA256: transition.Proof}
	switch scenario {
	case "receipt bytes":
		g.documents[proofRef] = append(g.documents[proofRef], ' ')
	case "source state":
		from.StateVersion = 17
	case "source worker":
		from.WorkerProtocol = 2
	case "manifest identity":
		manifest.Version = "v0.1.2"
		data, _ := json.Marshal(manifest)
		ref := g.put(target.Version, ManifestAssetName, data)
		target.Manifest = ref.SHA256
	case "receipt identity":
		var receipt TransitionReceipt
		_ = json.Unmarshal(g.documents[proofRef], &receipt)
		receipt.FromDigest = strings.Repeat("f", 64)
		data, _ := json.Marshal(receipt)
		ref := g.put(target.Version, digestBytes(data)+".json", data)
		manifest.Compatibility.Transitions[0].Proof = ref.SHA256
		data, _ = json.Marshal(manifest)
		ref = g.put(target.Version, ManifestAssetName, data)
		target.Manifest = ref.SHA256
	case "signature":
		ref := AssetRef{Tag: target.Version, Name: UpdateMetadataAssetName, SHA256: digestBytes([]byte("bad"))}
		if _, err := AuthenticateMetadata(t.Context(), DocumentSource{}, ref, []byte("bad"), 1); err == nil {
			t.Fatal("missing signature verifier accepted")
		}
		return
	}
	if _, err := NextHop(t.Context(), from, Position{Anchor: true}, target, RouteCursor{}, read, NewPassBudget()); err == nil {
		t.Fatal("accepted invalid edge")
	}
}

func catchupOrdinals(t *testing.T, g *routeGraph) []uint64 {
	t.Helper()
	from := uint64(0)
	var got []uint64
	for from < g.metadata[len(g.metadata)-1].Position.Ordinal {
		step, _ := graphNext(t, g, from, RouteCursor{})
		if step.Next == nil {
			t.Fatal("route waited")
		}
		from = step.Next.Position.Ordinal
		got = append(got, from)
	}
	return got
}
