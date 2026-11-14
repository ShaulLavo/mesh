package main

import (
	"encoding/json"
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

func writeWorkerRegistry(directory string, retained []workerArtifact, candidate release.Manifest) error {
	workers := slices.Clone(retained)
	for _, artifact := range candidate.Artifacts {
		workers = append(workers, workerArtifact{Version: candidate.Version, Commit: candidate.Commit, Protocol: candidate.Compatibility.WorkerWrite, Artifact: artifact})
	}
	data, err := json.Marshal(workers)
	if err != nil {
		return fmt.Errorf("publisher: encode worker registry: %w", err)
	}
	if len(data) > 8<<20 {
		return errors.New("publisher: exact worker registry exceeds 8 MiB; a reviewed format epoch is required")
	}
	return writePublisherFile(directory, "mesh-workers.json", data)
}

func verifyRetainedWorkerRegistry(directory string, history []publishedEvidence, workers []workerArtifact) error {
	for _, item := range slices.Backward(history) {
		ref, exists := item.asset("mesh-workers.json")
		if !exists {
			continue
		}
		data, err := readAsset(directory, ref, 8<<20)
		if err != nil {
			return err
		}
		if err := verifyRetainedSignature(directory, item, data); err != nil {
			return err
		}
		var retained []workerArtifact
		if err := json.Unmarshal(data, &retained); err != nil {
			return fmt.Errorf("publisher: decode authenticated worker registry: %w", err)
		}
		return compareWorkerRegistries(retained, workers)
	}
	return nil
}

func compareWorkerRegistries(retained, workers []workerArtifact) error {
	for _, item := range retained {
		if !slices.Contains(workers, item) {
			return fmt.Errorf("publisher: retained worker %s on %s was removed or changed", item.Version, item.Artifact.Platform)
		}
	}
	return nil
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
