package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"time"

	"github.com/shaul/mesh/internal/release"
)

type publicationInput struct {
	Version      string              `json:"version"`
	PublishedAt  time.Time           `json:"publishedAt"`
	Epoch        uint32              `json:"epoch"`
	Trust        int                 `json:"trust"`
	Policy       upgradePolicy       `json:"policy"`
	History      []publishedEvidence `json:"history"`
	Retained     []publishedEvidence `json:"retained,omitempty"`
	PreviousRoot *release.AssetRef   `json:"previousRoot,omitempty"`
	Waypoint     string              `json:"waypoint,omitempty"`
}

func publishOperation(operation string, data []byte, directory, destination string, output io.Writer) error {
	var input publicationInput
	if err := json.Unmarshal(data, &input); err != nil {
		return fmt.Errorf("publisher: decode input: %w", err)
	}
	history, err := loadHistory(directory, input.History)
	if err != nil {
		return err
	}
	if err := validatePublication(input, history); err != nil {
		return err
	}
	if err := validateEpoch(directory, input, history); err != nil {
		return err
	}
	graph, err := buildGraph(directory, history)
	if err != nil {
		return err
	}
	if operation == "seeds" {
		seeds, err := seedLadder(input.Policy, history, graph, input.PublishedAt)
		if err != nil {
			return err
		}
		return encodePublisherOutput(output, seeds)
	}
	selection, err := selectSources(input.Policy, history, input.Version, input.PublishedAt, input.Epoch, input.Waypoint)
	if err != nil {
		return err
	}
	setSeedRanges(selection.Anchors, graph)
	if err := selectBaselines(&selection, history, input.Policy); err != nil {
		return err
	}
	if operation == "audit" {
		longest, err := longestPath(history, history[len(history)-1].Tag, graph, nil)
		if err != nil {
			return err
		}
		return encodePublisherOutput(output, map[string]any{"oldestAnchor": selection.Anchors[selection.Direct.AnchorsFrom].Version, "oldestDate": selection.Oldest, "sourceCount": len(selection.Sources), "longestChain": longest, "verifiedEdges": len(graph), "manifestCount": len(manifestHistory(history))})
	}
	if operation == "select" {
		return encodePublisherOutput(output, selection)
	}
	return publishRoutes(input, selection, history, graph, directory, destination, output)
}

func encodePublisherOutput(output io.Writer, value any) error {
	if err := json.NewEncoder(output).Encode(value); err != nil {
		return fmt.Errorf("publisher: write result: %w", err)
	}
	return nil
}

func validatePublication(input publicationInput, history []publishedEvidence) error {
	if err := input.Policy.validate(); err != nil {
		return err
	}
	if _, err := release.CompareVersions(input.Version, input.Version); err != nil {
		return fmt.Errorf("publisher: candidate version: %w", err)
	}
	if input.PublishedAt.IsZero() || input.Epoch == 0 || input.Trust <= 0 || len(history) == 0 {
		return errors.New("publisher: candidate needs an exact publication time, epoch, trust and history")
	}
	if compareTags(history[len(history)-1].Tag, input.Version) >= 0 {
		return errors.New("publisher: candidate must follow every published release")
	}
	return checkRetained(input.Retained, history)
}

func publishRoutes(input publicationInput, selection sourceSelection, history []publishedEvidence, graph verifiedGraph, directory, destination string, output io.Writer) error {
	if destination == "" {
		return errors.New("publisher: publish requires -output-dir")
	}
	if err := authenticateHistory(directory, history); err != nil {
		return err
	}
	if err := verifyPreviousRetention(directory, history); err != nil {
		return err
	}
	candidate, err := readCandidate(directory, input)
	if err != nil {
		return err
	}
	if err := addTargetEdges(directory, history, candidate, graph); err != nil {
		return err
	}
	rollback, err := verifyDirectSources(directory, input.Policy, selection, history, candidate, graph)
	if err != nil {
		return err
	}
	longest, err := longestPath(history, input.Version, graph, selection.Sources)
	if err != nil {
		return err
	}
	prior, err := loadPriorPages(directory, history, input.Epoch)
	if err != nil {
		return err
	}
	page, err := publishPages(input, selection.Anchors, destination, prior)
	if err != nil {
		return err
	}
	metadata := release.UpdateMetadata{Schema: 1, Epoch: input.Epoch, Trust: input.Trust, Version: input.Version, Manifest: sum(candidate.ManifestBytes), PublishedAt: input.PublishedAt, Position: selection.Position, Direct: selection.Direct, Rollback: rollback, Page: page}
	if err := metadata.Validate(); err != nil {
		return fmt.Errorf("publisher: generated metadata: %w", err)
	}
	return finishPublication(input, metadata, history, destination, longest, selection, output)
}

