package main

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/shaul/mesh/internal/release"
)

func updaterSource(item publishedEvidence) bool {
	return item.Metadata != nil && item.Metadata.Updater != nil
}

func automaticAdmission(source, target publishedEvidence) bool {
	if !updaterSource(source) || target.Metadata == nil || source.Metadata.Updater.Epoch != target.Metadata.Epoch {
		return false
	}
	position := source.Metadata.Updater.Position
	if position.Anchor {
		return position.Ordinal >= target.Metadata.Direct.AnchorsFrom && position.Ordinal <= target.Metadata.Position.Ordinal
	}
	return slices.Contains(target.Metadata.Direct.Since, source.Tag)
}

func verifyAutomaticHistory(directory string, history []publishedEvidence, candidate publishedEvidence, forward verifiedGraph, policy upgradePolicy) error {
	byTag := make(map[string]publishedEvidence, len(history)+1)
	for _, item := range history {
		byTag[item.Tag] = item
	}
	byTag[candidate.Tag] = candidate
	automatic := verifiedGraph{}
	for key, transition := range forward {
		source, target := byTag[key.From], byTag[key.To]
		if !automaticAdmission(source, target) {
			continue
		}
		if _, _, _, err := automaticRollback(directory, source, target, transition); err != nil {
			return err
		}
		automatic[key] = transition
	}
	supported := slices.DeleteFunc(slices.Clone(history), func(item publishedEvidence) bool { return !updaterSource(item) })
	recent := recentUpdaterSources(supported, candidate.PublishedAt, policy)
	if _, err := longestPath(supported, candidate.Tag, automatic, recent); err != nil {
		return fmt.Errorf("publisher: supported updater history lost its unattended path: %w", err)
	}
	return nil
}

func recentUpdaterSources(history []publishedEvidence, at time.Time, policy upgradePolicy) []string {
	if len(history) == 0 {
		return nil
	}
	anchors := []int{}
	for index, item := range history {
		if item.Metadata.Position.Anchor {
			anchors = append(anchors, index)
		}
	}
	if len(anchors) < policy.WindowAnchors {
		return historyVersions(history)
	}
	first := len(anchors) - policy.WindowAnchors
	cutoff := at.AddDate(0, 0, -policy.WindowDays)
	for first > 0 && history[anchors[first]].PublishedAt.After(cutoff) {
		first--
	}
	return historyVersions(history[anchors[first]:])
}

func historyVersions(history []publishedEvidence) []string {
	versions := make([]string, len(history))
	for index, item := range history {
		versions[index] = item.Tag
	}
	return versions
}

func automaticRollback(directory string, source, target publishedEvidence, transition release.Transition) (release.RollbackProof, []byte, []byte, error) {
	if !automaticAdmission(source, target) {
		return release.RollbackProof{}, nil, nil, errors.New("publisher: source reader needs its compatible format waypoint first")
	}
	forward, err := readAsset(directory, release.AssetRef{Tag: target.Tag, Name: transition.Proof + ".json", SHA256: transition.Proof}, 1<<20)
	if err != nil {
		return release.RollbackProof{}, nil, nil, err
	}
	digest := ""
	for _, ref := range target.Metadata.Rollback {
		if ref.Forward == transition.Proof {
			digest = ref.Rollback
		}
	}
	if digest == "" {
		return release.RollbackProof{}, nil, nil, errors.New("publisher: permitted automatic edge lacks its bound rollback proof")
	}
	rollback, err := readAsset(directory, release.AssetRef{Tag: target.Tag, Name: digest + ".rollback.json", SHA256: digest}, 1<<20)
	if err != nil {
		return release.RollbackProof{}, nil, nil, err
	}
	proof, err := release.VerifyRollbackProof(rollback, digest, forward, transition, target.Manifest.Compatibility)
	if err != nil {
		return proof, nil, nil, fmt.Errorf("publisher: automatic rollback: %w", err)
	}
	return proof, forward, rollback, nil
}
