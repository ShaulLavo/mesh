package main

import (
	"errors"
	"fmt"
	"slices"

	"github.com/shaul/mesh/internal/release"
)

type workerArtifact struct {
	Version  string           `json:"version"`
	Commit   string           `json:"commit"`
	Protocol int              `json:"protocol"`
	Artifact release.Artifact `json:"artifact"`
}

func selectWorkerRegistry(policy upgradePolicy, history []publishedEvidence) ([]workerArtifact, error) {
	if _, err := release.CompareVersions(policy.WorkerHistoryFrom, policy.WorkerHistoryFrom); err != nil {
		return nil, fmt.Errorf("publisher: worker history floor: %w", err)
	}
	versions := slices.Clone(policy.Workers)
	for _, item := range history {
		if compareTags(item.Tag, policy.WorkerHistoryFrom) >= 0 {
			versions = append(versions, item.Tag)
		}
	}
	slices.SortFunc(versions, compareTags)
	versions = slices.Compact(versions)
	workers := []workerArtifact{}
	for _, version := range versions {
		index := slices.IndexFunc(history, func(item publishedEvidence) bool { return item.Tag == version })
		if index < 0 || history[index].Manifest.Version == "" {
			return nil, fmt.Errorf("publisher: retained worker %s lacks its exact published manifest", version)
		}
		manifest := history[index].Manifest
		for _, artifact := range manifest.Artifacts {
			workers = append(workers, workerArtifact{Version: version, Commit: manifest.Commit, Protocol: manifest.Compatibility.WorkerWrite, Artifact: artifact})
		}
	}
	if len(workers) == 0 {
		return nil, errors.New("publisher: retained worker registry is empty")
	}
	return workers, nil
}

func requireWorkerCoverage(proof release.RollbackProof, source publishedEvidence, platform release.Platform, workers []workerArtifact) error {
	for _, worker := range workers {
		if worker.Artifact.Platform != platform || compareTags(worker.Version, source.Tag) > 0 {
			continue
		}
		if !slices.Contains(proof.Workers, worker.Artifact.BinarySHA256) {
			return fmt.Errorf("publisher: rollback from %s lacks retained worker %s on %s", source.Tag, worker.Version, platform)
		}
	}
	return nil
}