func readCandidate(directory string, input publicationInput) (publishedEvidence, error) {
	data, err := readLocal(directory+"/"+input.Version+"/mesh-release.json", manifestLimit)
	if err != nil {
		return publishedEvidence{}, err
	}
	item := publishedEvidence{Tag: input.Version, PublishedAt: input.PublishedAt, Assets: []retainedAsset{{Name: "mesh-release.json", SHA256: sum(data)}}}
	if err := json.Unmarshal(data, &item.Manifest); err != nil {
		return item, fmt.Errorf("publisher: decode candidate manifest: %w", err)
	}
	if err := item.Manifest.Validate(); err != nil {
		return item, fmt.Errorf("publisher: validate candidate manifest: %w", err)
	}
	if item.Manifest.Version != input.Version {
		return item, errors.New("publisher: candidate manifest names another release")
	}
	item.ManifestBytes = data
	return item, nil
}

func verifyDirectSources(directory string, policy upgradePolicy, selection sourceSelection, history []publishedEvidence, candidate publishedEvidence, graph verifiedGraph) ([]release.RollbackRef, error) {
	refs := []release.RollbackRef{}
	for _, version := range selection.Sources {
		index := slices.IndexFunc(history, func(item publishedEvidence) bool { return item.Tag == version })
		if index < 0 || !graph.covers(version, candidate.Tag) {
			return nil, fmt.Errorf("publisher: required direct source %s lacks a verified edge on every platform", version)
		}
		source := history[index]
		if source.Metadata == nil && !slices.Contains(policy.RequiredRollback, version) {
			continue
		}
		verified, err := verifyRollbackAssets(directory, source, candidate, graph)
		if err != nil {
			return nil, err
		}
		refs = append(refs, verified...)
	}
	return refs, nil
}

