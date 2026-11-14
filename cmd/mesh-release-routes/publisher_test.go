package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/release"
)

func testPolicy() upgradePolicy {
	return upgradePolicy{Schema: 1, AnchorReleases: 16, WindowAnchors: 16, WindowDays: 30, SeedAgeFloor: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), RequiredRollback: []string{"v0.1.249", "v0.1.255", "v0.1.257"}}
}

func fixtureRelease(t *testing.T, directory string, patch int, sources []publishedEvidence) publishedEvidence {
	t.Helper()
	item := publishedEvidence{Tag: fmt.Sprintf("v0.1.%d", patch), PublishedAt: time.Date(2026, 1, 1, 0, patch, 0, 0, time.UTC)}
	manifest := release.Manifest{Schema: 1, Version: item.Tag, Commit: strings.Repeat("a", 40), Compatibility: release.Compatibility{StateReadMin: 16, StateReadMax: 16, StateWrite: 16, WorkerMin: 1, WorkerMax: 1, WorkerWrite: 1, JournalVersion: 1}}
	for _, platform := range proofPlatforms {
		digest := sum([]byte(item.Tag + platform.String()))
		manifest.Artifacts = append(manifest.Artifacts, release.Artifact{Platform: platform, Archive: "mesh_" + platform.OS + "_" + platform.Arch + ".tar.gz", SHA256: sum([]byte("archive" + digest)), BinarySHA256: digest})
		for _, source := range sources {
			transition := fixtureTransition(t, directory, source, item.Tag, digest, platform, manifest.Compatibility)
			manifest.Compatibility.Transitions = append(manifest.Compatibility.Transitions, transition)
		}
	}
	data := fixtureJSON(t, manifest)
	item.Assets = []retainedAsset{{Name: "mesh-release.json", SHA256: sum(data)}}
	for _, artifact := range manifest.Artifacts {
		item.Assets = append(item.Assets, retainedAsset{Name: artifact.Archive, SHA256: artifact.SHA256})
	}
	fixtureWrite(t, directory, item.Tag, "mesh-release.json", data)
	if err := loadPublished(directory, &item); err != nil {
		t.Fatal(err)
	}
	return item
}

func fixtureTransition(t *testing.T, directory string, source publishedEvidence, target, digest string, platform release.Platform, compatibility release.Compatibility) release.Transition {
	t.Helper()
	from, err := source.Manifest.Artifact(platform)
	if err != nil {
		t.Fatal(err)
	}
	receipt := release.TransitionReceipt{Schema: 1, Platform: platform, FromDigest: from.BinarySHA256, ToDigest: digest, StateReadMin: compatibility.StateReadMin, StateReadMax: compatibility.StateReadMax, StateWrite: compatibility.StateWrite, WorkerMin: 1, WorkerMax: 1, WorkerWrite: 1, JournalVersion: 1, CandidateOpenedRetainedState: true, SessionsPreserved: true, RecoveryRecordsPreserved: true}
	data := fixtureJSON(t, receipt)
	if compareTags(target, "v0.1.232") < 0 {
		var fields map[string]any
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		delete(fields, "candidateOpenedRetainedState")
		fields["retainedOpenedCandidateState"] = true
		data = fixtureJSON(t, fields)
	}
	proof := sum(data)
	fixtureWrite(t, directory, target, proof+".json", data)
	return release.Transition{FromDigest: from.BinarySHA256, ToDigest: digest, Platform: platform, Proof: proof}
}

func fixtureJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func fixtureWrite(t *testing.T, directory, tag, name string, data []byte) {
	t.Helper()
	if err := writePublisherFile(filepath.Join(directory, tag), name, data); err != nil {
		t.Fatal(err)
	}
}

func TestPublisherEdgesRequireExactReceiptBytesAndCapabilities(t *testing.T) {
	for _, failure := range []string{"missing", "corrupt", "identity", "compatibility", "source-range"} {
		t.Run(failure, func(t *testing.T) {
			testInvalidPublisherEdge(t, failure)
		})
	}
}

