package main

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/shaul/mesh/internal/release"
)

type upgradePolicy struct {
	Schema           int       `json:"schema"`
	AnchorReleases   int       `json:"anchorReleases"`
	WindowAnchors    int       `json:"windowAnchors"`
	WindowDays       int       `json:"windowDays"`
	RequiredRollback []string  `json:"requiredRollback"`
	Seeds            []string  `json:"seeds"`
	SeedAgeFloor     time.Time `json:"seedAgeFloor"`
}

type sourceSelection struct {
	Sources   []string              `json:"sources"`
	Anchors   []release.AnchorEntry `json:"anchors"`
	Position  release.Position      `json:"position"`
	Direct    release.DirectSources `json:"direct"`
	Oldest    time.Time             `json:"oldest"`
	Baselines []baselineArtifact    `json:"baselines"`
}

type baselineArtifact struct {
	Version  string           `json:"version"`
	Artifact release.Artifact `json:"artifact"`
	Rollback bool             `json:"rollback"`
}

func selectBaselines(selection *sourceSelection, history []publishedEvidence, policy upgradePolicy) error {
	for _, version := range selection.Sources {
		index := slices.IndexFunc(history, func(item publishedEvidence) bool { return item.Tag == version })
		if index < 0 || history[index].Manifest.Version == "" {
			return fmt.Errorf("publisher: selected source %s has no verified manifest", version)
		}
		item := history[index]
		for _, artifact := range item.Manifest.Artifacts {
			selection.Baselines = append(selection.Baselines, baselineArtifact{Version: version, Artifact: artifact, Rollback: updaterSource(item) || slices.Contains(policy.RequiredRollback, version)})
		}
	}
	return nil
}

func (policy upgradePolicy) validate() error {
	if policy.Schema != 1 || policy.AnchorReleases != 16 || policy.WindowAnchors < 16 || policy.WindowDays < 30 || policy.SeedAgeFloor.IsZero() {
		return errors.New("publisher: invalid anchor cadence, window or initial history floor")
	}
	if !slices.Equal(policy.RequiredRollback, []string{"v0.1.249", "v0.1.255", "v0.1.257"}) {
		return errors.New("publisher: required fleet rollback sources changed")
	}
	return nil
}

func selectSources(policy upgradePolicy, history []publishedEvidence, candidate string, at time.Time, epoch uint32, waypoint string) (sourceSelection, error) {
	if err := policy.validate(); err != nil {
		return sourceSelection{}, err
	}
	anchors, err := historyAnchors(policy, history, epoch, waypoint)
	if err != nil {
		return sourceSelection{}, err
	}
	if len(anchors) == 0 || epoch == 1 && len(anchors) < policy.WindowAnchors {
		return sourceSelection{}, errors.New("publisher: seed ladder has fewer than sixteen verified anchors")
	}
	last := anchors[len(anchors)-1]
	if at.Before(last.PublishedAt) {
		return sourceSelection{}, errors.New("publisher: candidate publication time precedes its newest anchor")
	}
	first := windowStart(anchors, at, policy)
	direct := release.DirectSources{AnchorsFrom: anchors[first].Ordinal, Since: []string{}}
	sources := anchorVersions(anchors[first:])
	for _, item := range history {
		if compareTags(item.Tag, last.Version) <= 0 {
			continue
		}
		if item.Manifest.Version == "" {
			return sourceSelection{}, fmt.Errorf("publisher: required recent source %s lacks its manifest", item.Tag)
		}
		direct.Since = append(direct.Since, item.Tag)
		sources = append(sources, item.Tag)
	}
	sources = append(sources, policy.RequiredRollback...)
	if epoch == 1 && !slices.ContainsFunc(history, func(item publishedEvidence) bool { return item.Metadata != nil }) {
		sources = append(sources, policy.Seeds...)
	}
	slices.SortFunc(sources, compareTags)
	sources = slices.Compact(sources)
	position := release.Position{Ordinal: last.Ordinal}
	if differentISOWeek(last.PublishedAt, at) || len(direct.Since)+1 >= policy.AnchorReleases || waypoint == candidate {
		position.Anchor = true
		position.Ordinal++
	}
	return sourceSelection{Sources: sources, Anchors: anchors, Position: position, Direct: direct, Oldest: anchors[first].PublishedAt}, nil
}

func windowStart(anchors []release.AnchorEntry, at time.Time, policy upgradePolicy) int {
	first := max(0, len(anchors)-policy.WindowAnchors)
	cutoff := at.AddDate(0, 0, -policy.WindowDays)
	for first > 0 && anchors[first].PublishedAt.After(cutoff) {
		first--
	}
	return first
}

func differentISOWeek(left, right time.Time) bool {
	leftYear, leftWeek := left.UTC().ISOWeek()
	rightYear, rightWeek := right.UTC().ISOWeek()
	return leftYear != rightYear || leftWeek != rightWeek
}

func anchorVersions(anchors []release.AnchorEntry) []string {
	versions := make([]string, len(anchors))
	for index, anchor := range anchors {
		versions[index] = anchor.Version
	}
	return versions
}

func compareTags(left, right string) int {
	order, _ := release.CompareVersions(left, right)
	return order
}

func historyAnchors(policy upgradePolicy, history []publishedEvidence, epoch uint32, waypoint string) ([]release.AnchorEntry, error) {
	seeds := make(map[string]bool, len(policy.Seeds))
	for _, seed := range policy.Seeds {
		seeds[seed] = true
	}
	anchors := []release.AnchorEntry{}
	for _, item := range history {
		entry, include := historicalAnchor(item, seeds, epoch, waypoint)
		if !include {
			continue
		}
		if entry.Metadata == "" {
			entry.Ordinal = uint64(len(anchors))
		}
		if entry.Ordinal != uint64(len(anchors)) {
			return nil, fmt.Errorf("publisher: %s has a non-dense epoch position", item.Tag)
		}
		anchors = append(anchors, entry)
	}
	return anchors, nil
}

func historicalAnchor(item publishedEvidence, seeds map[string]bool, epoch uint32, waypoint string) (release.AnchorEntry, bool) {
	if item.Metadata != nil && item.Metadata.Epoch == epoch && item.Metadata.Position.Anchor {
		metadata := item.Metadata
		return release.AnchorEntry{Ordinal: metadata.Position.Ordinal, Version: item.Tag, Metadata: sum(item.MetadataBytes), AnchorsFrom: metadata.Direct.AnchorsFrom, PublishedAt: metadata.PublishedAt}, true
	}
	if item.Tag == waypoint {
		return release.AnchorEntry{Version: item.Tag, Manifest: sum(item.ManifestBytes), PublishedAt: item.PublishedAt}, true
	}
	if epoch != 1 || !seeds[item.Tag] || item.Metadata != nil {
		return release.AnchorEntry{}, false
	}
	return release.AnchorEntry{Version: item.Tag, Manifest: sum(item.ManifestBytes), PublishedAt: item.PublishedAt}, true
}