func verifyRollbackAssets(directory string, source, candidate publishedEvidence, graph verifiedGraph) ([]release.RollbackRef, error) {
	refs := make([]release.RollbackRef, 0, len(proofPlatforms))
	for _, platform := range proofPlatforms {
		transition := graph[edgeKey{From: source.Tag, To: candidate.Tag, Platform: platform}]
		forward, err := readAsset(directory, release.AssetRef{Tag: candidate.Tag, Name: transition.Proof + ".json", SHA256: transition.Proof}, 1<<20)
		if err != nil {
			return nil, err
		}
		ref, err := findRollback(directory, candidate.Tag, transition, forward, candidate.Manifest.Compatibility)
		if err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

func findRollback(directory, tag string, transition release.Transition, forward []byte, compatibility release.Compatibility) (release.RollbackRef, error) {
	index, err := readLocal(directory+"/"+tag+"/rollback-index.json", 1<<20)
	if err != nil {
		return release.RollbackRef{}, err
	}
	var refs []release.RollbackRef
	if err := json.Unmarshal(index, &refs); err != nil {
		return release.RollbackRef{}, fmt.Errorf("publisher: rollback index: %w", err)
	}
	for _, ref := range refs {
		if ref.Forward != transition.Proof {
			continue
		}
		data, err := readAsset(directory, release.AssetRef{Tag: tag, Name: ref.Rollback + ".rollback.json", SHA256: ref.Rollback}, 1<<20)
		if err != nil {
			return ref, err
		}
		if _, err := release.VerifyRollbackProof(data, ref.Rollback, forward, transition, compatibility); err != nil {
			return ref, fmt.Errorf("publisher: rollback evidence: %w", err)
		}
		return ref, nil
	}
	return release.RollbackRef{}, fmt.Errorf("publisher: missing rollback proof for %s", transition.Proof)
}

func publishPages(input publicationInput, anchors []release.AnchorEntry, directory string, prior map[uint64]retainedPage) (release.AssetRef, error) {
	previous := map[uint64]release.AssetRef{}
	var newest release.AssetRef
	for start := 0; start < len(anchors); start += release.AnchorsPerPage {
		number := uint64(start / release.AnchorsPerPage)
		entries := anchors[start:min(start+release.AnchorsPerPage, len(anchors))]
		retained, reusable, err := reusePage(prior[number], entries)
		if err != nil {
			return newest, err
		}
		if reusable {
			newest, previous[number] = retained, retained
			continue
		}
		page, err := release.BuildAnchorPage(input.Epoch, number, entries, previous, input.Version)
		if err != nil {
			return newest, fmt.Errorf("publisher: construct anchor page: %w", err)
		}
		data, err := release.EncodeAnchorPage(page, input.Version)
		if err != nil {
			return newest, fmt.Errorf("publisher: encode anchor page: %w", err)
		}
		newest = release.AssetRef{Tag: input.Version, Name: "mesh-anchors-" + sum(data) + ".json", SHA256: sum(data)}
		if err := writePublisherFile(directory, newest.Name, data); err != nil {
			return newest, err
		}
		previous[number] = newest
	}
	return newest, nil
}

func finishPublication(input publicationInput, metadata release.UpdateMetadata, history []publishedEvidence, directory string, longest int, selection sourceSelection, output io.Writer) error {
	data, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("publisher: encode metadata: %w", err)
	}
	root, err := release.EncodeDiscoveryRoot(release.DiscoveryRoot{Format: 1, Epoch: input.Epoch, Head: release.AssetRef{Tag: input.Version, Name: release.UpdateMetadataAssetName, SHA256: sum(data)}, Previous: input.PreviousRoot})
	if err != nil {
		return fmt.Errorf("publisher: construct discovery root: %w", err)
	}
	retention, err := json.Marshal(history)
	if err != nil {
		return fmt.Errorf("publisher: encode retention: %w", err)
	}
	for name, contents := range map[string][]byte{release.UpdateMetadataAssetName: data, release.DiscoveryRootAssetName: root, "mesh-retention.json": retention} {
		if err := writePublisherFile(directory, name, contents); err != nil {
			return err
		}
	}
	return encodePublisherOutput(output, map[string]any{"oldestAnchor": selection.Anchors[selection.Direct.AnchorsFrom].Version, "oldestDate": selection.Oldest, "sourceCount": len(selection.Sources), "longestChain": longest})
}

func authenticateHistory(directory string, history []publishedEvidence) error {
	trust, err := release.LoadTrustRoot(os.Getenv("MESH_RELEASE_TRUST_CACHE"))
	if err != nil {
		return fmt.Errorf("publisher: load trust: %w", err)
	}
	for _, item := range history {
		if item.Metadata == nil {
			continue
		}
		if err := authenticatePublished(directory, item, trust); err != nil {
			return err
		}
	}
	return nil
}

func authenticatePublished(directory string, item publishedEvidence, trust release.TrustRoot) error {
	ref, exists := item.asset("mesh-update.sig.json")
	if !exists {
		return fmt.Errorf("publisher: %s has no signature envelope", item.Tag)
	}
	envelope, err := readAsset(directory, ref, release.MaxSignatureBytes)
	if err != nil {
		return err
	}
	page, err := readAsset(directory, item.Metadata.Page, release.MaxAnchorPageBytes)
	if err != nil {
		return err
	}
	if _, err := release.VerifyMetadata(trust, item.MetadataBytes, envelope, item.ManifestBytes, page); err != nil {
		return fmt.Errorf("publisher: authenticate %s: %w", item.Tag, err)
	}
	if _, err := release.DecodeAnchorPage(page, item.Metadata.Page); err != nil {
		return fmt.Errorf("publisher: authenticated page: %w", err)
	}
	return nil
}

func verifyPreviousRetention(directory string, history []publishedEvidence) error {
	for _, item := range slices.Backward(history) {
		ref, exists := item.asset("mesh-retention.json")
		if !exists {
			continue
		}
		data, err := readAsset(directory, ref, 16<<20)
		if err != nil {
			return err
		}
		if err := verifyRetainedSignature(directory, item, data); err != nil {
			return err
		}
		var retained []publishedEvidence
		if err := json.Unmarshal(data, &retained); err != nil {
			return fmt.Errorf("publisher: decode frozen retention: %w", err)
		}
		return checkRetained(retained, history)
	}
	return nil
}

func verifyRetainedSignature(directory string, item publishedEvidence, data []byte) error {
	ref, exists := item.asset("mesh-update.sig.json")
	if !exists {
		return errors.New("publisher: retained inventory lacks its signing envelope")
	}
	envelope, err := readAsset(directory, ref, release.MaxSignatureBytes)
	if err != nil {
		return err
	}
	trust, err := release.LoadTrustRoot(os.Getenv("MESH_RELEASE_TRUST_CACHE"))
	if err != nil {
		return fmt.Errorf("publisher: load inventory trust: %w", err)
	}
	if err := release.VerifySignature(trust, data, envelope); err != nil {
		return fmt.Errorf("publisher: authenticate frozen retention: %w", err)
	}
	return nil
}