func testInvalidPublisherEdge(t *testing.T, failure string) {
	t.Helper()
	directory := t.TempDir()
	source := fixtureRelease(t, directory, 248, nil)
	target := fixtureRelease(t, directory, 249, []publishedEvidence{source})
	transition := target.Manifest.Compatibility.Transitions[0]
	path := filepath.Join(directory, target.Tag, transition.Proof+".json")
	data, err := readLocal(path, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	switch failure {
	case "missing":
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	case "corrupt":
		fixtureWrite(t, directory, target.Tag, transition.Proof+".json", append(data, ' '))
	case "identity", "compatibility":
		var receipt release.TransitionReceipt
		if err := json.Unmarshal(data, &receipt); err != nil {
			t.Fatal(err)
		}
		if failure == "identity" {
			receipt.ToDigest = strings.Repeat("f", 64)
		} else {
			receipt.StateWrite++
		}
		data = fixtureJSON(t, receipt)
		target.Manifest.Compatibility.Transitions[0].Proof = sum(data)
		fixtureWrite(t, directory, target.Tag, sum(data)+".json", data)
	case "source-range":
		source.Manifest.Compatibility.StateWrite = 15
		source.Manifest.Compatibility.StateReadMin = 15
	default:
		t.Fatal("unhandled test case")
	}
	if _, err := verifyHistoricalEdge(directory, source, target, proofPlatforms[0]); err == nil {
		t.Fatal("invalid edge accepted")
	}
}

func TestPublisherPre232ContractChecksActualOldRange(t *testing.T) {
	directory := t.TempDir()
	source := fixtureRelease(t, directory, 150, nil)
	target := fixtureRelease(t, directory, 151, []publishedEvidence{source})
	if _, err := verifyHistoricalEdge(directory, source, target, proofPlatforms[0]); err != nil {
		t.Fatal(err)
	}
	target.Manifest.Compatibility.StateWrite = 15
	if _, err := verifyHistoricalEdge(directory, source, target, proofPlatforms[0]); err == nil {
		t.Fatal("pre232 receipt no longer matches the target write version")
	}
}

func TestPublisherWindowCountAndAgeAreIndependent(t *testing.T) {
	at := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	anchors := make([]release.AnchorEntry, 40)
	for index := range anchors {
		anchors[index] = release.AnchorEntry{Ordinal: uint64(index), PublishedAt: at.AddDate(0, 0, index-39)}
	}
	first := windowStart(anchors, at, testPolicy())
	if first != 9 || !anchors[first].PublishedAt.Equal(at.AddDate(0, 0, -30)) {
		t.Fatalf("exactly thirty days: %d", first)
	}
	anchors[9].PublishedAt = at.AddDate(0, 0, -29)
	if got := windowStart(anchors, at, testPolicy()); got != 8 {
		t.Fatalf("one day short: %d", got)
	}
	for index := range anchors {
		anchors[index].PublishedAt = at.Add(time.Duration(index-39) * 16 * 24 * time.Hour / 20)
	}
	if got := windowStart(anchors, at, testPolicy()); got != 1 || anchors[got].PublishedAt.After(at.AddDate(0, 0, -30)) {
		t.Fatalf("twenty releases per day did not widen to thirty days: %d", got)
	}
}

func TestPublisherCadenceUsesUTCISOWeekAndSixteenthCandidate(t *testing.T) {
	sunday := time.Date(2026, 11, 8, 23, 0, 0, 0, time.UTC)
	if !differentISOWeek(sunday, sunday.Add(time.Hour)) || differentISOWeek(sunday, sunday.Add(-time.Hour)) {
		t.Fatal("ISO week cadence is wrong")
	}
	policy := testPolicy()
	history := make([]publishedEvidence, 30)
	for index := range history {
		tag := fmt.Sprintf("v0.1.%d", 300+index)
		history[index] = publishedEvidence{Tag: tag, PublishedAt: sunday.Add(time.Duration(index) * time.Minute), Manifest: release.Manifest{Version: tag}}
		if index < 16 {
			policy.Seeds = append(policy.Seeds, tag)
		}
	}
	selection, err := selectSources(policy, history, "v0.1.330", sunday.Add(31*time.Minute), 1, "")
	if err != nil || selection.Position.Anchor {
		t.Fatalf("fifteenth candidate: %+v, %v", selection, err)
	}
	history = append(history, publishedEvidence{Tag: "v0.1.330", PublishedAt: sunday.Add(32 * time.Minute), Manifest: release.Manifest{Version: "v0.1.330"}})
	selection, err = selectSources(policy, history, "v0.1.331", sunday.Add(33*time.Minute), 1, "")
	if err != nil || !selection.Position.Anchor || selection.Position.Ordinal != 16 {
		t.Fatalf("sixteenth candidate: %+v, %v", selection, err)
	}
}

func TestPublisherRetentionRefusesDeletionAndMutation(t *testing.T) {
	expected := []publishedEvidence{{Tag: "v0.1.1", Assets: []retainedAsset{{Name: "proof.json", SHA256: strings.Repeat("a", 64)}}}}
	if err := checkRetained(expected, expected); err != nil {
		t.Fatal(err)
	}
	if err := checkRetained(expected, nil); err == nil {
		t.Fatal("missing retained release accepted")
	}
	changed := slices.Clone(expected)
	changed[0].Assets = []retainedAsset{{Name: "proof.json", SHA256: strings.Repeat("b", 64)}}
	if err := checkRetained(expected, changed); err == nil {
		t.Fatal("changed retained bytes accepted")
	}
}

func TestPublisherPagesAreBoundedAndAcyclic(t *testing.T) {
	anchors := make([]release.AnchorEntry, 130)
	for index := range anchors {
		anchors[index] = release.AnchorEntry{Ordinal: uint64(index), Version: fmt.Sprintf("v0.1.%d", index+1), Manifest: strings.Repeat("a", 64), PublishedAt: time.Date(2026, 1, 1, 0, index, 0, 0, time.UTC)}
	}
	directory := t.TempDir()
	ref, err := publishPages(publicationInput{Version: "v0.1.131", Epoch: 1}, anchors, directory, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err := readLocal(filepath.Join(directory, ref.Name), release.MaxAnchorPageBytes)
	if err != nil {
		t.Fatal(err)
	}
	page, err := release.DecodeAnchorPage(data, ref)
	if err != nil || page.Page != 2 || len(page.Entries) != 2 || len(page.Back) != 2 {
		t.Fatalf("page: %+v, %v", page, err)
	}
	anchors[129].Version = "v0.1.131"
	if _, err := publishPages(publicationInput{Version: "v0.1.131", Epoch: 1}, anchors, t.TempDir(), nil); err == nil {
		t.Fatal("current target admitted to its own page")
	}
}

func TestPublisherEpochWaypointHasDenseNewSourcePosition(t *testing.T) {
	waypoint := publishedEvidence{Tag: "v0.1.400", PublishedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), Manifest: release.Manifest{Version: "v0.1.400"}, Metadata: &release.UpdateMetadata{Epoch: 1, Position: release.Position{Anchor: true, Ordinal: 200}}}
	selection, err := selectSources(testPolicy(), []publishedEvidence{waypoint}, "v0.1.401", waypoint.PublishedAt.Add(time.Hour), 2, waypoint.Tag)
	if err != nil || len(selection.Anchors) != 1 || selection.Anchors[0].Ordinal != 0 || selection.Position.Ordinal != 0 {
		t.Fatalf("new epoch waypoint: %+v, %v", selection, err)
	}
}

func TestPublisherRecordedPre232ReceiptUsesItsOwnDecoder(t *testing.T) {
	data, err := readLocal("testdata/pre232/transition.json", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Transition    release.Transition    `json:"transition"`
		Compatibility release.Compatibility `json:"compatibility"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	receipt, err := readLocal("testdata/pre232/"+fixture.Transition.Proof+".json", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := verifyPre232TransitionReceipt(receipt, fixture.Transition, fixture.Compatibility)
	if err != nil || proof.CandidateOpenedRetainedState {
		t.Fatalf("historical evidence: %+v, %v", proof, err)
	}
	if _, err := release.VerifyTransitionReceipt(receipt, fixture.Transition, fixture.Compatibility); err == nil {
		t.Fatal("current reader accepted the historical format")
	}
}

func TestPublisherImmutablePageReuseAndRetainedMutation(t *testing.T) {
	entry := release.AnchorEntry{Ordinal: 0, Version: "v0.1.1", Manifest: strings.Repeat("a", 64), PublishedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	prior := retainedPage{Ref: release.AssetRef{Tag: "v0.1.2", Name: "page.json", SHA256: strings.Repeat("b", 64)}, Page: release.AnchorPage{Entries: []release.AnchorEntry{entry}}}
	ref, reuse, err := reusePage(prior, []release.AnchorEntry{entry})
	if err != nil || !reuse || ref != prior.Ref {
		t.Fatalf("reuse: %+v, %v, %v", ref, reuse, err)
	}
	entry.Manifest = strings.Repeat("c", 64)
	if _, _, err := reusePage(prior, []release.AnchorEntry{entry}); err == nil {
		t.Fatal("changed retained anchor accepted")
	}
}

func TestPublisherMissingOrCorruptSignatureIsNotPublicationEvidence(t *testing.T) {
	directory := t.TempDir()
	item := fixtureRelease(t, directory, 300, nil)
	item.Metadata = &release.UpdateMetadata{}
	if err := authenticatePublished(directory, item, release.EmbeddedTrustRoot()); err == nil {
		t.Fatal("unsigned metadata accepted")
	}
	envelope := []byte(`{"schema":1,"entries":[]}`)
	fixtureWrite(t, directory, item.Tag, "mesh-update.sig.json", envelope)
	item.Assets = append(item.Assets, retainedAsset{Name: "mesh-update.sig.json", SHA256: sum(envelope)})
	page := []byte(`{}`)
	fixtureWrite(t, directory, item.Tag, "page.json", page)
	item.Metadata.Page = release.AssetRef{Tag: item.Tag, Name: "page.json", SHA256: sum(page)}
	if err := authenticatePublished(directory, item, release.EmbeddedTrustRoot()); err == nil {
		t.Fatal("invalid signature accepted")
	}
}

func TestPublisherEpochRequiresAnOldFormatBoundWaypoint(t *testing.T) {
	directory := t.TempDir()
	item := fixtureRelease(t, directory, 400, nil)
	item.Metadata = &release.UpdateMetadata{Epoch: 1, Position: release.Position{Anchor: true, Ordinal: 200}}
	item.MetadataBytes = fixtureJSON(t, item.Metadata)
	root := release.DiscoveryRoot{Format: 1, Epoch: 1, Head: release.AssetRef{Tag: item.Tag, Name: release.UpdateMetadataAssetName, SHA256: sum(item.MetadataBytes)}}
	data, err := release.EncodeDiscoveryRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	fixtureWrite(t, directory, item.Tag, release.DiscoveryRootAssetName, data)
	previous := release.AssetRef{Tag: item.Tag, Name: release.DiscoveryRootAssetName, SHA256: sum(data)}
	input := publicationInput{Epoch: 2, Waypoint: item.Tag, PreviousRoot: &previous}
	if err := validateEpoch(directory, input, []publishedEvidence{item}); err != nil {
		t.Fatal(err)
	}
	input.Epoch = 3
	if err := validateEpoch(directory, input, []publishedEvidence{item}); err == nil {
		t.Fatal("skipped format epoch accepted")
	}
	input.Epoch = 2
	item.Metadata.Position.Anchor = false
	if err := validateEpoch(directory, input, []publishedEvidence{item}); err == nil {
		t.Fatal("non-anchor waypoint accepted")
	}
}

func TestPublisherGraphKeepsHistoryAndRecentHopLimit(t *testing.T) {
	history := make([]publishedEvidence, 300)
	graph := verifiedGraph{}
	for index := range history {
		tag := fmt.Sprintf("v0.1.%d", index+1)
		history[index] = publishedEvidence{Tag: tag, Manifest: release.Manifest{Version: tag}}
	}
	for index := 0; index < len(history)-1; index++ {
		for _, platform := range proofPlatforms {
			graph[edgeKey{From: history[index].Tag, To: history[min(index+24, len(history)-1)].Tag, Platform: platform}] = release.Transition{}
		}
	}
	longest, err := longestPath(history, history[len(history)-1].Tag, graph, nil)
	if err != nil || longest != 13 {
		t.Fatalf("historical graph: %d, %v", longest, err)
	}
	if _, err := longestPath(history, history[len(history)-1].Tag, graph, []string{history[0].Tag}); err == nil {
		t.Fatal("recent source accepted with more than two hops")
	}
	delete(graph, edgeKey{From: history[0].Tag, To: history[24].Tag, Platform: proofPlatforms[0]})
	if _, err := longestPath(history, history[len(history)-1].Tag, graph, nil); err == nil {
		t.Fatal("historical source lost its path without blocking publication")
	}
}
