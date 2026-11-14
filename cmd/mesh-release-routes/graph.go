package main

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/shaul/mesh/internal/release"
)

var proofPlatforms = []release.Platform{{OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"}, {OS: "darwin", Arch: "arm64"}}

type edgeKey struct {
	From, To string
	Platform release.Platform
}

type verifiedGraph map[edgeKey]release.Transition

func buildGraph(directory string, history []publishedEvidence) (verifiedGraph, error) {
	graph := verifiedGraph{}
	sources := sourceArtifacts(history)
	for _, target := range history {
		if target.Manifest.Version == "" {
			continue
		}
		if err := addIndexedEdges(directory, sources, target, graph); err != nil {
			return nil, err
		}
	}
	return graph, nil
}

type artifactIdentity struct {
	Platform release.Platform
	Digest   string
}

func sourceArtifacts(history []publishedEvidence) map[artifactIdentity]publishedEvidence {
	sources := map[artifactIdentity]publishedEvidence{}
	for _, item := range history {
		for _, artifact := range item.Manifest.Artifacts {
			sources[artifactIdentity{Platform: artifact.Platform, Digest: artifact.BinarySHA256}] = item
		}
	}
	return sources
}

func addIndexedEdges(directory string, sources map[artifactIdentity]publishedEvidence, target publishedEvidence, graph verifiedGraph) error {
	for _, transition := range target.Manifest.Compatibility.Transitions {
		source, exists := sources[artifactIdentity{Platform: transition.Platform, Digest: transition.FromDigest}]
		if !exists {
			continue
		}
		if compareTags(source.Tag, target.Tag) >= 0 {
			return errors.New("publisher: graph contains a non-forward edge")
		}
		verified, err := verifyHistoricalEdge(directory, source, target, transition.Platform)
		if err != nil {
			return err
		}
		graph[edgeKey{From: source.Tag, To: target.Tag, Platform: transition.Platform}] = verified
	}
	return nil
}

func addTargetEdges(directory string, history []publishedEvidence, target publishedEvidence, graph verifiedGraph) error {
	for _, source := range history {
		if source.Manifest.Version == "" || compareTags(source.Tag, target.Tag) >= 0 {
			continue
		}
		if err := addSourceEdges(directory, source, target, graph); err != nil {
			return err
		}
	}
	return nil
}

func addSourceEdges(directory string, source, target publishedEvidence, graph verifiedGraph) error {
	for _, platform := range proofPlatforms {
		from, err := source.Manifest.Artifact(platform)
		if err != nil {
			return fmt.Errorf("publisher: source %s: %w", source.Tag, err)
		}
		to, err := target.Manifest.Artifact(platform)
		if err != nil {
			return fmt.Errorf("publisher: target %s: %w", target.Tag, err)
		}
		if _, exists := findTransition(target.Manifest, from.BinarySHA256, to.BinarySHA256, platform); !exists {
			continue
		}
		transition, err := verifyHistoricalEdge(directory, source, target, platform)
		if err != nil {
			return err
		}
		graph[edgeKey{From: source.Tag, To: target.Tag, Platform: platform}] = transition
	}
	return nil
}

func (graph verifiedGraph) covers(source, target string) bool {
	for _, platform := range proofPlatforms {
		if _, exists := graph[edgeKey{From: source, To: target, Platform: platform}]; !exists {
			return false
		}
	}
	return true
}

func seedLadder(policy upgradePolicy, history []publishedEvidence, graph verifiedGraph, at time.Time) ([]string, error) {
	available := manifestHistory(history)
	if len(available) < policy.WindowAnchors {
		return nil, errors.New("publisher: insufficient published manifest history for seed ladder")
	}
	cutoff := at.AddDate(0, 0, -policy.WindowDays)
	if policy.SeedAgeFloor.After(cutoff) {
		cutoff = policy.SeedAgeFloor
	}
	current := len(available) - 1
	seeds := []string{available[current].Tag}
	for current > 0 && (len(seeds) < policy.WindowAnchors || available[current].PublishedAt.After(cutoff)) {
		next := previousSeed(available, current, graph)
		if next == current {
			return nil, fmt.Errorf("publisher: no verified seed path into %s", available[current].Tag)
		}
		current = next
		seeds = append(seeds, available[current].Tag)
	}
	if len(seeds) < policy.WindowAnchors || available[current].PublishedAt.After(cutoff) {
		return nil, errors.New("publisher: seed history cannot satisfy its count and initial age floor")
	}
	seeds = append(seeds, policy.RequiredRollback...)
	slices.SortFunc(seeds, compareTags)
	return slices.Compact(seeds), nil
}

func previousSeed(history []publishedEvidence, current int, graph verifiedGraph) int {
	first := current
	for first > 0 && graph.covers(history[first-1].Tag, history[current].Tag) {
		first--
	}
	return first
}

func manifestHistory(history []publishedEvidence) []publishedEvidence {
	return slices.DeleteFunc(slices.Clone(history), func(item publishedEvidence) bool { return item.Manifest.Version == "" })
}

func setSeedRanges(anchors []release.AnchorEntry, graph verifiedGraph) {
	minimum := uint64(0)
	for index := range anchors {
		if anchors[index].Metadata != "" {
			minimum = anchors[index].AnchorsFrom
			continue
		}
		first := index
		for first > 0 && anchors[first].Ordinal > minimum && graph.covers(anchors[first-1].Version, anchors[index].Version) {
			first--
		}
		anchors[index].AnchorsFrom = anchors[first].Ordinal
		minimum = anchors[first].Ordinal
	}
}

func longestPath(history []publishedEvidence, target string, graph verifiedGraph, recent []string) (int, error) {
	longest := 0
	for _, platform := range proofPlatforms {
		length, err := platformPaths(history, target, graph, recent, platform)
		if err != nil {
			return 0, err
		}
		longest = max(longest, length)
	}
	return longest, nil
}

func platformPaths(history []publishedEvidence, target string, graph verifiedGraph, recent []string, platform release.Platform) (int, error) {
	distances := map[string]int{target: 0}
	longest := 0
	for _, item := range slices.Backward(history) {
		if item.Manifest.Version == "" || item.Tag == target {
			continue
		}
		distance := shortestDistance(item.Tag, platform, graph, distances)
		if distance == 0 {
			return 0, fmt.Errorf("publisher: %s has no verified %s path to %s", item.Tag, platform, target)
		}
		if slices.Contains(recent, item.Tag) && distance > 2 {
			return 0, fmt.Errorf("publisher: recent %s requires %d hops on %s", item.Tag, distance, platform)
		}
		distances[item.Tag] = distance
		longest = max(longest, distance)
	}
	return longest, nil
}

func shortestDistance(source string, platform release.Platform, graph verifiedGraph, distances map[string]int) int {
	best := 0
	for edge := range graph {
		if edge.From != source || edge.Platform != platform {
			continue
		}
		distance, exists := distances[edge.To]
		if exists && (best == 0 || distance+1 < best) {
			best = distance + 1
		}
	}
	return best
}
